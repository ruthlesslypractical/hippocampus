// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package classify provides track classification for memory entries.
// It uses windowed session context and track manifests to assign entries
// to one or more project tracks.
package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
)

const (
	entryPrefix = "entry:"
	tagPrefix   = "tag:"
	allTagsKey  = "tags:all"
	timelineKey = "timeline"
	manifestKey = "meta:track-manifests"
)

// Entry is a minimal entry representation for classification.
type Entry struct {
	ID        string
	Content   string
	Tags      []string
	Timestamp int64
}

// Result holds the classification output for a single entry.
type Result struct {
	ID       string   `json:"id"`
	Tracks   []string `json:"tracks"`
	Confused bool     `json:"confused"`
	Reason   string   `json:"reason,omitempty"`
}

// ProgressEvent is emitted for each classified entry.
type ProgressEvent struct {
	Current   int
	Total     int
	EntryID   string
	OldTracks []string
	NewTracks []string
	Confused  bool
}

// Link represents a discovered relationship between two entries.
type Link struct {
	A      string  `json:"a"`
	B      string  `json:"b"`
	Score  float64 `json:"score"`
	Type   string  `json:"type,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// Options configures a classification run.
type Options struct {
	// LeadContext is how many messages before the core to include as read-only context.
	// Default: 10.
	LeadContext int

	// CoreSize is how many messages to classify per window.
	// Default: 20.
	CoreSize int

	// TrailContext is how many messages after the core to include as read-only context.
	// Default: 10.
	TrailContext int

	// MaxContentChars truncates entry content at this length.
	MaxContentChars int

	// BatchSize is how many entries to classify per LLM call (within a core window).
	// Default: 20 (same as CoreSize).
	BatchSize int

	// Force reclassifies even if already classified.
	Force bool

	// DryRun returns results without applying them.
	DryRun bool

	// OnProgress is called after each entry is classified. Optional.
	OnProgress func(ProgressEvent)
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() Options {
	return Options{
		LeadContext:     10,
		CoreSize:        20,
		TrailContext:    10,
		MaxContentChars: 1500,
		BatchSize:       20,
	}
}

// ClassifyEntries classifies a batch of entries using a sliding window approach.
// Each window has: [lead context] [core to classify] [trail context]
// Only the core entries get classified; lead/trail provide continuity context.
// Entries with explicit "track:<X>" mentions in content are fast-path classified without LLM.
func ClassifyEntries(ctx context.Context, client *redis.Client, ollamaClient *ollama.Client, entries []Entry, opts Options) ([]Result, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	if opts.LeadContext == 0 {
		opts.LeadContext = 10
	}
	if opts.CoreSize == 0 {
		opts.CoreSize = 20
	}
	if opts.TrailContext == 0 {
		opts.TrailContext = 10
	}
	if opts.MaxContentChars == 0 {
		opts.MaxContentChars = 1500
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = opts.CoreSize
	}

	// Load track manifests
	manifests, err := loadManifests(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("loading track manifests: %w", err)
	}

	// Build valid track name set for fast-path detection
	validTracks := make(map[string]bool)
	for name := range manifests {
		if !strings.Contains(name, " ") { // Skip disambiguation notes
			validTracks[name] = true
		}
	}

	// Build old-tracks lookup for progress reporting
	oldTracksMap := make(map[string][]string)
	for _, e := range entries {
		oldTracksMap[e.ID] = getTrackTags(e.Tags)
	}

	// Group entries by session for windowed context
	sessionGroups := groupBySession(entries)

	var allResults []Result
	processed := 0

	for sessionID, group := range sessionGroups {
		// Fast-path: check for explicit "track:<X>" mentions in content
		var needsLLM []Entry
		for _, e := range group {
			if track := detectExplicitTrack(e.Content, validTracks); track != "" {
				// Fast-path classification — user explicitly stated the track
				result := Result{ID: e.ID, Tracks: []string{track}, Confused: false}
				allResults = append(allResults, result)
				processed++
				if opts.OnProgress != nil {
					opts.OnProgress(ProgressEvent{
						Current:   processed,
						Total:     len(entries),
						EntryID:   e.ID,
						OldTracks: oldTracksMap[e.ID],
						NewTracks: []string{track},
						Confused:  false,
					})
				}
			} else {
				needsLLM = append(needsLLM, e)
			}
		}

		if len(needsLLM) == 0 {
			continue
		}

		// Load the full session for sliding window context
		var sessionEntries []Entry
		if sessionID != "" {
			sessionEntries = loadFullSession(ctx, client, sessionID)
		}
		if len(sessionEntries) == 0 {
			sessionEntries = needsLLM
		}

		// Build index of entries-to-classify within the session
		targetIDs := make(map[string]bool)
		for _, e := range needsLLM {
			targetIDs[e.ID] = true
		}

		// Sliding window: stride by CoreSize, with LeadContext before and TrailContext after
		results, err := classifyWithSlidingWindow(ctx, ollamaClient, client, sessionEntries, targetIDs, manifests, opts)
		if err != nil {
			slog.Error("classification error", "session", sessionID, "error", err)
			continue
		}

		// Emit progress events
		for _, r := range results {
			processed++
			if opts.OnProgress != nil {
				opts.OnProgress(ProgressEvent{
					Current:   processed,
					Total:     len(entries),
					EntryID:   r.ID,
					OldTracks: oldTracksMap[r.ID],
					NewTracks: r.Tracks,
					Confused:  r.Confused,
				})
			}
		}

		allResults = append(allResults, results...)
	}

	// Apply results unless dry-run
	if !opts.DryRun {
		if opts.Force {
			for _, e := range entries {
				stripAutoClassification(ctx, client, &e)
			}
		}
		applyResults(ctx, client, allResults)
	}

	return allResults, nil
}

// classifyWithSlidingWindow processes entries using overlapping windows.
// Window structure: [LeadContext | CoreSize | TrailContext]
// Stride: CoreSize (so windows overlap by LeadContext + TrailContext)
func classifyWithSlidingWindow(ctx context.Context, ollamaClient *ollama.Client, client *redis.Client, sessionEntries []Entry, targetIDs map[string]bool, manifests map[string]string, opts Options) ([]Result, error) {
	var allResults []Result

	// Find indices of target entries in the sorted session
	targetIndices := []int{}
	for i, e := range sessionEntries {
		if targetIDs[e.ID] {
			targetIndices = append(targetIndices, i)
		}
	}

	if len(targetIndices) == 0 {
		return nil, nil
	}

	// Process in sliding windows
	// Start from the first target, stride by CoreSize
	windowStart := targetIndices[0]
	for windowStart <= targetIndices[len(targetIndices)-1] {
		windowEnd := windowStart + opts.CoreSize
		if windowEnd > len(sessionEntries) {
			windowEnd = len(sessionEntries)
		}

		// Determine core: entries in [windowStart, windowEnd) that are targets
		var core []Entry
		for i := windowStart; i < windowEnd; i++ {
			if targetIDs[sessionEntries[i].ID] {
				core = append(core, sessionEntries[i])
			}
		}

		if len(core) == 0 {
			windowStart += opts.CoreSize
			continue
		}

		// Lead context: entries before the core window
		leadStart := windowStart - opts.LeadContext
		if leadStart < 0 {
			leadStart = 0
		}
		leadContext := sessionEntries[leadStart:windowStart]

		// Trail context: entries after the core window
		trailEnd := windowEnd + opts.TrailContext
		if trailEnd > len(sessionEntries) {
			trailEnd = len(sessionEntries)
		}
		trailContext := sessionEntries[windowEnd:trailEnd]

		// Classify the core with lead+trail as context
		results, links, err := classifyWindowedBatch(ctx, ollamaClient, core, leadContext, trailContext, manifests, opts)
		if err != nil {
			return allResults, fmt.Errorf("classifying window at offset %d: %w", windowStart, err)
		}

		// Apply links
		if !opts.DryRun && len(links) > 0 {
			applyLinks(ctx, client, links)
		}

		allResults = append(allResults, results...)

		// Stride forward by CoreSize
		windowStart += opts.CoreSize
	}

	return allResults, nil
}

// classifyWindowedBatch sends a single LLM request for a core batch with lead/trail context.
func classifyWindowedBatch(ctx context.Context, ollamaClient *ollama.Client, core, leadContext, trailContext []Entry, manifests map[string]string, opts Options) ([]Result, []Link, error) {
	prompt := buildSlidingWindowPrompt(core, leadContext, trailContext, manifests, opts.MaxContentChars)

	resp, err := ollamaClient.Generate(ctx, prompt)
	if err != nil {
		return nil, nil, err
	}

	return parseClassifyResponse(resp, core)
}

// detectExplicitTrack checks if message content contains an explicit "track:<ValidTrack>"
// annotation from the user. Returns the track name if found, empty string otherwise.
func detectExplicitTrack(content string, validTracks map[string]bool) string {
	// Look for patterns like "track:MyProject" or "track:AnotherProject" (case-sensitive match against valid tracks)
	idx := strings.Index(content, "track:")
	if idx == -1 {
		return ""
	}

	// Extract the word after "track:"
	rest := content[idx+6:]
	// Track name ends at space, comma, paren, period, newline, or end of string
	end := strings.IndexAny(rest, " ,.)(\n\r\t")
	trackName := rest
	if end > 0 {
		trackName = rest[:end]
	}

	if validTracks[trackName] {
		return trackName
	}
	return ""
}

// loadFullSession loads all entries for a session, sorted by timestamp.
func loadFullSession(ctx context.Context, client *redis.Client, sessionID string) []Entry {
	ids, err := client.SMembers(ctx, tagPrefix+"session:"+sessionID).Result()
	if err != nil || len(ids) == 0 {
		return nil
	}
	entries := loadEntries(ctx, client, ids)
	sortByTimestamp(entries)
	return entries
}

// ClassifySingle classifies a single entry with its surrounding context.
func ClassifySingle(ctx context.Context, client *redis.Client, ollamaClient *ollama.Client, entryID string, opts Options) (*Result, error) {
	entry, err := loadEntry(ctx, client, entryID)
	if err != nil {
		return nil, err
	}

	// Check if already classified (unless force)
	if !opts.Force && hasTag(entry.Tags, "classified") && !hasTag(entry.Tags, "classified:auto") {
		return &Result{ID: entryID, Tracks: getTrackTags(entry.Tags)}, nil
	}

	results, err := ClassifyEntries(ctx, client, ollamaClient, []Entry{*entry}, opts)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no classification result for %s", entryID)
	}
	return &results[0], nil
}

// Reclassify strips auto-classification and re-runs on entries in the given list.
// Skips entries with manual classification (classified without classified:auto).
func Reclassify(ctx context.Context, client *redis.Client, ollamaClient *ollama.Client, entryIDs []string, opts Options) ([]Result, error) {
	var toClassify []Entry

	for _, id := range entryIDs {
		entry, err := loadEntry(ctx, client, id)
		if err != nil {
			continue
		}

		// Skip manual corrections
		if hasTag(entry.Tags, "classified") && !hasTag(entry.Tags, "classified:auto") {
			continue
		}

		// Strip existing auto-classification
		if !opts.DryRun {
			stripAutoClassification(ctx, client, entry)
		}

		toClassify = append(toClassify, *entry)
	}

	opts.Force = true
	return ClassifyEntries(ctx, client, ollamaClient, toClassify, opts)
}

// --- Internal ---

func loadManifests(ctx context.Context, client *redis.Client) (map[string]string, error) {
	content, err := client.HGet(ctx, entryPrefix+manifestKey, "content").Result()
	if err != nil {
		return nil, fmt.Errorf("manifest entry not found (store meta:track-manifests first): %w", err)
	}

	var manifests map[string]string
	if err := json.Unmarshal([]byte(content), &manifests); err != nil {
		return nil, fmt.Errorf("parsing manifest JSON: %w", err)
	}
	return manifests, nil
}

func groupBySession(entries []Entry) map[string][]Entry {
	groups := make(map[string][]Entry)
	for _, e := range entries {
		sessionID := ""
		for _, tag := range e.Tags {
			if strings.HasPrefix(tag, "session:") {
				sessionID = strings.TrimPrefix(tag, "session:")
				break
			}
		}
		groups[sessionID] = append(groups[sessionID], e)
	}
	return groups
}





func buildSlidingWindowPrompt(core, leadContext, trailContext []Entry, manifests map[string]string, maxChars int) string {
	var b strings.Builder

	b.WriteString("You are a classification system for a personal knowledge base. Assign each TARGET entry to one or more project tracks.\n\n")

	// Track descriptions
	b.WriteString("## Available Tracks\n\n")
	for name, desc := range manifests {
		if strings.Contains(name, " ") {
			b.WriteString(fmt.Sprintf("NOTE: %s\n\n", desc))
			continue
		}
		b.WriteString(fmt.Sprintf("**%s**: %s\n\n", name, desc))
	}

	// Lead context (preceding messages — read-only)
	if len(leadContext) > 0 {
		b.WriteString("## Preceding Context (DO NOT classify — for continuity and linking)\n\n")
		for _, e := range leadContext {
			content := util.Truncate(e.Content, maxChars/2)
			tracks := getTrackTags(e.Tags)
			trackStr := ""
			if len(tracks) > 0 {
				trackStr = fmt.Sprintf(" [track: %s]", strings.Join(tracks, ", "))
			}
			b.WriteString(fmt.Sprintf("ID: %s%s\nContent: %s\n\n", e.ID, trackStr, content))
		}
	}

	// Core entries (to be classified)
	b.WriteString("## === ENTRIES TO CLASSIFY === \n\n")
	for _, e := range core {
		content := util.Truncate(e.Content, maxChars)
		existingTracks := getTrackTags(e.Tags)
		meta := ""
		if len(existingTracks) > 0 {
			meta = fmt.Sprintf(" (currently tagged: %s)", strings.Join(existingTracks, ", "))
		}
		b.WriteString(fmt.Sprintf("ID: %s%s\nContent: %s\n\n", e.ID, meta, content))
	}
	b.WriteString("## === END CLASSIFICATION ZONE ===\n\n")

	// Trail context (following messages — read-only)
	if len(trailContext) > 0 {
		b.WriteString("## Following Context (DO NOT classify — for continuity and linking)\n\n")
		for _, e := range trailContext {
			content := util.Truncate(e.Content, maxChars/2)
			tracks := getTrackTags(e.Tags)
			trackStr := ""
			if len(tracks) > 0 {
				trackStr = fmt.Sprintf(" [track: %s]", strings.Join(tracks, ", "))
			}
			b.WriteString(fmt.Sprintf("ID: %s%s\nContent: %s\n\n", e.ID, trackStr, content))
		}
	}

	// Instructions
	b.WriteString(`## Instructions

1. Assign each entry IN THE CLASSIFICATION ZONE to one or more tracks from the list above. An entry CAN belong to multiple tracks if the discussion substantively involves both.
2. A passing mention of another project does NOT warrant a track tag. Only tag if there's substantive content about that track.
3. Use the preceding and following context to disambiguate ambiguous entries. Short messages like "OK" or "yeah" should inherit the track of their surrounding conversation.
4. If an entry genuinely doesn't fit ANY track, assign tracks: ["none"].
5. If you are UNCERTAIN between two tracks and can't confidently decide, set "confused": true and explain briefly in "reason".
6. Identify LINKS between any entries visible in this prompt — including links FROM classification zone entries TO entries in the preceding/following context. Only include strong relationships, not every sequential pair.

Respond with ONLY this JSON object (no markdown, no explanation):
{
  "classifications": [{"id": "<entry_id>", "tracks": ["TrackName", ...], "confused": false, "reason": ""}],
  "links": [{"a": "<id1>", "b": "<id2>", "score": 0.8, "type": "extends", "reason": "brief explanation"}]
}

IMPORTANT: Only return classifications for entries in the CLASSIFICATION ZONE. Do NOT classify context entries. Links, however, MAY reference any entry ID visible in this prompt (context or core).

Link types: "extends" (builds on), "supports" (reinforces), "contradicts" (disagrees/supersedes), "preceded_by" (temporal sequence with causal connection).
Score: 0.5-1.0 for positive relationships, -0.5 to -1.0 for contradictions/superseded content.
Only include links with |score| >= 0.6. Omit "links" array entirely if none found.
`)

	return b.String()
}



// batchResponse wraps the full classification + linking response.
type batchResponse struct {
	Classifications []Result `json:"classifications"`
	Links           []Link   `json:"links"`
}

func parseClassifyResponse(resp string, targets []Entry) ([]Result, []Link, error) {
	// Strip think blocks
	resp = stripThinkBlocks(resp)

	// Try to parse as wrapped object first (new format with links)
	if idx := strings.Index(resp, "{"); idx != -1 {
		// Check if it's a wrapped object (has "classifications" key)
		if strings.Contains(resp[idx:], `"classifications"`) {
			endIdx := strings.LastIndex(resp, "}")
			if endIdx > idx {
				jsonStr := sanitizeJSON(resp[idx : endIdx+1])
				var wrapped batchResponse
				if err := json.Unmarshal([]byte(jsonStr), &wrapped); err == nil && len(wrapped.Classifications) > 0 {
					return wrapped.Classifications, wrapped.Links, nil
				}
			}
		}
	}

	// Fall back to plain array format (no links)
	start := strings.Index(resp, "[")
	end := strings.LastIndex(resp, "]")
	if start == -1 || end == -1 || end <= start {
		return nil, nil, fmt.Errorf("no JSON array in response")
	}

	jsonStr := sanitizeJSON(resp[start : end+1])

	var results []Result
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		return nil, nil, fmt.Errorf("parsing classification JSON: %w (raw: %.500s)", err, jsonStr)
	}

	return results, nil, nil
}

// sanitizeJSON attempts to fix common LLM JSON generation errors.
func sanitizeJSON(s string) string {
	// Fix trailing whitespace inside strings before closing quote
	// (LLMs sometimes emit: "reason": "blah. " } — the space is fine but
	// sometimes they embed unescaped quotes within strings)

	// Replace smart quotes with regular quotes
	s = strings.ReplaceAll(s, "\u201c", `\"`)
	s = strings.ReplaceAll(s, "\u201d", `\"`)
	s = strings.ReplaceAll(s, "\u2018", `'`)
	s = strings.ReplaceAll(s, "\u2019", `'`)

	return s
}

func applyResults(ctx context.Context, client *redis.Client, results []Result) {
	for _, r := range results {
		key := entryPrefix + r.ID
		existing, err := client.HGet(ctx, key, "tags").Result()
		if err != nil {
			continue
		}

		tags := strings.Split(existing, ",")

		// Add track tags
		for _, track := range r.Tracks {
			if track == "none" || track == "" {
				continue
			}
			trackTag := "track:" + track
			tags = append(tags, trackTag)
			client.SAdd(ctx, tagPrefix+trackTag, r.ID)
			client.SAdd(ctx, allTagsKey, trackTag)
		}

		// Mark as classified
		tags = append(tags, "classified", "classified:auto")
		client.SAdd(ctx, tagPrefix+"classified", r.ID)
		client.SAdd(ctx, tagPrefix+"classified:auto", r.ID)
		client.SAdd(ctx, allTagsKey, "classified", "classified:auto")

		// If confused, add that tag too
		if r.Confused {
			tags = append(tags, "classified:confused")
			client.SAdd(ctx, tagPrefix+"classified:confused", r.ID)
			client.SAdd(ctx, allTagsKey, "classified:confused")
		}

		// If no track assigned, mark unclassifiable
		hasTrack := false
		for _, track := range r.Tracks {
			if track != "none" && track != "" {
				hasTrack = true
				break
			}
		}
		if !hasTrack {
			tags = append(tags, "unclassifiable")
			client.SAdd(ctx, tagPrefix+"unclassifiable", r.ID)
			client.SAdd(ctx, allTagsKey, "unclassifiable")
		}

		tags = util.Dedupe(tags)
		client.HSet(ctx, key, "tags", strings.Join(tags, ","))
	}
}

func applyLinks(ctx context.Context, client *redis.Client, links []Link) {
	for _, l := range links {
		if l.A == "" || l.B == "" {
			continue
		}
		// Unified format: HASH links:<id> with field=targetID, value="score|type"
		linkType := l.Type
		if linkType == "" {
			linkType = "corecall"
		}
		value := fmt.Sprintf("%.4f|%s", l.Score, linkType)

		// Bidirectional
		client.HSet(ctx, "links:"+l.A, l.B, value)
		client.HSet(ctx, "links:"+l.B, l.A, value)
	}
}

func stripAutoClassification(ctx context.Context, client *redis.Client, entry *Entry) {
	key := entryPrefix + entry.ID
	var newTags []string
	for _, tag := range entry.Tags {
		if strings.HasPrefix(tag, "track:") ||
			tag == "classified:auto" ||
			tag == "classified:confused" ||
			tag == "unclassifiable" {
			// Remove from tag set
			client.SRem(ctx, tagPrefix+tag, entry.ID)
			continue
		}
		// Keep "classified" so we know it was looked at (will be re-added as classified:auto)
		if tag == "classified" {
			client.SRem(ctx, tagPrefix+tag, entry.ID)
			continue
		}
		newTags = append(newTags, tag)
	}
	client.HSet(ctx, key, "tags", strings.Join(newTags, ","))
	entry.Tags = newTags
}

// --- Helpers ---

func loadEntry(ctx context.Context, client *redis.Client, id string) (*Entry, error) {
	data, err := client.HGetAll(ctx, entryPrefix+id).Result()
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("entry not found: %s", id)
	}
	var ts int64
	fmt.Sscanf(data["timestamp"], "%d", &ts)
	return &Entry{
		ID:        id,
		Content:   data["content"],
		Tags:      strings.Split(data["tags"], ","),
		Timestamp: ts,
	}, nil
}

func loadEntries(ctx context.Context, client *redis.Client, ids []string) []Entry {
	var entries []Entry
	for _, id := range ids {
		e, err := loadEntry(ctx, client, id)
		if err == nil {
			entries = append(entries, *e)
		}
	}
	return entries
}

func sortByTimestamp(entries []Entry) {
	for i := 0; i < len(entries)-1; i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Timestamp < entries[i].Timestamp {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

func getTrackTags(tags []string) []string {
	var tracks []string
	for _, t := range tags {
		if strings.HasPrefix(t, "track:") {
			tracks = append(tracks, strings.TrimPrefix(t, "track:"))
		}
	}
	return tracks
}



func stripThinkBlocks(s string) string {
	for {
		start := strings.Index(s, "<think>")
		if start == -1 {
			break
		}
		end := strings.Index(s, "</think>")
		if end == -1 {
			s = s[:start]
			break
		}
		s = s[:start] + s[end+8:]
	}
	return s
}
