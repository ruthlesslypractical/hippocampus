// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// hippocampus-ingest ingests content into Hippocampus memory.
//
// Modes:
//   - URL mode (default): hippocampus-ingest <url> [--tags ...] [--title ...]
//   - Session mode: hippocampus-ingest --session <path> [--tags ...] [--track ...]
//
// Session mode reads Kiro .jsonl session files and stores each prompt/response
// as a memory entry with appropriate tags and timestamps.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/pkg/ingest"
)

func main() {
	var (
		tagsStr     string
		title       string
		verbose     bool
		dryRun      bool
		maxChunk    int
		sessionPath string
		track       string
	)

	flag.StringVar(&tagsStr, "tags", "", "Comma-separated tags to apply")
	flag.StringVar(&title, "title", "", "Override extracted title (URL mode only)")
	flag.BoolVar(&verbose, "v", false, "Verbose output")
	flag.BoolVar(&dryRun, "dry-run", false, "Show what would be ingested without writing")
	flag.IntVar(&maxChunk, "max-chunk", 3000, "Maximum chunk size in characters (URL mode only)")
	flag.StringVar(&sessionPath, "session", "", "Path to .jsonl session file or directory of session files")
	flag.StringVar(&track, "track", "", "Track tag to apply to all ingested entries (session mode)")
	flag.Parse()

	// Parse tags
	var tags []string
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	// Load config
	configPath := config.FindConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Connect to Redis
	store, err := memory.NewRedisStore(cfg.Redis, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to Redis: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()

	// Dispatch by mode
	if sessionPath != "" {
		// Session JSONL mode
		runSessionIngest(ctx, store, sessionPath, track, tags, verbose, dryRun)
	} else {
		// URL mode (original behavior)
		if flag.NArg() < 1 {
			fmt.Fprintf(os.Stderr, "hippocampus-ingest v%s\n\n", config.Version)
			fmt.Fprintf(os.Stderr, "Usage:\n")
			fmt.Fprintf(os.Stderr, "  URL mode:     hippocampus-ingest <url> [--tags tag1,tag2] [--title \"Title\"] [--dry-run] [-v]\n")
			fmt.Fprintf(os.Stderr, "  Session mode: hippocampus-ingest --session <path.jsonl|dir> [--tags tag1,tag2] [--track Name] [--dry-run] [-v]\n")
			os.Exit(1)
		}
		runURLIngest(ctx, store, flag.Arg(0), tags, title, verbose, dryRun, maxChunk, cfg)
	}
}

// ───────────────────────────────────────────────────────────────────
// URL Ingest (original behavior)
// ───────────────────────────────────────────────────────────────────

func runURLIngest(ctx context.Context, store *memory.RedisStore, rawURL string, tags []string, title string, verbose, dryRun bool, maxChunk int, cfg config.Config) {
	if verbose {
		fmt.Printf("Ingesting: %s\n", rawURL)
		fmt.Printf("Tags: %v\n", tags)
	}

	if dryRun {
		fmt.Printf("DRY RUN — extracting and scanning only, not storing\n")
		os.Exit(0)
	}

	opts := ingest.DefaultOptions()
	opts.Tags = tags
	opts.ChunkOpts.MaxChunkSize = maxChunk

	result, err := ingest.Pipeline(ctx, store, rawURL, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ingestion failed: %v\n", err)
		if result != nil && result.SafetyResult.RiskScore > 0 {
			fmt.Fprintf(os.Stderr, "Safety scan: risk_score=%.2f, %d flags\n",
				result.SafetyResult.RiskScore, len(result.SafetyResult.Flags))
		}
		os.Exit(1)
	}

	fmt.Printf("✓ Ingested: \"%s\"\n", result.Title)
	fmt.Printf("  URL:        %s\n", result.URL)
	fmt.Printf("  Words:      %d\n", result.WordCount)
	fmt.Printf("  Chunks:     %d\n", result.ChunkCount)
	fmt.Printf("  Stub ID:    %s\n", result.StubID)

	if verbose {
		fmt.Printf("  Content IDs:\n")
		for _, id := range result.ContentIDs {
			fmt.Printf("    - %s\n", id)
		}
	}

	if result.SafetyResult.RiskScore > 0 {
		fmt.Printf("  Safety:     risk_score=%.2f (%d flags)\n",
			result.SafetyResult.RiskScore, len(result.SafetyResult.Flags))
	} else {
		fmt.Printf("  Safety:     clean ✓\n")
	}
}

// ───────────────────────────────────────────────────────────────────
// Session JSONL Ingest
// ───────────────────────────────────────────────────────────────────

// sessionLogEntry represents a single line from a Kiro .jsonl session file.
type sessionLogEntry struct {
	Version string         `json:"version"`
	Kind    string         `json:"kind"`
	Data    sessionLogData `json:"data"`
}

type sessionLogData struct {
	MessageID string         `json:"message_id"`
	Content   []contentBlock `json:"content"`
	Meta      *sessionMeta   `json:"meta,omitempty"`
}

type contentBlock struct {
	Kind string `json:"kind"`
	Data string `json:"data"`
}

type sessionMeta struct {
	Timestamp int64 `json:"timestamp"`
}

// sessionMetadata is parsed from the .json sidecar file for each session.
type sessionMetadata struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	CreatedAt string `json:"created_at"`
	Title     string `json:"title"`
}

func runSessionIngest(ctx context.Context, store *memory.RedisStore, path, track string, baseTags []string, verbose, dryRun bool) {
	// Collect session files
	var files []string
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if info.IsDir() {
		matches, _ := filepath.Glob(filepath.Join(path, "*.jsonl"))
		files = matches
	} else {
		files = []string{path}
	}

	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "No .jsonl session files found at %s\n", path)
		os.Exit(1)
	}

	fmt.Printf("Found %d session file(s) to ingest\n", len(files))

	// Build base tags
	if track != "" {
		baseTags = append(baseTags, "track:"+track)
	}

	if dryRun {
		for _, f := range files {
			count := countSessionEntries(f)
			fmt.Printf("  %s (%d entries)\n", filepath.Base(f), count)
		}
		return
	}

	totalIngested := 0
	for _, file := range files {
		n, err := ingestSessionFile(ctx, store, file, baseTags, verbose)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error ingesting %s: %v\n", filepath.Base(file), err)
			continue
		}
		totalIngested += n
		fmt.Printf("  ✓ %s: %d entries\n", filepath.Base(file), n)
	}

	fmt.Printf("\nTotal: %d entries ingested\n", totalIngested)
}

func ingestSessionFile(ctx context.Context, store *memory.RedisStore, filePath string, baseTags []string, verbose bool) (int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")

	// Try to load session metadata from sidecar .json
	var meta *sessionMetadata
	metaPath := strings.TrimSuffix(filePath, ".jsonl") + ".json"
	if metaData, err := os.ReadFile(metaPath); err == nil {
		var m sessionMetadata
		if json.Unmarshal(metaData, &m) == nil {
			meta = &m
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var logEntry sessionLogEntry
		if err := json.Unmarshal(line, &logEntry); err != nil {
			continue
		}

		// Only ingest user prompts and assistant messages
		if logEntry.Kind != "Prompt" && logEntry.Kind != "AssistantMessage" {
			continue
		}

		// Extract text content
		var content strings.Builder
		for _, block := range logEntry.Data.Content {
			if block.Kind == "text" {
				content.WriteString(block.Data)
			}
		}
		if content.Len() == 0 {
			continue
		}

		// Build tags
		entryTags := make([]string, len(baseTags))
		copy(entryTags, baseTags)
		entryTags = append(entryTags, "session:"+sessionID)

		switch logEntry.Kind {
		case "Prompt":
			entryTags = append(entryTags, "kind:prompt")
		case "AssistantMessage":
			entryTags = append(entryTags, "kind:assistantmessage")
		}

		if meta != nil && meta.CWD != "" {
			entryTags = append(entryTags, "cwd:"+meta.CWD)
		}

		// Determine timestamp
		var ts time.Time
		if logEntry.Data.Meta != nil && logEntry.Data.Meta.Timestamp > 0 {
			ts = time.Unix(logEntry.Data.Meta.Timestamp, 0)
		} else if meta != nil && meta.CreatedAt != "" {
			ts, _ = time.Parse(time.RFC3339, meta.CreatedAt)
		} else {
			ts = time.Now()
		}

		// Add date tag
		entryTags = append(entryTags, "date:"+ts.Format("2006-01-02"))

		entryID := fmt.Sprintf("%s:%s", sessionID, logEntry.Data.MessageID)

		entry := memory.Entry{
			ID:        entryID,
			Timestamp: ts,
			Content:   content.String(),
			Tags:      entryTags,
		}

		if err := store.Put(ctx, entry); err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "    error storing %s: %v\n", entryID, err)
			}
			continue
		}
		count++

		if verbose && count%50 == 0 {
			fmt.Printf("    ...%d entries\n", count)
		}
	}

	return count, scanner.Err()
}

func countSessionEntries(filePath string) int {
	f, err := os.Open(filePath)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	count := 0
	for scanner.Scan() {
		var entry sessionLogEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			if entry.Kind == "Prompt" || entry.Kind == "AssistantMessage" {
				count++
			}
		}
	}
	return count
}
