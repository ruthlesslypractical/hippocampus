package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/pkg/consolidate"
)

const (
	entryPrefix = "entry:"
	tagPrefix   = "tag:"
	allTagsKey  = "tags:all"
	timelineKey = "timeline"
)

type entry struct {
	ID        string
	Content   string
	Tags      []string
	Timestamp int64
}

func main() {
	log.SetFlags(log.Ltime)

	configPath := config.FindConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	mode := "daily"
	track := ""
	dryRun := false

	for i, arg := range os.Args[1:] {
		switch arg {
		case "--daily":
			mode = "daily"
		case "--3h":
			mode = "3h"
		case "--weekly":
			mode = "weekly"
		case "--cross-track":
			mode = "cross-track"
		case "--consolidate":
			mode = "consolidate"
		case "--track":
			if i+1 < len(os.Args[1:]) {
				track = os.Args[i+2]
			}
		case "--all":
			mode = "all"
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			fmt.Printf("hippocampus-summarize v%s\n\n", config.Version)
			fmt.Println("Usage: hippocampus-summarize [--daily|--3h|--weekly|--cross-track|--consolidate|--all] [--track Name] [--dry-run]")
			fmt.Println("  --daily        Summarize today's entries (default)")
			fmt.Println("  --3h           Summarize the last 3 hours")
			fmt.Println("  --weekly       Roll up daily summaries into weekly")
			fmt.Println("  --cross-track  Detect cross-track themes and create links")
			fmt.Println("  --consolidate  Run continuous pairwise relevance discovery (background link building)")
			fmt.Println("  --all          Regenerate all track summaries")
			fmt.Println("  --track X      Only summarize track X")
			fmt.Println("  --dry-run      Print what would be done without acting")
			os.Exit(0)
		}
	}

	store, err := memory.NewLightStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Fatalf("connecting to Valkey: %v", err)
	}
	client := store.Client()
	defer store.Close()

	ctxTimeout := time.Duration(cfg.Ollama.TimeoutMinutes) * time.Minute
	if ctxTimeout == 0 {
		ctxTimeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()

	ollamaClient := ollama.New(cfg.Ollama.BaseURL, cfg.Ollama.Model, cfg.Ollama.TimeoutMinutes)

	if mode == "cross-track" {
		runCrossTrackReview(ctx, client, store, ollamaClient, cfg.Memory.CrossTrackMaxChars, dryRun)
		return
	}

	if mode == "weekly" {
		runWeeklyRollup(ctx, client, store, ollamaClient, track, dryRun)
		return
	}

	if mode == "consolidate" {
		// Consolidate mode runs continuously (until killed)
		consolidateCfg := consolidate.Config{
			RedisAddr:         cfg.Redis.Addr,
			RedisPassword:     cfg.Redis.Password,
			OllamaURL:         cfg.Ollama.BaseURL,
			OllamaModel:       cfg.Ollama.Model,
			PairsPerRun:       cfg.Consolidation.PairsPerRun,
			MinScore:          cfg.Consolidation.MinScore,
			CyclePause:        time.Duration(cfg.Consolidation.CyclePauseS) * time.Second,
			MaxEntries:        cfg.Consolidation.MaxEntries,
			DriftDelta:        cfg.Consolidation.DriftDelta,
			ContentTruncation: cfg.Consolidation.ContentTruncation,
			MinContentLength:  cfg.Consolidation.MinContentLength,
			Temperature:       cfg.Consolidation.Temperature,
			MaxTokens:         cfg.Consolidation.MaxTokens,
			EvalTimeoutS:      cfg.Consolidation.EvalTimeoutS,
			DryRun:            dryRun,
		}
		// Use a long-lived context for continuous mode
		consolidateCtx, consolidateCancel := context.WithCancel(context.Background())
		defer consolidateCancel()

		// Handle interrupt
		go func() {
			<-ctx.Done()
			consolidateCancel()
		}()

		consolidate.RunContinuous(consolidateCtx, consolidateCfg)
		return
	}

	tracks := discoverTracks(ctx, client, track)
	if len(tracks) == 0 {
		log.Println("No tracks found to summarize.")
		return
	}

	log.Printf("Summarizing %d track(s): %v", len(tracks), tracks)

	for _, t := range tracks {
		log.Printf("--- Processing track: %s ---", t)

		var entries []entry
		switch mode {
		case "daily":
			entries = getTodayEntries(ctx, client, t)
		case "3h":
			threeHoursAgo := time.Now().Add(-3 * time.Hour).Unix()
			entries = getTrackEntriesByTime(ctx, client, t, threeHoursAgo, time.Now().Unix())
		case "all":
			entries = getTrackEntries(ctx, client, t, cfg.Memory.SummarizeMaxEntries)
		}

		if len(entries) == 0 {
			log.Printf("  No entries for track %s, skipping.", t)
			continue
		}

		log.Printf("  Found %d entries.", len(entries))

		if dryRun {
			log.Printf("  [dry-run] Would summarize %d entries for track:%s", len(entries), t)
			continue
		}

		// Classify orphans within this track's time window
		untagged := filterUntagged(entries)
		if len(untagged) > 0 {
			log.Printf("  Classifying %d untagged entries...", len(untagged))
			classifyEntries(ctx, client, ollamaClient, untagged, tracks, cfg.Memory.ClassifyMaxChars)
		}

		log.Printf("  Generating summary...")
		summary, err := generateSummary(ctx, ollamaClient, t, entries, cfg.Memory.SummarizeMaxInputChars)
		if err != nil {
			log.Printf("  ERROR: %v", err)
			continue
		}

		switch mode {
		case "3h":
			store3hSummary(ctx, store, t, summary)
		default:
			storeSummary(ctx, store, t, summary)
		}
		log.Printf("  Stored %s summary for track:%s (%d chars)", mode, t, len(summary))
	}

	// Classify orphans not belonging to any track
	if mode == "daily" && track == "" {
		orphans := getOrphanEntries(ctx, client)
		if len(orphans) > 0 {
			log.Printf("--- Classifying %d orphan entries ---", len(orphans))
			if !dryRun {
				classifyEntries(ctx, client, ollamaClient, orphans, tracks, cfg.Memory.ClassifyMaxChars)
			}
		}
	}

	log.Println("Done.")
}

// --- Track discovery ---

func discoverTracks(ctx context.Context, client *redis.Client, filter string) []string {
	if filter != "" {
		return []string{filter}
	}

	allTags, err := client.SMembers(ctx, allTagsKey).Result()
	if err != nil {
		return nil
	}

	var tracks []string
	for _, tag := range allTags {
		if strings.HasPrefix(tag, "track:") {
			tracks = append(tracks, strings.TrimPrefix(tag, "track:"))
		}
	}
	sort.Strings(tracks)
	return tracks
}

// --- Entry retrieval ---

func getTodayEntries(ctx context.Context, client *redis.Client, track string) []entry {
	today := time.Now().Format("2006-01-02")
	dateTag := "date:" + today
	trackTag := "track:" + track

	ids, err := client.SInter(ctx, tagPrefix+trackTag, tagPrefix+dateTag).Result()
	if err != nil || len(ids) == 0 {
		return getTrackEntriesByTime(ctx, client, track, todayStart(), time.Now().Unix())
	}

	return loadEntries(ctx, client, ids)
}

func getTrackEntries(ctx context.Context, client *redis.Client, track string, limit int) []entry {
	trackTag := "track:" + track
	ids, err := client.SMembers(ctx, tagPrefix+trackTag).Result()
	if err != nil {
		return nil
	}

	entries := loadEntries(ctx, client, ids)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp > entries[j].Timestamp })

	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

func getTrackEntriesByTime(ctx context.Context, client *redis.Client, track string, start, end int64) []entry {
	ids, err := client.ZRangeByScore(ctx, timelineKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", start),
		Max: fmt.Sprintf("%d", end),
	}).Result()
	if err != nil {
		return nil
	}

	trackTag := "track:" + track
	trackMembers, err := client.SMembers(ctx, tagPrefix+trackTag).Result()
	if err != nil {
		return nil
	}
	trackSet := make(map[string]bool)
	for _, m := range trackMembers {
		trackSet[m] = true
	}

	var filtered []string
	for _, id := range ids {
		if trackSet[id] {
			filtered = append(filtered, id)
		}
	}

	return loadEntries(ctx, client, filtered)
}

func getOrphanEntries(ctx context.Context, client *redis.Client) []entry {
	today := time.Now().Format("2006-01-02")
	ids, err := client.SInter(ctx, tagPrefix+"date:"+today, tagPrefix+"auto:captured").Result()
	if err != nil || len(ids) == 0 {
		return nil
	}

	entries := loadEntries(ctx, client, ids)

	var orphans []entry
	for _, e := range entries {
		hasTrack := false
		for _, tag := range e.Tags {
			if strings.HasPrefix(tag, "track:") {
				hasTrack = true
				break
			}
		}
		if !hasTrack {
			orphans = append(orphans, e)
		}
	}
	return orphans
}

func filterUntagged(entries []entry) []entry {
	var untagged []entry
	for _, e := range entries {
		hasTrack := false
		for _, tag := range e.Tags {
			if strings.HasPrefix(tag, "track:") {
				hasTrack = true
				break
			}
		}
		if !hasTrack {
			untagged = append(untagged, e)
		}
	}
	return untagged
}

// --- Classification ---

func classifyEntries(ctx context.Context, client *redis.Client, ollamaClient *ollama.Client, entries []entry, tracks []string, classifyMax int) {
	if len(entries) == 0 {
		return
	}

	var prompt strings.Builder
	prompt.WriteString("You are a classification system. Assign each entry below to one or more tracks from this list:\n")
	for _, t := range tracks {
		prompt.WriteString(fmt.Sprintf("- %s\n", t))
	}
	prompt.WriteString("\nIf an entry doesn't clearly belong to any track, respond with \"none\".\n")
	prompt.WriteString("Respond ONLY with a JSON array: [{\"id\": \"<entry_id>\", \"tracks\": [\"TrackName\", ...]}]\n\n")
	prompt.WriteString("Entries:\n")

	for _, e := range entries {
		content := e.Content
		if len(content) > classifyMax {
			content = content[:classifyMax] + "..."
		}
		prompt.WriteString(fmt.Sprintf("ID: %s\nContent: %s\n\n", e.ID, content))
	}

	resp, err := ollamaClient.Generate(ctx, prompt.String())
	if err != nil {
		log.Printf("  Classification error: %v", err)
		return
	}

	resp = extractJSON(resp)

	var classifications []struct {
		ID     string   `json:"id"`
		Tracks []string `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(resp), &classifications); err != nil {
		log.Printf("  Failed to parse classification: %v", err)
		return
	}

	for _, c := range classifications {
		for _, track := range c.Tracks {
			if track == "none" || track == "" {
				continue
			}
			trackTag := "track:" + track
			key := entryPrefix + c.ID
			existing, err := client.HGet(ctx, key, "tags").Result()
			if err != nil {
				continue
			}
			tags := strings.Split(existing, ",")
			tags = append(tags, trackTag)
			tags = dedupe(tags)
			client.HSet(ctx, key, "tags", strings.Join(tags, ","))
			client.SAdd(ctx, tagPrefix+trackTag, c.ID)
			client.SAdd(ctx, allTagsKey, trackTag)
		}
	}

	log.Printf("  Classified %d entries.", len(classifications))
}

// --- Summarization ---

func generateSummary(ctx context.Context, ollamaClient *ollama.Client, track string, entries []entry, maxInputChars int) (string, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp < entries[j].Timestamp })

	var prompt strings.Builder
	prompt.WriteString(fmt.Sprintf(`You are a technical summarizer. Produce a concise, structured summary of the following entries about the "%s" track.

Requirements:
- Use markdown headers and bullet points
- Capture key decisions, insights, and open questions
- Note any action items or next steps
- Preserve technical detail
- If entries span multiple subtopics, organize by subtopic
- Keep under 2000 words

Entries:
`, track))

	totalChars := 0
	for _, e := range entries {
		content := e.Content
		if totalChars+len(content) > maxInputChars {
			content = content[:maxInputChars-totalChars]
			prompt.WriteString(fmt.Sprintf("---\n[%s]\n%s\n[TRUNCATED]\n", time.Unix(e.Timestamp, 0).Format("2006-01-02 15:04"), content))
			break
		}
		prompt.WriteString(fmt.Sprintf("---\n[%s]\n%s\n", time.Unix(e.Timestamp, 0).Format("2006-01-02 15:04"), content))
		totalChars += len(content)
	}

	return ollamaClient.Generate(ctx, prompt.String())
}

// --- Storage ---

func storeSummary(ctx context.Context, store *memory.RedisStore, track, summary string) {
	today := time.Now().Format("2006-01-02")
	entryID := fmt.Sprintf("summary:track:%s:%s", track, today)

	tags := []string{
		"track:" + track,
		strings.ToLower(track),
		"summary:track:" + track,
		"summary:daily",
		"date:" + today,
	}

	store.Put(ctx, memory.Entry{
		ID:        entryID,
		Content:   summary,
		Tags:      tags,
		Timestamp: time.Now(),
	})
}

func store3hSummary(ctx context.Context, store *memory.RedisStore, track, summary string) {
	now := time.Now()
	window := now.Format("2006-01-02-15")
	entryID := fmt.Sprintf("summary:3h:%s:%s", track, window)

	tags := []string{
		"track:" + track,
		strings.ToLower(track),
		"summary:3h:" + track,
		"summary:3h",
		"date:" + now.Format("2006-01-02"),
	}

	store.Put(ctx, memory.Entry{
		ID:        entryID,
		Content:   summary,
		Tags:      tags,
		Timestamp: now,
	})
}

// --- Weekly rollup ---

func runWeeklyRollup(ctx context.Context, client *redis.Client, store *memory.RedisStore, ollamaClient *ollama.Client, trackFilter string, dryRun bool) {
	tracks := discoverTracks(ctx, client, trackFilter)
	if len(tracks) == 0 {
		log.Println("No tracks found.")
		return
	}

	for _, t := range tracks {
		log.Printf("--- Weekly rollup: %s ---", t)

		summaryTag := "summary:track:" + t
		ids, err := client.SMembers(ctx, tagPrefix+summaryTag).Result()
		if err != nil || len(ids) == 0 {
			log.Printf("  No daily summaries for %s.", t)
			continue
		}

		weekAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
		var dailySummaries []entry
		for _, id := range ids {
			data, err := client.HGetAll(ctx, entryPrefix+id).Result()
			if err != nil || len(data) == 0 {
				continue
			}
			var ts int64
			fmt.Sscanf(data["timestamp"], "%d", &ts)
			if ts >= weekAgo {
				var tags []string
				if tStr := data["tags"]; tStr != "" {
					tags = strings.Split(tStr, ",")
				}
				dailySummaries = append(dailySummaries, entry{ID: data["id"], Content: data["content"], Tags: tags, Timestamp: ts})
			}
		}

		if len(dailySummaries) == 0 {
			continue
		}

		log.Printf("  Rolling up %d daily summaries.", len(dailySummaries))
		if dryRun {
			continue
		}

		var prompt strings.Builder
		prompt.WriteString(fmt.Sprintf(`Produce a weekly summary for the "%s" track from these daily summaries.

Capture the arc: what started, progressed, was decided, remains open.
Note shifts in direction. Identify patterns. Keep under 1500 words. Use markdown.

Daily summaries:
`, t))

		sort.Slice(dailySummaries, func(i, j int) bool { return dailySummaries[i].Timestamp < dailySummaries[j].Timestamp })
		for _, ds := range dailySummaries {
			prompt.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", time.Unix(ds.Timestamp, 0).Format("2006-01-02"), ds.Content))
		}

		summary, err := ollamaClient.Generate(ctx, prompt.String())
		if err != nil {
			log.Printf("  ERROR: %v", err)
			continue
		}

		now := time.Now()
		weekStart := now.Add(-7 * 24 * time.Hour).Format("2006-01-02")
		entryID := fmt.Sprintf("summary:week:%s:%s", t, weekStart)

		tags := []string{"track:" + t, strings.ToLower(t), "summary:week:" + t, "summary:weekly", "date:" + now.Format("2006-01-02")}

		store.Put(ctx, memory.Entry{
			ID:        entryID,
			Content:   summary,
			Tags:      tags,
			Timestamp: now,
		})

		log.Printf("  Stored weekly summary (%d chars)", len(summary))
	}

	log.Println("Weekly rollup done.")
}

// --- Cross-track review ---

func runCrossTrackReview(ctx context.Context, client *redis.Client, store *memory.RedisStore, ollamaClient *ollama.Client, crossTrackMax int, dryRun bool) {
	log.Println("=== Cross-track review ===")

	tracks := discoverTracks(ctx, client, "")
	if len(tracks) < 2 {
		log.Println("Need at least 2 tracks.")
		return
	}

	type trackSummary struct {
		Track, Summary, ID string
	}

	var summaries []trackSummary
	for _, t := range tracks {
		summaryTag := "summary:track:" + t
		ids, err := client.SMembers(ctx, tagPrefix+summaryTag).Result()
		if err != nil || len(ids) == 0 {
			continue
		}

		var bestID string
		var bestTS int64
		for _, id := range ids {
			ts, err := client.HGet(ctx, entryPrefix+id, "timestamp").Int64()
			if err != nil {
				continue
			}
			if ts > bestTS {
				bestTS = ts
				bestID = id
			}
		}
		if bestID == "" {
			continue
		}

		content, _ := client.HGet(ctx, entryPrefix+bestID, "content").Result()
		summaries = append(summaries, trackSummary{Track: t, Summary: content, ID: bestID})
	}

	if len(summaries) < 2 {
		log.Println("Not enough summaries for cross-track analysis.")
		return
	}

	if dryRun {
		log.Printf("[dry-run] Would analyze %d track summaries.", len(summaries))
		return
	}

	var prompt strings.Builder
	prompt.WriteString(`Analyze these project tracks for cross-cutting themes, shared concepts, and synergies.

Output a JSON array of links:
[{"track_a": "Name", "track_b": "Name", "concept": "shared concept", "score": 0.7}]

Score: 0.9-1.0 direct dependency, 0.7-0.8 same concept different application, 0.5-0.6 tangential.
Only include genuine, non-obvious connections. Respond ONLY with JSON.

Track summaries:
`)

	for _, ts := range summaries {
		content := ts.Summary
		if len(content) > crossTrackMax {
			content = content[:crossTrackMax] + "..."
		}
		prompt.WriteString(fmt.Sprintf("\n=== %s ===\n%s\n", ts.Track, content))
	}

	resp, err := ollamaClient.Generate(ctx, prompt.String())
	if err != nil {
		log.Printf("ERROR: %v", err)
		return
	}

	resp = extractJSON(resp)

	var links []struct {
		TrackA  string  `json:"track_a"`
		TrackB  string  `json:"track_b"`
		Concept string  `json:"concept"`
		Score   float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(resp), &links); err != nil {
		log.Printf("Failed to parse: %v", err)
		return
	}

	for _, link := range links {
		var idA, idB string
		for _, ts := range summaries {
			if ts.Track == link.TrackA {
				idA = ts.ID
			}
			if ts.Track == link.TrackB {
				idB = ts.ID
			}
		}
		if idA == "" || idB == "" {
			continue
		}

		client.ZAdd(ctx, "link:"+idA, redis.Z{Score: link.Score, Member: idB})
		client.ZAdd(ctx, "link:"+idB, redis.Z{Score: link.Score, Member: idA})
		log.Printf("  Linked %s ↔ %s (%.1f): %s", link.TrackA, link.TrackB, link.Score, link.Concept)
	}

	if len(links) > 0 {
		var content strings.Builder
		content.WriteString("## Cross-Track Connections\n\n")
		for _, link := range links {
			content.WriteString(fmt.Sprintf("- **%s ↔ %s** (%.1f): %s\n", link.TrackA, link.TrackB, link.Score, link.Concept))
		}

		now := time.Now()
		entryID := fmt.Sprintf("summary:cross-track:%s", now.Format("2006-01-02"))
		tags := []string{"summary:cross-track", "summary:weekly", "date:" + now.Format("2006-01-02")}

		store.Put(ctx, memory.Entry{
			ID:        entryID,
			Content:   content.String(),
			Tags:      tags,
			Timestamp: now,
		})

		log.Printf("  Stored cross-track summary with %d connections.", len(links))
	}

	log.Println("Cross-track review done.")
}

// --- Helpers ---

func loadEntries(ctx context.Context, client *redis.Client, ids []string) []entry {
	var entries []entry
	for _, id := range ids {
		data, err := client.HGetAll(ctx, entryPrefix+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		var ts int64
		fmt.Sscanf(data["timestamp"], "%d", &ts)
		var tags []string
		if t := data["tags"]; t != "" {
			tags = strings.Split(t, ",")
		}

		// Security: exclude full web content entries from summarization.
		// These are untrusted and could contain prompt injection payloads
		// that would be laundered through the summarizer into high-trust summaries.
		if hasTag(tags, "content:full") {
			continue
		}

		entries = append(entries, entry{ID: data["id"], Content: data["content"], Tags: tags, Timestamp: ts})
	}
	return entries
}

// hasTag checks if a tag slice contains a specific tag.
func hasTag(tags []string, target string) bool {
	for _, t := range tags {
		if t == target {
			return true
		}
	}
	return false
}

func extractJSON(s string) string {
	if idx := strings.Index(s, "</think>"); idx != -1 {
		s = s[idx+len("</think>"):]
	}
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}
	return strings.TrimSpace(s)
}

func todayStart() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

func dedupe(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}


