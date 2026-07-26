// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/logging"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
)

// HookEvent is the JSON payload received on stdin from the MCP client (e.g., Kiro CLI).
type HookEvent struct {
	HookEventName     string `json:"hook_event_name"`
	CWD               string `json:"cwd"`
	Prompt            string `json:"prompt,omitempty"`             // userPromptSubmit
	AssistantResponse string `json:"assistant_response,omitempty"` // stop
}

func main() {
	// Quick version check (hook is normally invoked via stdin, but support --version for diagnostics)
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("hippocampus-hook v%s\n", config.Version)
		os.Exit(0)
	}

	// Check for --ofc flag
	ofcFlagged := false
	for _, arg := range os.Args[1:] {
		if arg == "--ofc" {
			ofcFlagged = true
		}
	}

	configPath := config.FindConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		os.Exit(0)
	}

	// Guard against empty queue key
	if cfg.Daemon.QueueKey == "" {
		cfg.Daemon.QueueKey = "ingest:queue"
	}

	cleanupLog := logging.Setup(cfg, "hook")
	defer cleanupLog()

	// OFC enabled if CLI flag --ofc is set OR config has ofc.enabled = true
	ofcEnabled := ofcFlagged || cfg.OFC.Enabled

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Warn("failed to read stdin", "error", err)
		os.Exit(0)
	}

	var event HookEvent
	if err := json.Unmarshal(input, &event); err != nil {
		slog.Warn("failed to parse hook event", "error", err)
		os.Exit(0)
	}

	slog.Debug("hook starting", "config", configPath, "mode", event.HookEventName)

	store, err := memory.NewLightStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		slog.Error("redis connection failed", "addr", cfg.Redis.Addr, "error", err)
		os.Exit(0)
	}
	client := store.Client()
	defer store.Close()

	slog.Debug("redis connected", "addr", cfg.Redis.Addr)

	timeoutS := cfg.Hook.TimeoutS
	if timeoutS == 0 {
		timeoutS = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
	defer cancel()

	switch event.HookEventName {
	case "userPromptSubmit":
		// Generate deterministic prompt entry ID for this event
		sessionID := os.Getenv("KIRO_SESSION_ID")
		promptID := fmt.Sprintf("prompt:%s:%d", sessionID, time.Now().UnixNano())

		// Stash prompt for the sidecar (accumulates last 3 exchanges)
		if cfg.WorkingSet.Enabled {
			if sessionID != "" {
				listKey := "meta:recent-prompts:" + sessionID
				client.LPush(ctx, listKey, event.Prompt)
				client.LTrim(ctx, listKey, 0, 2) // Keep last 3
				client.Expire(ctx, listKey, 30*time.Minute)
			}
		}
		// OFC: update neuromodulator state based on user's prompt
		if ofcEnabled {
			ofcUpdate(ctx, client, event.Prompt, cfg.OFC, cfg.Ollama.BaseURL)
		}
		handleRecall(ctx, client, event, cfg, ofcEnabled, promptID)
		storePromptWithID(ctx, store, event, cfg, promptID)
	case "stop":
		handleStore(ctx, store, event, cfg)
		// Fire working set sidecar (runs on stop path — doesn't block user's next prompt)
		if cfg.WorkingSet.Enabled && event.AssistantResponse != "" {
			fireWorkingSetSidecar(cfg, event)
		}
	}
}

// handleRecall queries Redis for relevant memories and outputs them to stdout.
// The output gets injected into the agent's context.
func handleRecall(ctx context.Context, client *redis.Client, event HookEvent, cfg config.Config, ofcEnabled bool, promptID string) {
	mem := cfg.Memory
	if event.Prompt == "" {
		return
	}

	// Check for reload trigger
	promptLower := strings.ToLower(event.Prompt)
	if strings.Contains(promptLower, "reload context") || strings.Contains(promptLower, "full context") {
		bootPhaseTTLH := cfg.Hook.BootPhaseTTLH
		if bootPhaseTTLH == 0 {
			bootPhaseTTLH = 24
		}
		resetBootPhase(ctx, client, bootPhaseTTLH)
	}

	// Strategy 0: Always inject orientation entry
	bootPhaseTTLH := cfg.Hook.BootPhaseTTLH
	if bootPhaseTTLH == 0 {
		bootPhaseTTLH = 24
	}
	orientationResults := loadOrientation(ctx, client, bootPhaseTTLH)

	// Contextual recall (budgeted)
	var contextResults []memoryEntry

	keywords := extractKeywords(event.Prompt)

	// Strategy 1: Tag-based search
	if len(keywords) > 0 {
		tagResults := searchByTagOverlap(ctx, client, keywords, mem.RecallMaxEntries)
		contextResults = append(contextResults, tagResults...)
	}

	// Strategy 2: Full-text search
	ftResults := searchFullText(ctx, client, event.Prompt, mem.RecallMaxEntries)
	contextResults = append(contextResults, ftResults...)

	// Deduplicate and exclude orientation
	contextResults = dedupeResults(contextResults)
	contextResults = excludeIDs(contextResults, orientationResults)

	// Layer 4/5: Exclude full web content entries (they use on-demand loading only)
	contextResults = excludeByTag(contextResults, "content:full")

	// Detect current dominant track for weighting
	currentTrack := detectDominantTrack(contextResults)

	// Weight and cap (with track boost for same-track entries)
	contextResults = weightedSort(contextResults, mem.DecayHalfLifeDays, currentTrack)

	// Relevance floor: drop entries with negligible weight (prevents noise injection)
	relevanceFloor := cfg.Memory.RelevanceFloor
	if relevanceFloor == 0 {
		relevanceFloor = 0.05
	}
	contextResults = applyRelevanceFloor(contextResults, relevanceFloor)

	contextResults = capToBudget(contextResults, mem.RecallMaxChars, mem.RecallMaxEntries)

	// Strategy 3: Follow associative links
	maxLinkHops := cfg.Hook.MaxLinkHops
	if maxLinkHops == 0 {
		maxLinkHops = 3
	}
	linkedResults := followLinks(ctx, client, contextResults, maxLinkHops, cfg.Hook.MinLinkFollowScore)
	linkedResults = excludeIDs(linkedResults, orientationResults)
	linkedResults = excludeIDs(linkedResults, contextResults)
	linkedResults = excludeByTag(linkedResults, "content:full") // Also exclude from link-following
	// Links get their own budget (don't compete with search results)
	linkBudgetChars := cfg.Hook.LinkBudgetChars
	if linkBudgetChars <= 0 {
		linkBudgetChars = 3000
	}
	linkBudgetEntries := cfg.Hook.LinkBudgetEntries
	if linkBudgetEntries <= 0 {
		linkBudgetEntries = 3
	}
	linkedResults = capToBudget(linkedResults, linkBudgetChars, linkBudgetEntries)

	// Combine: orientation first, then contextual, then linked
	allResults := append(orientationResults, contextResults...)
	allResults = append(allResults, linkedResults...)

	if len(allResults) == 0 {
		return
	}

	// Epistemic warnings: check if any topic keywords match contested/false claims
	epistemicWarnings := getEpistemicWarnings(ctx, client, keywords)

	// Format output for injection — TIERED:
	// Tier 1 (top 1): full content
	// Tier 2 (next 2-4): summary or truncated content
	// Tier 3 (remaining): one-line breadcrumb + entry ID
	tier2MaxChars := cfg.Hook.Tier2MaxChars
	if tier2MaxChars <= 0 {
		tier2MaxChars = 300
	}
	tier3SnippetChars := cfg.Hook.Tier3SnippetChars
	if tier3SnippetChars <= 0 {
		tier3SnippetChars = 80
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("[MEMORY CONTEXT — retrieved from persistent memory (unix_now: %d)]\n", time.Now().Unix()))
	for i, r := range allResults {
		if i < len(orientationResults) {
			// Orientation entries always injected in full
			out.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, strings.Join(r.Tags, ", "), r.Content))
		} else {
			// Contextual + linked results: tiered injection
			rank := i - len(orientationResults)
			if rank == 0 {
				// Tier 1: full content (the single most relevant recall)
				out.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, strings.Join(r.Tags, ", "), r.Content))
			} else if rank < 5 {
				// Tier 2: summary/truncated (next few results)
				condensed := condensedContent(r, tier2MaxChars)
				out.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, strings.Join(r.Tags, ", "), condensed))
				if len(r.Content) > tier2MaxChars && r.Summary == "" {
					out.WriteString(fmt.Sprintf("   [full entry: %s]\n", r.ID))
				}
			} else {
				// Tier 3: breadcrumb only (ID + first N chars)
				snippet := r.Content
				if len(snippet) > tier3SnippetChars {
					snippet = snippet[:tier3SnippetChars] + "…"
				}
				out.WriteString(fmt.Sprintf("%d. [%s] %s [ref: %s]\n", i+1, strings.Join(r.Tags, ", "), snippet, r.ID))
			}
		}
	}

	// Inject epistemic warnings if any
	if len(epistemicWarnings) > 0 {
		out.WriteString("\n[EPISTEMIC WARNINGS — known contested/false claims relevant to this topic]\n")
		injected := epistemicWarnings
		if len(injected) > 5 {
			injected = injected[:5]
		}
		for _, w := range injected {
			out.WriteString(w + "\n")
		}
		if len(epistemicWarnings) > 5 {
			overflowID := "epistemic:overflow:" + promptID
			out.WriteString(fmt.Sprintf("... and %d more (ref: %s)\n", len(epistemicWarnings)-5, overflowID))

			// Store overflow as a proper entry linked to the triggering prompt
			var overflowContent strings.Builder
			overflowContent.WriteString(fmt.Sprintf("Epistemic overflow: %d warnings triggered by prompt %s\n\n", len(epistemicWarnings), promptID))
			for i, w := range epistemicWarnings {
				overflowContent.WriteString(fmt.Sprintf("%d. %s\n", i+1, w))
			}
			sessionID := os.Getenv("KIRO_SESSION_ID")
			overflowTags := []string{"epistemic", "overflow", "date:" + time.Now().Format("2006-01-02")}
			if sessionID != "" {
				overflowTags = append(overflowTags, "session:"+sessionID)
			}
			client.HSet(ctx, "entry:"+overflowID, map[string]interface{}{
				"id":        overflowID,
				"content":   overflowContent.String(),
				"tags":      strings.Join(overflowTags, ","),
				"timestamp": fmt.Sprintf("%d", time.Now().Unix()),
			})
			// Add to timeline
			client.ZAdd(ctx, "timeline", redis.Z{Score: float64(time.Now().Unix()), Member: overflowID})
			// Add to tag sets
			for _, tag := range overflowTags {
				client.SAdd(ctx, "tag:"+tag, overflowID)
				client.SAdd(ctx, "tags:all", tag)
			}
			// Link overflow entry to the prompt that triggered it
			client.HSet(ctx, "links:"+overflowID, promptID, "0.8000|overflow")
			client.HSet(ctx, "links:"+promptID, overflowID, "0.8000|overflow")
		}
		out.WriteString("[END EPISTEMIC WARNINGS]\n")
	}

	// Vibe Condenser: inject prior session's exchange rhythm on cold boot.
	// Only fires on first prompt of a new session (when no vibe for THIS session exists yet).
	sessionID := os.Getenv("KIRO_SESSION_ID")
	thisSessionVibeKey := "meta:vibe:session:" + sessionID
	hasSessionVibe, _ := client.Exists(ctx, thisSessionVibeKey).Result()
	if hasSessionVibe == 0 {
		// First prompt of this session — inject prior vibe and mark session as warm
		vibeItems, _ := client.LRange(ctx, "meta:vibe:latest", 0, 11).Result()
		if len(vibeItems) >= 2 {
			out.WriteString("\n[RELATIONAL CONTEXT — last exchanges from prior session]\n")
			for _, item := range vibeItems {
				out.WriteString(item + "\n")
			}
			out.WriteString("[END RELATIONAL CONTEXT]\n")
		}
		// Mark this session as having received its vibe primer (don't re-inject)
		client.Set(ctx, thisSessionVibeKey, "1", 24*time.Hour)
	}

	// Inject working set if available
	if ws := loadWorkingSet(ctx, client, cfg); ws != "" {
		out.WriteString("\n[WORKING SET — current session context]\n")
		out.WriteString(ws + "\n")
		out.WriteString("[END WORKING SET]\n")
	}

	coverage := getCoverageInfo(ctx, client)
	if coverage != "" {
		out.WriteString(coverage)
	}

	// Track shift detection: if the recalled context suggests a different track
	// than the last prompt, auto-inject that track's orientation entry.
	if sessionID != "" {
		lastTrackKey := "meta:last-track:" + sessionID
		lastTrack, _ := client.Get(ctx, lastTrackKey).Result()

		// Detect current track from recalled context tags
		currentTrack := detectDominantTrack(contextResults)
		if currentTrack != "" && lastTrack != "" && currentTrack != lastTrack {
			out.WriteString(fmt.Sprintf("\n[TRACK SHIFT: %s → %s]\n", lastTrack, currentTrack))

			// Auto-inject orientation for the new track
			orientID := "meta:orientation:track:" + currentTrack
			orientData, err := client.HGetAll(ctx, "entry:"+orientID).Result()
			if err == nil && len(orientData) > 0 && orientData["content"] != "" {
				out.WriteString(fmt.Sprintf("[TRACK ORIENTATION — %s]\n", currentTrack))
				out.WriteString(orientData["content"] + "\n")
				out.WriteString("[END TRACK ORIENTATION]\n")
				slog.Debug("injected track orientation on shift", "track", currentTrack, "id", orientID, "chars", len(orientData["content"]))
			} else {
				// No orientation entry exists — fall back to hint
				out.WriteString(fmt.Sprintf("[No orientation found for %s. Create one: hippocampus-admin orientation add %s]\n", currentTrack, orientID))
			}
		}

		// Update last track for next prompt
		if currentTrack != "" {
			client.Set(ctx, lastTrackKey, currentTrack, 24*time.Hour)
		}
	}

	out.WriteString("[END MEMORY CONTEXT]\n")

	// OFC: inject neuromodulator state
	if ofcEnabled {
		out.WriteString(ofcFormatBlock(ctx, client, cfg.OFC))
	}

	fmt.Print(out.String())

	// Daemon integration: record which entries were recalled for this prompt.
	// The daemon uses this for co-recall linking (entries recalled together get linked).
	if promptID != "" && len(contextResults) > 0 {
		recalledKey := "recalled:" + promptID
		recalledIDs := make([]interface{}, 0, len(contextResults))
		for _, r := range contextResults {
			recalledIDs = append(recalledIDs, r.ID)
		}
		client.SAdd(ctx, recalledKey, recalledIDs...)
		client.Expire(ctx, recalledKey, time.Duration(cfg.Daemon.RecalledTTLH)*time.Hour)
		slog.Debug("recorded recalled entries", "prompt_id", promptID, "count", len(contextResults))
	}
}

// handleStore captures the assistant's response.
func handleStore(ctx context.Context, store *memory.RedisStore, event HookEvent, cfg config.Config) {
	mem := cfg.Memory
	if event.AssistantResponse == "" {
		return
	}
	if len(event.AssistantResponse) < mem.StoreMinResponseLen {
		return
	}

	sessionID := os.Getenv("KIRO_SESSION_ID")

	tags := []string{"kind:assistant_response", "auto:captured"}
	if cfg.Author != "" {
		tags = append(tags, "author:"+cfg.Author)
	}
	if sessionID != "" {
		tags = append(tags, "session:"+sessionID)
	}
	if event.CWD != "" {
		tags = append(tags, "cwd:"+event.CWD)
	}

	now := time.Now()
	tags = append(tags, "date:"+now.Format("2006-01-02"))

	entryID := fmt.Sprintf("auto:%s:%d", sessionID, now.UnixNano())

	content := event.AssistantResponse
	if mem.StoreMaxChars > 0 && len(content) > mem.StoreMaxChars {
		content = content[:mem.StoreMaxChars] + "..."
	}

	store.Put(ctx, memory.Entry{
		ID:        entryID,
		Content:   content,
		Tags:      tags,
		Timestamp: now,
	})

	slog.Info("stored entry", "id", entryID, "kind", "assistant_response")

	// Daemon integration: push entry to async processing queue
	client := store.Client()
	client.LPush(ctx, cfg.Daemon.QueueKey, entryID)

	slog.Debug("queued for daemon", "entry_id", entryID, "queue", cfg.Daemon.QueueKey)

	// Vibe Condenser: update the rolling exchange buffer for next-session priming.
	// Stores last 6 exchanges (3 pairs) as a cross-session relational primer.
	// The recall hook injects this on cold boot to calibrate tone/rhythm.
	vibeKey := "meta:vibe:latest"

	// Get the most recent user prompt from this session (stored moments before us)
	recentPrompts, _ := client.LRange(ctx, "meta:recent-prompts:"+sessionID, 0, 0).Result()
	userText := ""
	if len(recentPrompts) > 0 {
		userText = recentPrompts[0]
	}

	// Truncate for vibe purposes (we only need the shape, not the full content)
	vibeTruncateChars := cfg.Hook.VibeTruncateChars
	if vibeTruncateChars <= 0 {
		vibeTruncateChars = 200
	}
	if len(userText) > vibeTruncateChars {
		userText = userText[:vibeTruncateChars]
	}
	assistantText := event.AssistantResponse
	if len(assistantText) > vibeTruncateChars {
		assistantText = assistantText[:vibeTruncateChars]
	}

	// Push both sides as a pair, newest first
	client.LPush(ctx, vibeKey, "A: "+assistantText)
	client.LPush(ctx, vibeKey, "U: "+userText)
	vibeMaxExchanges := cfg.Hook.VibeMaxExchanges
	if vibeMaxExchanges <= 0 {
		vibeMaxExchanges = 6
	}
	client.LTrim(ctx, vibeKey, 0, int64(vibeMaxExchanges*2-1)) // Keep last N exchanges (2 items each)
}

// storePrompt stores the user prompt for future reference.
func storePrompt(ctx context.Context, store *memory.RedisStore, event HookEvent, cfg config.Config) {
	storePromptWithID(ctx, store, event, cfg, "")
}

// storePromptWithID stores the user prompt using a pre-generated entry ID.
// If promptID is empty, generates one automatically.
func storePromptWithID(ctx context.Context, store *memory.RedisStore, event HookEvent, cfg config.Config, promptID string) {
	mem := cfg.Memory
	if event.Prompt == "" || len(event.Prompt) < mem.StoreMinPromptLen {
		return
	}

	sessionID := os.Getenv("KIRO_SESSION_ID")

	tags := []string{"kind:user_prompt", "auto:captured"}
	if cfg.Author != "" {
		tags = append(tags, "author:"+cfg.Author)
	}
	if sessionID != "" {
		tags = append(tags, "session:"+sessionID)
	}
	if event.CWD != "" {
		tags = append(tags, "cwd:"+event.CWD)
	}

	now := time.Now()
	tags = append(tags, "date:"+now.Format("2006-01-02"))

	content := event.Prompt
	if mem.StoreMaxChars > 0 && len(content) > mem.StoreMaxChars {
		content = content[:mem.StoreMaxChars] + "..."
	}

	entryID := promptID
	if entryID == "" {
		entryID = fmt.Sprintf("prompt:%s:%d", sessionID, now.UnixNano())
	}

	store.Put(ctx, memory.Entry{
		ID:        entryID,
		Content:   content,
		Tags:      tags,
		Timestamp: now,
	})

	slog.Info("stored entry", "id", entryID, "kind", "user_prompt")

	// Daemon integration: push entry to async processing queue
	store.Client().LPush(ctx, cfg.Daemon.QueueKey, entryID)

	slog.Debug("queued for daemon", "entry_id", entryID, "queue", cfg.Daemon.QueueKey)
}

// --- Orientation loading ---

// loadOrientation fetches the orientation entry to inject.
// Priority: 1) custom file at ~/.config/hippocampus/prompt.txt, 2) Redis entry.
// First prompt in a session gets full orientation; subsequent prompts get the lean kernel.
func loadOrientation(ctx context.Context, client *redis.Client, bootPhaseTTLH int) []memoryEntry {
	sessionID := os.Getenv("KIRO_SESSION_ID")
	bootKey := "session:" + sessionID + ":boot-phase"

	phase, _ := client.Get(ctx, bootKey).Result()

	if phase == "" || phase == "full" {
		// First prompt or reset → inject full orientation, switch to lean
		client.Set(ctx, bootKey, "lean", time.Duration(bootPhaseTTLH)*time.Hour)
		return loadFullOrientation(ctx, client)
	}

	// Subsequent prompts → lean kernel only
	return loadLeanKernel(ctx, client)
}

func loadFullOrientation(ctx context.Context, client *redis.Client) []memoryEntry {
	// Check for custom prompt file first
	if content := loadPromptFile("prompt.txt"); content != "" {
		return []memoryEntry{{ID: "local:prompt", Content: content, Tags: []string{"meta", "orientation"}}}
	}

	// Find entries tagged both "orientation" and "meta"
	ids, err := client.SInter(ctx, "tag:orientation", "tag:meta").Result()
	if err != nil || len(ids) == 0 {
		return nil
	}

	// Find the one tagged summary:comprehensive (the full orientation)
	var best memoryEntry
	var bestTimestamp int64
	for _, id := range ids {
		data, err := client.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		tags := data["tags"]
		if !strings.Contains(tags, "summary:comprehensive") {
			continue
		}
		entry := parseEntry(data)
		var ts int64
		fmt.Sscanf(data["timestamp"], "%d", &ts)
		if ts > bestTimestamp {
			bestTimestamp = ts
			best = entry
		}
	}

	if best.ID == "" {
		return nil
	}
	return []memoryEntry{best}
}

func loadLeanKernel(ctx context.Context, client *redis.Client) []memoryEntry {
	// Check for custom lean kernel file first
	if content := loadPromptFile("prompt-lean.txt"); content != "" {
		return []memoryEntry{{ID: "local:prompt-lean", Content: content, Tags: []string{"meta", "orientation", "lean-kernel"}}}
	}

	data, err := client.HGetAll(ctx, "entry:entry:meta:lean-kernel").Result()
	if err != nil || len(data) == 0 {
		return loadFullOrientation(ctx, client)
	}
	entry := parseEntry(data)
	if entry.ID == "" {
		return loadFullOrientation(ctx, client)
	}
	return []memoryEntry{entry}
}

// loadPromptFile tries to read a custom prompt from ~/.config/hippocampus/<filename>
func loadPromptFile(filename string) string {
	homeDir, _ := os.UserHomeDir()
	candidates := []string{
		homeDir + "/.config/hippocampus/" + filename,
		"/etc/hippocampus/" + filename,
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func resetBootPhase(ctx context.Context, client *redis.Client, bootPhaseTTLH int) {
	sessionID := os.Getenv("KIRO_SESSION_ID")
	if sessionID != "" {
		client.Set(ctx, "session:"+sessionID+":boot-phase", "full", time.Duration(bootPhaseTTLH)*time.Hour)
	}
}

// --- Search helpers ---

func followLinks(ctx context.Context, client *redis.Client, entries []memoryEntry, maxLinks int, minLinkFollowScore float64) []memoryEntry {
	if len(entries) == 0 {
		return nil
	}

	type scoredLink struct {
		id       string
		absScore float64
	}

	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.ID] = true
	}

	var candidates []scoredLink
	for _, e := range entries {
		// Read from unified HASH format: links:<id> with field=targetID, value="score|type"
		results, err := client.HGetAll(ctx, "links:"+e.ID).Result()
		if err != nil || len(results) == 0 {
			continue
		}
		for targetID, value := range results {
			if seen[targetID] {
				continue
			}
			seen[targetID] = true

			// Parse "score|type" value
			score := 0.0
			if parts := strings.SplitN(value, "|", 2); len(parts) >= 1 {
				fmt.Sscanf(parts[0], "%f", &score)
			}
			abs := score
			if abs < 0 {
				abs = -abs
			}
			// Only follow links with meaningful scores (skip unscored 0.0 links)
			minFollow := minLinkFollowScore
			if minFollow == 0 {
				minFollow = 0.3
			}
			if abs < minFollow {
				continue
			}
			candidates = append(candidates, scoredLink{id: targetID, absScore: abs})
		}
	}

	// Sort by |score| descending
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].absScore > candidates[j-1].absScore; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	if len(candidates) > maxLinks {
		candidates = candidates[:maxLinks]
	}

	var linked []memoryEntry
	for _, c := range candidates {
		data, err := client.HGetAll(ctx, "entry:"+c.id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		linked = append(linked, parseEntry(data))
	}
	return linked
}

func searchByTagOverlap(ctx context.Context, client *redis.Client, keywords []string, maxEntries int) []memoryEntry {
	var results []memoryEntry

	allTags, err := client.SMembers(ctx, "tags:all").Result()
	if err != nil {
		return nil
	}

	tagSet := make(map[string]bool)
	for _, tag := range allTags {
		tagSet[tag] = true
	}

	var matchedTags []string
	for _, kw := range keywords {
		kwLower := strings.ToLower(kw)
		if tagSet[kwLower] {
			matchedTags = append(matchedTags, kwLower)
		}
		if tagSet["track:"+kwLower] {
			matchedTags = append(matchedTags, "track:"+kwLower)
		}
	}

	if len(matchedTags) == 0 {
		return nil
	}

	tagKeys := make([]string, len(matchedTags))
	for i, t := range matchedTags {
		tagKeys[i] = "tag:" + t
	}

	ids, err := client.SUnion(ctx, tagKeys...).Result()
	if err != nil || len(ids) == 0 {
		return nil
	}

	for _, id := range ids {
		if len(results) >= maxEntries {
			break
		}
		data, err := client.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		entry := parseEntry(data)
		if hasSummaryTag(entry.Tags) {
			results = append([]memoryEntry{entry}, results...)
		} else {
			results = append(results, entry)
		}
	}

	return results
}

func searchFullText(ctx context.Context, client *redis.Client, query string, maxEntries int) []memoryEntry {
	var results []memoryEntry

	res, err := client.Do(ctx, "FT.SEARCH", "idx:entries", query, "LIMIT", "0", fmt.Sprintf("%d", maxEntries)).Result()
	if err != nil {
		return searchNaiveRecent(ctx, client, query, maxEntries)
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return nil
	}

	for i := 1; i+1 < len(arr); i += 2 {
		fields, ok := arr[i+1].([]interface{})
		if !ok {
			continue
		}
		data := make(map[string]string)
		for j := 0; j+1 < len(fields); j += 2 {
			k, _ := fields[j].(string)
			v, _ := fields[j+1].(string)
			data[k] = v
		}
		results = append(results, parseEntry(data))
	}

	return results
}

func searchNaiveRecent(ctx context.Context, client *redis.Client, query string, maxEntries int) []memoryEntry {
	var results []memoryEntry
	queryLower := strings.ToLower(query)

	ids, err := client.ZRevRange(ctx, "timeline", 0, int64(maxEntries)*10).Result()
	if err != nil {
		return nil
	}

	for _, id := range ids {
		if len(results) >= maxEntries {
			break
		}
		data, err := client.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		if strings.Contains(strings.ToLower(data["content"]), queryLower) {
			results = append(results, parseEntry(data))
		}
	}

	return results
}

// --- Coverage info ---

func getCoverageInfo(ctx context.Context, client *redis.Client) string {
	summaryIDs, err := client.SMembers(ctx, "tag:summary:comprehensive").Result()
	if err != nil || len(summaryIDs) == 0 {
		return ""
	}

	var lastSummaryTime int64
	for _, id := range summaryIDs {
		ts, err := client.HGet(ctx, "entry:"+id, "timestamp").Int64()
		if err != nil {
			continue
		}
		if ts > lastSummaryTime {
			lastSummaryTime = ts
		}
	}

	if lastSummaryTime == 0 {
		return ""
	}

	now := time.Now().Unix()
	count, err := client.ZCount(ctx, "timeline",
		fmt.Sprintf("%d", lastSummaryTime),
		fmt.Sprintf("%d", now)).Result()
	if err != nil {
		return ""
	}

	hoursSince := (now - lastSummaryTime) / 3600
	if count == 0 && hoursSince < 1 {
		return ""
	}

	return fmt.Sprintf("[Coverage: %d entries since last summary, last summarized %dh ago]\n", count, hoursSince)
}

// --- Helpers ---

type memoryEntry struct {
	ID        string
	Content   string
	Summary   string  // condensed version for tiered injection
	Tags      []string
	Timestamp int64
	Score     float64 // search/relevance score (0-1, higher = more relevant)
}

func parseEntry(data map[string]string) memoryEntry {
	var tags []string
	if t := data["tags"]; t != "" {
		tags = strings.Split(t, ",")
	}
	var ts int64
	fmt.Sscanf(data["timestamp"], "%d", &ts)
	return memoryEntry{
		ID:        data["id"],
		Content:   data["content"],
		Summary:   data["summary"], // empty string if not yet summarized
		Tags:      tags,
		Timestamp: ts,
	}
}

// detectDominantTrack examines recalled context results and returns the most frequent track tag.
func detectDominantTrack(results []memoryEntry) string {
	counts := make(map[string]int)
	for _, r := range results {
		for _, tag := range r.Tags {
			if strings.HasPrefix(tag, "track:") && !strings.HasPrefix(tag, "track_auto:") {
				counts[strings.TrimPrefix(tag, "track:")]++
			} else if strings.HasPrefix(tag, "track_auto:") {
				counts[strings.TrimPrefix(tag, "track_auto:")]++
			}
		}
	}
	if len(counts) == 0 {
		return ""
	}
	var best string
	var bestCount int
	for track, count := range counts {
		if count > bestCount {
			best = track
			bestCount = count
		}
	}
	return best
}

func extractKeywords(prompt string) []string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "shall": true, "can": true, "need": true,
		"to": true, "of": true, "in": true, "for": true, "on": true,
		"with": true, "at": true, "by": true, "from": true, "about": true,
		"that": true, "this": true, "it": true, "its": true, "what": true,
		"which": true, "who": true, "whom": true, "how": true, "when": true,
		"where": true, "why": true, "if": true, "then": true, "else": true,
		"and": true, "or": true, "not": true, "no": true, "but": true,
		"so": true, "than": true, "too": true, "very": true, "just": true,
		"i": true, "me": true, "my": true, "we": true, "our": true,
		"you": true, "your": true, "he": true, "she": true, "they": true,
		"them": true, "their": true, "let": true, "let's": true,
		"tell": true, "show": true, "give": true, "get": true, "make": true,
	}

	words := strings.Fields(prompt)
	var keywords []string
	seen := make(map[string]bool)

	for _, w := range words {
		w = strings.ToLower(strings.Trim(w, ".,;:!?\"'()[]{}"))
		if len(w) < 3 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		keywords = append(keywords, w)
	}

	return keywords
}

func hasSummaryTag(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, "summary:") {
			return true
		}
	}
	return false
}

func dedupeResults(entries []memoryEntry) []memoryEntry {
	seen := make(map[string]bool)
	var out []memoryEntry
	for _, e := range entries {
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

func excludeIDs(entries []memoryEntry, exclude []memoryEntry) []memoryEntry {
	excludeSet := make(map[string]bool)
	for _, e := range exclude {
		excludeSet[e.ID] = true
	}
	var out []memoryEntry
	for _, e := range entries {
		if !excludeSet[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// excludeByTag removes entries that have the specified tag.
// Used to prevent full web content entries from being auto-injected (Layer 5 security).
func excludeByTag(entries []memoryEntry, tag string) []memoryEntry {
	var out []memoryEntry
	for _, e := range entries {
		hasTag := false
		for _, t := range e.Tags {
			if t == tag {
				hasTag = true
				break
			}
		}
		if !hasTag {
			out = append(out, e)
		}
	}
	return out
}

func capToBudget(entries []memoryEntry, maxChars int, maxEntries int) []memoryEntry {
	var out []memoryEntry
	totalChars := 0

	for _, e := range entries {
		if len(out) >= maxEntries {
			break
		}
		entryLen := len(e.Content) + len(strings.Join(e.Tags, ", ")) + 20
		if totalChars+entryLen > maxChars {
			break
		}
		totalChars += entryLen
		out = append(out, e)
	}

	return out
}

func weightedSort(entries []memoryEntry, halfLifeDays float64, currentTrack string) []memoryEntry {
	if len(entries) <= 1 {
		return entries
	}

	type weighted struct {
		entry  memoryEntry
		weight float64
	}

	now := time.Now().Unix()
	var w []weighted
	for _, e := range entries {
		weight := entryWeight(e)
		// Apply confidence decay: weight *= 0.5^(days_since_update / half_life)
		if halfLifeDays > 0 && e.Timestamp > 0 {
			daysSince := float64(now-e.Timestamp) / 86400.0
			decay := math.Pow(0.5, daysSince/halfLifeDays)
			weight *= decay
		}
		// Track boost: same-track entries get 2× weight
		if currentTrack != "" && entryHasTrack(e, currentTrack) {
			weight *= 2.0
		}
		e.Score = weight // store computed score for tiered injection
		w = append(w, weighted{entry: e, weight: weight})
	}

	for i := 1; i < len(w); i++ {
		for j := i; j > 0 && w[j].weight > w[j-1].weight; j-- {
			w[j], w[j-1] = w[j-1], w[j]
		}
	}

	out := make([]memoryEntry, len(w))
	for i, we := range w {
		out[i] = we.entry
	}
	return out
}

// entryHasTrack checks if an entry belongs to a given track (explicit or auto).
func entryHasTrack(e memoryEntry, track string) bool {
	for _, t := range e.Tags {
		if t == "track:"+track || t == "track_auto:"+track {
			return true
		}
	}
	return false
}

// applyRelevanceFloor removes entries below a minimum weight threshold.
// If nothing survives the floor, returns empty — null recall is acceptable.
func applyRelevanceFloor(entries []memoryEntry, minWeight float64) []memoryEntry {
	var out []memoryEntry
	for _, e := range entries {
		if e.Score >= minWeight {
			out = append(out, e)
		}
	}
	return out
}

// condensedContent returns the summary if available, otherwise truncates content.
func condensedContent(e memoryEntry, maxChars int) string {
	if e.Summary != "" {
		return e.Summary
	}
	if len(e.Content) <= maxChars {
		return e.Content
	}
	return e.Content[:maxChars] + "…"
}

func entryWeight(e memoryEntry) float64 {
	for _, t := range e.Tags {
		if t == "summary:comprehensive" || strings.HasPrefix(t, "summary:track:") {
			return 0.9
		}
		if strings.HasPrefix(t, "summary:") {
			return 0.8
		}
	}
	for _, t := range e.Tags {
		if t == "kind:user_prompt" {
			return 0.3
		}
		if t == "kind:assistant_response" || t == "kind:assistantmessage" {
			return 0.5
		}
		if t == "auto:captured" {
			return 0.4
		}
	}
	return 1.0
}



// --- Working Set Tracker ---

// loadWorkingSet retrieves the working set for this session (or inherits from a recent one).
func loadWorkingSet(ctx context.Context, client *redis.Client, cfg config.Config) string {
	if !cfg.WorkingSet.Enabled {
		return ""
	}

	sessionID := os.Getenv("KIRO_SESSION_ID")
	if sessionID == "" {
		return ""
	}

	// Try this session's working set first (plain string — sidecar path)
	key := "meta:working-set:" + sessionID
	ws, err := client.Get(ctx, key).Result()
	if err == nil && ws != "" {
		return ws
	}

	// Also check if it was stored via MCP memory_store (entry hash)
	entryKey := "entry:" + key
	entryContent, err := client.HGet(ctx, entryKey, "content").Result()
	if err == nil && entryContent != "" {
		return entryContent
	}

	// No working set for this session — try to inherit from most recent
	inheritTTL := cfg.WorkingSet.InheritTTLH
	if inheritTTL == 0 {
		inheritTTL = 24
	}

	// Scan for any working set keys created recently
	iter := client.Scan(ctx, 0, "meta:working-set:*", 50).Iterator()
	var bestKey string
	var bestTime int64
	for iter.Next(ctx) {
		k := iter.Val()
		if k == key {
			continue // skip our own (empty) key
		}
		// Check how recently this key was accessed
		idle, err := client.ObjectIdleTime(ctx, k).Result()
		if err != nil {
			continue
		}
		age := int64(idle.Seconds())
		maxAge := int64(inheritTTL) * 3600
		if age < maxAge && age < bestTime || bestTime == 0 {
			bestTime = age
			bestKey = k
		}
	}

	if bestKey != "" {
		inherited, err := client.Get(ctx, bestKey).Result()
		if err == nil && inherited != "" {
			// Seed this session's working set from the inherited one
			client.Set(ctx, key, inherited, 0)
			return inherited
		}
	}

	return ""
}

// fireWorkingSetSidecar calls the local Ollama sidecar model to update the working set.
// Runs async — if it's slow, the next prompt just gets a slightly stale working set.
func fireWorkingSetSidecar(cfg config.Config, event HookEvent) {
	model := cfg.WorkingSet.Model
	if model == "" {
		model = "qwen3:1.7b"
	}

	sessionID := os.Getenv("KIRO_SESSION_ID")
	if sessionID == "" {
		return
	}

	// Connect to Redis to get context
	store, err := memory.NewLightStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		slog.Warn("working-set sidecar: redis connect failed", "error", err)
		return
	}
	defer store.Close()
	client := store.Client()

	timeoutS := cfg.WorkingSet.TimeoutS
	if timeoutS <= 0 {
		timeoutS = 120
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
	defer cancel()

	// Get recent prompts from this session (last 3)
	recentPrompts, _ := client.LRange(ctx, "meta:recent-prompts:"+sessionID, 0, 2).Result()
	var recentContext strings.Builder
	for i, p := range recentPrompts {
		if len(p) > 300 {
			p = p[:300] + "..."
		}
		recentContext.WriteString(fmt.Sprintf("User (%d turns ago): %s\n", len(recentPrompts)-i, p))
	}

	// Get current working set
	currentWS, _ := client.Get(ctx, "meta:working-set:"+sessionID).Result()
	if currentWS == "" {
		// Fall back to entry hash
		currentWS, _ = client.HGet(ctx, "entry:meta:working-set:"+sessionID, "content").Result()
	}

	maxBullets := cfg.WorkingSet.MaxBullets
	if maxBullets == 0 {
		maxBullets = 5
	}

	// Truncate response for the sidecar (don't overwhelm a small model)
	response := event.AssistantResponse
	if len(response) > 2000 {
		response = response[:2000] + "..."
	}

	// Build sidecar prompt
	prompt := fmt.Sprintf(`You are a working-set tracker. Your job is to maintain a concise summary of what's being worked on right now, and suggest memory links.

Current working set:
%s

Recent conversation context:
%s
Latest assistant response: %s

Update the working set and suggest links. Rules:
- Maximum %d bullet points
- Each bullet: one-line description of an active topic/task
- Remove items that are clearly finished or no longer relevant
- Add new items that emerged in this exchange
- Keep entry IDs or commit hashes if mentioned
- If nothing changed, return the working set as-is

After the bullet list, if any memory entry IDs were mentioned in the exchange, output a LINKS section:
LINKS:
<entry-id-a> -> <entry-id-b> score:<0.0-1.0> reason:<one word>

Output the updated bullet list first, then LINKS if applicable. If no links, omit the LINKS section.`, currentWS, recentContext.String(), response, maxBullets)

	// Call Ollama
	ollamaURL := cfg.Ollama.BaseURL
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	// Use configured timeout (in seconds, convert to minutes for client; minimum 2 min)
	ollamaTimeoutMin := (timeoutS + 59) / 60
	if ollamaTimeoutMin < 2 {
		ollamaTimeoutMin = 2
	}
	ollamaClient := ollama.New(ollamaURL, model, ollamaTimeoutMin)
	result, err := ollamaClient.Generate(ctx, prompt)
	if err != nil {
		slog.Warn("working-set sidecar: ollama generate failed", "model", model, "error", err)
		return
	}

	// Split result into working set and links
	parts := strings.SplitN(result, "LINKS:", 2)
	wsContent := strings.TrimSpace(parts[0])

	// Cap the working set content
	maxChars := cfg.WorkingSet.MaxChars
	if maxChars == 0 {
		maxChars = 500
	}
	if len(wsContent) > maxChars {
		wsContent = wsContent[:maxChars]
	}

	// Store updated working set
	client.Set(ctx, "meta:working-set:"+sessionID, wsContent, 0)

	// Process link suggestions if any
	if len(parts) > 1 {
		createLinksFromSidecar(ctx, client, strings.TrimSpace(parts[1]))
	}
}

// createLinksFromSidecar parses link suggestions from the sidecar output and creates them.
// Format: "<id-a> -> <id-b> score:<float> reason:<word>"
func createLinksFromSidecar(ctx context.Context, client *redis.Client, linksText string) {
	for _, line := range strings.Split(linksText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "->") {
			continue
		}
		// Parse: "entry-a -> entry-b score:0.7 reason:extends"
		parts := strings.SplitN(line, "->", 2)
		if len(parts) != 2 {
			continue
		}
		idA := strings.TrimSpace(parts[0])
		rest := strings.TrimSpace(parts[1])

		// Extract id-b and score
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		idB := fields[0]
		score := 0.7 // default if parsing fails
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "score:") {
				fmt.Sscanf(strings.TrimPrefix(f, "score:"), "%f", &score)
			}
		}

		// Verify both entries exist before linking
		if client.Exists(ctx, "entry:"+idA).Val() == 0 {
			continue
		}
		if client.Exists(ctx, "entry:"+idB).Val() == 0 {
			continue
		}

		// Create bidirectional link
		value := fmt.Sprintf("%.4f|%s", score, "corecall")
		client.HSet(ctx, "links:"+idA, idB, value)
		client.HSet(ctx, "links:"+idB, idA, value)
	}
}

// getEpistemicWarnings checks if any of the prompt keywords match subjects or objects
// in the epistemic registry with status "false" or "contested".
// Returns formatted warning strings for injection, or nil if none match.
func getEpistemicWarnings(ctx context.Context, client *redis.Client, keywords []string) []string {
	if len(keywords) == 0 {
		return nil
	}

	// Track how many keywords match each canonical
	matchCount := make(map[string]int)
	var allCanonicals []string

	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if len(kw) < 4 {
			continue
		}

		// Check by subject
		canonicals, _ := client.SMembers(ctx, "epistemic:by_subject:"+kw).Result()
		for _, canonical := range canonicals {
			if matchCount[canonical] == 0 {
				allCanonicals = append(allCanonicals, canonical)
			}
			matchCount[canonical]++
		}

		// Check by object
		canonicals, _ = client.SMembers(ctx, "epistemic:by_object:"+kw).Result()
		for _, canonical := range canonicals {
			if matchCount[canonical] == 0 {
				allCanonicals = append(allCanonicals, canonical)
			}
			matchCount[canonical]++
		}
	}

	// Filter: require >= 2 keyword matches (topic gating)
	var warnings []string
	seen := make(map[string]bool)

	for _, canonical := range allCanonicals {
		if seen[canonical] || matchCount[canonical] < 2 {
			continue
		}
		seen[canonical] = true

		warning := checkAndFormat(ctx, client, canonical)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}

	// Cap at 5 warnings to avoid bloating context
	if len(warnings) > 5 {
		warnings = warnings[:5]
	}

	return warnings
}

// checkAndFormat reads an epistemic entry and returns a formatted warning if
// its status is "false" or "contested". Returns "" for verified/unknown entries.
// Also filters by confidence threshold (>= 0.70) to avoid wishy-washy warnings.
func checkAndFormat(ctx context.Context, client *redis.Client, canonical string) string {
	key := "epistemic:" + canonical
	vals, err := client.HGetAll(ctx, key).Result()
	if err != nil || len(vals) == 0 {
		return ""
	}

	status := vals["status"]
	if status != "false" && status != "contested" {
		return ""
	}

	// Confidence threshold: skip wishy-washy contested entries
	confidence := 0.0
	if c, err := strconv.ParseFloat(vals["confidence"], 64); err == nil {
		confidence = c
	}
	if confidence < 0.70 {
		return ""
	}

	subject := vals["subject"]
	verb := vals["verb"]
	object := vals["object"]
	evidenceFor := vals["evidence_for"]
	evidenceAgainst := vals["evidence_against"]
	encounters := vals["encounter_count"]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚠️ \"%s|%s|%s\" — %s (seen %s times)",
		subject, verb, object, strings.ToUpper(status), encounters))
	if evidenceFor != "" {
		if len(evidenceFor) > 80 {
			evidenceFor = evidenceFor[:80] + "..."
		}
		sb.WriteString(fmt.Sprintf("\n   For: %s", evidenceFor))
	}
	if evidenceAgainst != "" {
		if len(evidenceAgainst) > 80 {
			evidenceAgainst = evidenceAgainst[:80] + "..."
		}
		sb.WriteString(fmt.Sprintf("\n   Against: %s", evidenceAgainst))
	}

	return sb.String()
}
