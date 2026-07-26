// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/pkg/classify"
)

func (s *Server) getClassifyDeps() (*memory.RedisStore, *ollama.Client, error) {
	rs, ok := s.store.(*memory.RedisStore)
	if !ok {
		return nil, nil, fmt.Errorf("classify tools require RedisStore (not available in this mode)")
	}
	ollamaClient := ollama.New(s.cfg.Ollama.BaseURL, s.cfg.Ollama.Model, s.cfg.Ollama.TimeoutMinutes)
	return rs, ollamaClient, nil
}

func (s *Server) toolClassify(ctx context.Context, args map[string]interface{}) (CallToolResult, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return CallToolResult{}, fmt.Errorf("id is required")
	}
	force, _ := args["force"].(bool)
	dryRun, _ := args["dry_run"].(bool)

	rs, ollamaClient, err := s.getClassifyDeps()
	if err != nil {
		return CallToolResult{}, err
	}

	opts := classify.DefaultOptions()
	opts.Force = force
	opts.DryRun = dryRun

	result, err := classify.ClassifySingle(ctx, rs.Client(), ollamaClient, id, opts)
	if err != nil {
		return CallToolResult{}, err
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return CallToolResult{Content: []ContentBlock{{Type: "text", Text: string(data)}}}, nil
}

func (s *Server) toolClassifyRange(ctx context.Context, args map[string]interface{}) (CallToolResult, error) {
	rs, ollamaClient, err := s.getClassifyDeps()
	if err != nil {
		return CallToolResult{}, err
	}

	force, _ := args["force"].(bool)
	dryRun, _ := args["dry_run"].(bool)
	session, _ := args["session"].(string)

	opts := classify.DefaultOptions()
	opts.Force = force
	opts.DryRun = dryRun

	client := rs.Client()

	var entryIDs []string

	if session != "" {
		// Get all entries in this session
		ids, err := client.SMembers(ctx, "tag:session:"+session).Result()
		if err != nil {
			return CallToolResult{}, fmt.Errorf("session not found: %w", err)
		}
		entryIDs = ids
	} else {
		// Use time range
		startF, _ := args["start"].(float64)
		endF, _ := args["end"].(float64)
		if startF == 0 || endF == 0 {
			return CallToolResult{}, fmt.Errorf("start and end timestamps required (or provide session)")
		}

		ids, err := client.ZRangeByScore(ctx, "timeline", &redis.ZRangeBy{
			Min: fmt.Sprintf("%d", int64(startF)),
			Max: fmt.Sprintf("%d", int64(endF)),
		}).Result()
		if err != nil {
			return CallToolResult{}, err
		}
		entryIDs = ids
	}

	// Filter to unclassified unless force
	var toClassify []classify.Entry
	for _, id := range entryIDs {
		entry, err := client.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(entry) == 0 {
			continue
		}

		tags := splitTags(entry["tags"])

		// Skip if already classified (unless force)
		if !force && hasTag(tags, "classified") {
			continue
		}

		var ts int64
		fmt.Sscanf(entry["timestamp"], "%d", &ts)
		toClassify = append(toClassify, classify.Entry{
			ID:        id,
			Content:   entry["content"],
			Tags:      tags,
			Timestamp: ts,
		})
	}

	if len(toClassify) == 0 {
		return CallToolResult{Content: []ContentBlock{{Type: "text", Text: "No entries to classify in range."}}}, nil
	}

	results, err := classify.ClassifyEntries(ctx, client, ollamaClient, toClassify, opts)
	if err != nil {
		return CallToolResult{}, err
	}

	// Summary
	confused := 0
	for _, r := range results {
		if r.Confused {
			confused++
		}
	}

	summary := fmt.Sprintf("Classified %d entries. %d flagged as confused.", len(results), confused)
	if dryRun {
		summary = "[DRY RUN] Would classify " + fmt.Sprintf("%d entries. %d uncertain.", len(results), confused)
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"summary": summary,
		"results": results,
	}, "", "  ")
	return CallToolResult{Content: []ContentBlock{{Type: "text", Text: string(data)}}}, nil
}

func (s *Server) toolReclassify(ctx context.Context, args map[string]interface{}) (CallToolResult, error) {
	rs, ollamaClient, err := s.getClassifyDeps()
	if err != nil {
		return CallToolResult{}, err
	}

	dryRun, _ := args["dry_run"].(bool)
	tagFilter, _ := args["tag"].(string)

	client := rs.Client()

	var entryIDs []string

	if tagFilter != "" {
		// Get entries with this specific tag
		ids, err := client.SMembers(ctx, "tag:"+tagFilter).Result()
		if err != nil {
			return CallToolResult{}, fmt.Errorf("tag not found: %w", err)
		}
		entryIDs = ids
	} else {
		// Use time range
		startF, _ := args["start"].(float64)
		endF, _ := args["end"].(float64)
		if startF == 0 || endF == 0 {
			return CallToolResult{}, fmt.Errorf("start and end timestamps required (or provide tag filter)")
		}

		ids, err := client.ZRangeByScore(ctx, "timeline", &redis.ZRangeBy{
			Min: fmt.Sprintf("%d", int64(startF)),
			Max: fmt.Sprintf("%d", int64(endF)),
		}).Result()
		if err != nil {
			return CallToolResult{}, err
		}
		entryIDs = ids
	}

	opts := classify.DefaultOptions()
	opts.DryRun = dryRun

	results, err := classify.Reclassify(ctx, client, ollamaClient, entryIDs, opts)
	if err != nil {
		return CallToolResult{}, err
	}

	confused := 0
	for _, r := range results {
		if r.Confused {
			confused++
		}
	}

	summary := fmt.Sprintf("Reclassified %d entries. %d flagged as confused.", len(results), confused)
	if dryRun {
		summary = "[DRY RUN] Would reclassify " + fmt.Sprintf("%d entries. %d uncertain.", len(results), confused)
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"summary": summary,
		"results": results,
	}, "", "  ")
	return CallToolResult{Content: []ContentBlock{{Type: "text", Text: string(data)}}}, nil
}

// helpers

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
