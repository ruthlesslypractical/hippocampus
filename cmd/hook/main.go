package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
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

	configPath := config.FindConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		os.Exit(0)
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}

	var event HookEvent
	if err := json.Unmarshal(input, &event); err != nil {
		os.Exit(0)
	}

	store, err := memory.NewLightStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		os.Exit(0)
	}
	client := store.Client()
	defer store.Close()

	timeoutS := cfg.Hook.TimeoutS
	if timeoutS == 0 {
		timeoutS = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
	defer cancel()

	switch event.HookEventName {
	case "userPromptSubmit":
		// Stash prompt for the sidecar (accumulates last 3 exchanges)
		if cfg.WorkingSet.Enabled {
			sessionID := os.Getenv("KIRO_SESSION_ID")
			if sessionID != "" {
				listKey := "meta:recent-prompts:" + sessionID
				client.LPush(ctx, listKey, event.Prompt)
				client.LTrim(ctx, listKey, 0, 2) // Keep last 3
				client.Expire(ctx, listKey, 30*time.Minute)
			}
		}
		handleRecall(ctx, client, event, cfg)
		storePrompt(ctx, store, event, cfg)
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
func handleRecall(ctx context.Context, client *redis.Client, event HookEvent, cfg config.Config) {
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

	// Weight and cap
	contextResults = weightedSort(contextResults, mem.DecayHalfLifeDays)
	contextResults = capToBudget(contextResults, mem.RecallMaxChars, mem.RecallMaxEntries)

	// Strategy 3: Follow associative links
	maxLinkHops := cfg.Hook.MaxLinkHops
	if maxLinkHops == 0 {
		maxLinkHops = 3
	}
	linkedResults := followLinks(ctx, client, contextResults, maxLinkHops)
	linkedResults = excludeIDs(linkedResults, orientationResults)
	linkedResults = excludeIDs(linkedResults, contextResults)
	linkedResults = excludeByTag(linkedResults, "content:full") // Also exclude from link-following

	// Combine: orientation first, then contextual, then linked
	allResults := append(orientationResults, contextResults...)
	allResults = append(allResults, linkedResults...)

	if len(allResults) == 0 {
		return
	}

	// Format output for injection
	var out strings.Builder
	out.WriteString("[MEMORY CONTEXT — retrieved from persistent memory]\n")
	for i, r := range allResults {
		out.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, strings.Join(r.Tags, ", "), r.Content))
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

	out.WriteString("[END MEMORY CONTEXT]\n")
	fmt.Print(out.String())
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
}

// storePrompt stores the user prompt for future reference.
func storePrompt(ctx context.Context, store *memory.RedisStore, event HookEvent, cfg config.Config) {
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

	entryID := fmt.Sprintf("prompt:%s:%d", sessionID, now.UnixNano())

	store.Put(ctx, memory.Entry{
		ID:        entryID,
		Content:   content,
		Tags:      tags,
		Timestamp: now,
	})
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

func followLinks(ctx context.Context, client *redis.Client, entries []memoryEntry, maxLinks int) []memoryEntry {
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
		results, err := client.ZRangeWithScores(ctx, "link:"+e.ID, 0, -1).Result()
		if err != nil {
			continue
		}
		for _, z := range results {
			targetID := z.Member.(string)
			if seen[targetID] {
				continue
			}
			seen[targetID] = true
			abs := z.Score
			if abs < 0 {
				abs = -abs
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
	Tags      []string
	Timestamp int64
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
		Tags:      tags,
		Timestamp: ts,
	}
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

func weightedSort(entries []memoryEntry, halfLifeDays float64) []memoryEntry {
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
		return
	}
	defer store.Close()
	client := store.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	ollamaClient := ollama.New(ollamaURL, model, 1)
	result, err := ollamaClient.Generate(ctx, prompt)
	if err != nil {
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
		client.ZAdd(ctx, "link:"+idA, redis.Z{Score: score, Member: idB})
		client.ZAdd(ctx, "link:"+idB, redis.Z{Score: score, Member: idA})
	}
}
