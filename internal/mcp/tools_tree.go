// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TreeNode represents one node in a summary provenance tree.
type TreeNode struct {
	ID    string     `json:"id"`
	Level string     `json:"level"` // "weekly", "daily", "3h", "entry"
	Tags  []string   `json:"tags,omitempty"`
	Date  string     `json:"date,omitempty"`
	Kids  []TreeNode `json:"children,omitempty"`
}

// toolSummaryTree walks the summary hierarchy and returns the provenance tree.
func (s *Server) toolSummaryTree(ctx context.Context, args map[string]interface{}) (CallToolResult, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return CallToolResult{}, fmt.Errorf("id is required")
	}

	// Optional: max depth (default: full recursion)
	maxDepth := 10
	if d, ok := args["max_depth"].(float64); ok && d > 0 {
		maxDepth = int(d)
	}

	// Verify the entry exists
	entry, err := s.store.Get(ctx, id)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("entry not found: %s", id)
	}

	// Parse the summary ID to determine level, track, and date
	level, track, date := parseSummaryID(id)
	if level == "" {
		return CallToolResult{}, fmt.Errorf("cannot parse summary ID format: %s (expected summary:<level>:<track>:<date>)", id)
	}

	root := TreeNode{
		ID:    id,
		Level: level,
		Tags:  entry.Tags,
		Date:  date,
	}

	// Recursively build the tree
	root.Kids = s.getChildren(ctx, level, track, date, maxDepth, 1)

	data, _ := json.MarshalIndent(root, "", "  ")
	return CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(data)}},
	}, nil
}

// toolSummaryLeaves returns just the leaf entry IDs (deepest level) for a summary.
// This is the "give me the list so I can iterate and retag" version.
func (s *Server) toolSummaryLeaves(ctx context.Context, args map[string]interface{}) (CallToolResult, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return CallToolResult{}, fmt.Errorf("id is required")
	}

	// Verify the entry exists
	_, err := s.store.Get(ctx, id)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("entry not found: %s", id)
	}

	level, track, date := parseSummaryID(id)
	if level == "" {
		return CallToolResult{}, fmt.Errorf("cannot parse summary ID format: %s", id)
	}

	leaves := s.collectLeaves(ctx, level, track, date)

	type leafEntry struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}

	var result []leafEntry
	for _, leaf := range leaves {
		entry, err := s.store.Get(ctx, leaf)
		if err != nil {
			continue
		}
		result = append(result, leafEntry{ID: entry.ID, Tags: entry.Tags})
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"summary_id":  id,
		"level":       level,
		"track":       track,
		"date":        date,
		"leaf_count":  len(result),
		"leaves":      result,
	}, "", "  ")

	return CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(data)}},
	}, nil
}

// parseSummaryID extracts level, track, and date from a summary entry ID.
// Formats:
//   summary:weekly:<Track>:<date>       → ("weekly", Track, date)
//   summary:daily:<Track>:<date>        → ("daily", Track, date)  [alias: summary:track:<Track>:<date>]
//   summary:track:<Track>:<date>        → ("daily", Track, date)
//   summary:3h:<Track>:<date-hour>      → ("3h", Track, date-hour)
func parseSummaryID(id string) (level, track, date string) {
	parts := strings.SplitN(id, ":", 4)
	if len(parts) < 4 || parts[0] != "summary" {
		return "", "", ""
	}

	switch parts[1] {
	case "weekly":
		return "weekly", parts[2], parts[3]
	case "daily":
		return "daily", parts[2], parts[3]
	case "track":
		// summary:track:<Track>:<date> is the daily format
		return "daily", parts[2], parts[3]
	case "3h":
		return "3h", parts[2], parts[3]
	default:
		return "", "", ""
	}
}

// getChildren finds child summaries/entries for a given level.
func (s *Server) getChildren(ctx context.Context, level, track, date string, maxDepth, currentDepth int) []TreeNode {
	if currentDepth >= maxDepth {
		return nil
	}

	switch level {
	case "weekly":
		return s.getWeeklyChildren(ctx, track, date, maxDepth, currentDepth)
	case "daily":
		return s.getDailyChildren(ctx, track, date, maxDepth, currentDepth)
	case "3h":
		return s.get3hChildren(ctx, track, date)
	default:
		return nil
	}
}

// getWeeklyChildren finds daily summaries within the week.
func (s *Server) getWeeklyChildren(ctx context.Context, track, weekStart string, maxDepth, currentDepth int) []TreeNode {
	// Parse week start date, find all dailies in that 7-day window
	t, err := time.Parse("2006-01-02", weekStart)
	if err != nil {
		return nil
	}
	weekEnd := t.AddDate(0, 0, 7)

	// Search for daily summaries on this track within the week
	// Daily IDs: summary:track:<Track>:<date> OR summary:daily:<Track>:<date>
	summaryTag := "summary:track:" + track
	entries, err := s.store.ByTags(ctx, []string{summaryTag, "summary:daily"}, 100, 0)
	if err != nil || len(entries) == 0 {
		// Also try summary:daily:<Track> tag
		summaryTag = "summary:daily:" + track
		entries, err = s.store.ByTags(ctx, []string{summaryTag}, 100, 0)
		if err != nil {
			return nil
		}
	}

	var kids []TreeNode
	for _, e := range entries {
		// Filter to entries within the week window
		if e.Timestamp.Before(t) || !e.Timestamp.Before(weekEnd) {
			continue
		}
		dateStr := e.Timestamp.Format("2006-01-02")
		node := TreeNode{
			ID:    e.ID,
			Level: "daily",
			Tags:  e.Tags,
			Date:  dateStr,
		}
		node.Kids = s.getChildren(ctx, "daily", track, dateStr, maxDepth, currentDepth+1)
		kids = append(kids, node)
	}
	return kids
}

// getDailyChildren finds 3h summaries for a specific day.
func (s *Server) getDailyChildren(ctx context.Context, track, dateStr string, maxDepth, currentDepth int) []TreeNode {
	// 3h summaries are tagged: summary:3h:<Track> + date:<date>
	// OR we can search by tag intersection
	summaryTag := "summary:3h:" + track
	dateTag := "date:" + dateStr

	entries, err := s.store.ByTags(ctx, []string{summaryTag, dateTag}, 100, 0)
	if err != nil || len(entries) == 0 {
		// No 3h summaries — fall through to raw entries directly
		return s.getRawEntriesForDay(ctx, track, dateStr)
	}

	var kids []TreeNode
	for _, e := range entries {
		// Extract the hour window from the ID (e.g., "2026-06-14-15")
		_, _, hourDate := parseSummaryID(e.ID)
		node := TreeNode{
			ID:    e.ID,
			Level: "3h",
			Tags:  e.Tags,
			Date:  hourDate,
		}
		node.Kids = s.getChildren(ctx, "3h", track, hourDate, maxDepth, currentDepth+1)
		kids = append(kids, node)
	}
	return kids
}

// get3hChildren finds raw entries within a 3-hour window.
func (s *Server) get3hChildren(ctx context.Context, track, dateHour string) []TreeNode {
	// dateHour format: "2026-06-14-15" (date + start hour)
	// Parse to get the 3h window
	t, err := time.Parse("2006-01-02-15", dateHour)
	if err != nil {
		// Try just the date
		t, err = time.Parse("2006-01-02", dateHour)
		if err != nil {
			return nil
		}
	}
	windowEnd := t.Add(3 * time.Hour)

	// Get entries in this time window that belong to this track
	entries, err := s.store.EntriesByTimeRange(ctx, t.Unix(), windowEnd.Unix(), nil, 200)
	if err != nil {
		return nil
	}

	var kids []TreeNode
	for _, e := range entries {
		// Skip summaries and meta entries
		if strings.HasPrefix(e.ID, "summary:") || strings.HasPrefix(e.ID, "meta:") {
			continue
		}
		// Check if entry belongs to this track (via track: or track_auto: tag)
		if !entryBelongsToTrack(e.Tags, track) {
			continue
		}
		node := TreeNode{
			ID:    e.ID,
			Level: "entry",
			Tags:  e.Tags,
		}
		kids = append(kids, node)
	}
	return kids
}

// getRawEntriesForDay finds raw entries for a day when no 3h summaries exist.
func (s *Server) getRawEntriesForDay(ctx context.Context, track, dateStr string) []TreeNode {
	dateTag := "date:" + dateStr
	trackTag := "track:" + track

	entries, err := s.store.ByTags(ctx, []string{dateTag, trackTag}, 200, 0)
	if err != nil || len(entries) == 0 {
		// Try track_auto: tag
		trackAutoTag := "track_auto:" + track
		entries, err = s.store.ByTags(ctx, []string{dateTag, trackAutoTag}, 200, 0)
		if err != nil {
			return nil
		}
	}

	var kids []TreeNode
	for _, e := range entries {
		if strings.HasPrefix(e.ID, "summary:") || strings.HasPrefix(e.ID, "meta:") {
			continue
		}
		node := TreeNode{
			ID:    e.ID,
			Level: "entry",
			Tags:  e.Tags,
		}
		kids = append(kids, node)
	}
	return kids
}

// collectLeaves recursively collects all leaf-level entry IDs.
func (s *Server) collectLeaves(ctx context.Context, level, track, date string) []string {
	switch level {
	case "weekly":
		return s.collectWeeklyLeaves(ctx, track, date)
	case "daily":
		return s.collectDailyLeaves(ctx, track, date)
	case "3h":
		return s.collect3hLeaves(ctx, track, date)
	default:
		return nil
	}
}

func (s *Server) collectWeeklyLeaves(ctx context.Context, track, weekStart string) []string {
	t, err := time.Parse("2006-01-02", weekStart)
	if err != nil {
		return nil
	}
	weekEnd := t.AddDate(0, 0, 7)

	summaryTag := "summary:track:" + track
	entries, err := s.store.ByTags(ctx, []string{summaryTag, "summary:daily"}, 100, 0)
	if err != nil || len(entries) == 0 {
		summaryTag = "summary:daily:" + track
		entries, _ = s.store.ByTags(ctx, []string{summaryTag}, 100, 0)
	}

	var leaves []string
	for _, e := range entries {
		if e.Timestamp.Before(t) || !e.Timestamp.Before(weekEnd) {
			continue
		}
		dateStr := e.Timestamp.Format("2006-01-02")
		dayLeaves := s.collectDailyLeaves(ctx, track, dateStr)
		leaves = append(leaves, dayLeaves...)
	}
	return leaves
}

func (s *Server) collectDailyLeaves(ctx context.Context, track, dateStr string) []string {
	summaryTag := "summary:3h:" + track
	dateTag := "date:" + dateStr

	entries, err := s.store.ByTags(ctx, []string{summaryTag, dateTag}, 100, 0)
	if err != nil || len(entries) == 0 {
		// No 3h summaries — raw entries are the leaves
		return s.rawEntryIDsForDay(ctx, track, dateStr)
	}

	var leaves []string
	for _, e := range entries {
		_, _, hourDate := parseSummaryID(e.ID)
		hourLeaves := s.collect3hLeaves(ctx, track, hourDate)
		leaves = append(leaves, hourLeaves...)
	}
	return leaves
}

func (s *Server) collect3hLeaves(ctx context.Context, track, dateHour string) []string {
	t, err := time.Parse("2006-01-02-15", dateHour)
	if err != nil {
		t, err = time.Parse("2006-01-02", dateHour)
		if err != nil {
			return nil
		}
	}
	windowEnd := t.Add(3 * time.Hour)

	entries, err := s.store.EntriesByTimeRange(ctx, t.Unix(), windowEnd.Unix(), nil, 200)
	if err != nil {
		return nil
	}

	var ids []string
	for _, e := range entries {
		if strings.HasPrefix(e.ID, "summary:") || strings.HasPrefix(e.ID, "meta:") {
			continue
		}
		if !entryBelongsToTrack(e.Tags, track) {
			continue
		}
		ids = append(ids, e.ID)
	}
	return ids
}

func (s *Server) rawEntryIDsForDay(ctx context.Context, track, dateStr string) []string {
	dateTag := "date:" + dateStr
	trackTag := "track:" + track

	entries, err := s.store.ByTags(ctx, []string{dateTag, trackTag}, 200, 0)
	if err != nil || len(entries) == 0 {
		trackAutoTag := "track_auto:" + track
		entries, _ = s.store.ByTags(ctx, []string{dateTag, trackAutoTag}, 200, 0)
	}

	var ids []string
	for _, e := range entries {
		if strings.HasPrefix(e.ID, "summary:") || strings.HasPrefix(e.ID, "meta:") {
			continue
		}
		ids = append(ids, e.ID)
	}
	return ids
}

// entryBelongsToTrack checks if any tag matches track:<name> or track_auto:<name>.
func entryBelongsToTrack(tags []string, track string) bool {
	trackTag := "track:" + track
	trackAutoTag := "track_auto:" + track
	for _, t := range tags {
		if t == trackTag || t == trackAutoTag {
			return true
		}
	}
	return false
}
