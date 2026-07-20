// hippocampus-ingest is a CLI tool for ingesting web pages into Hippocampus memory.
//
// Usage:
//
//	hippocampus-ingest <url> [--tags tag1,tag2,...] [--title "Custom Title"]
//
// It fetches the URL, extracts readable content, scans for prompt injection,
// chunks if needed, and stores in the two-stage model (stub + content entries).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/pkg/ingest"
)

func main() {
	var (
		tagsStr  string
		title    string
		verbose  bool
		dryRun   bool
		maxChunk int
	)

	flag.StringVar(&tagsStr, "tags", "", "Comma-separated tags to apply (in addition to automatic source:web, url:*, domain:* tags)")
	flag.StringVar(&title, "title", "", "Override extracted title")
	flag.BoolVar(&verbose, "v", false, "Verbose output")
	flag.BoolVar(&dryRun, "dry-run", false, "Extract and scan but don't store (useful for testing)")
	flag.IntVar(&maxChunk, "max-chunk", 3000, "Maximum chunk size in characters")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "hippocampus-ingest v%s\n\nUsage: hippocampus-ingest <url> [--tags tag1,tag2] [--title \"Title\"] [--dry-run] [-v]\n", config.Version)
		os.Exit(1)
	}

	rawURL := flag.Arg(0)

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
	store, err := memory.NewRedisStore(cfg.Redis, nil) // No embedder needed for ingest
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to Redis: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()

	// Configure options
	opts := ingest.DefaultOptions()
	opts.Tags = tags
	opts.ChunkOpts.MaxChunkSize = maxChunk

	if verbose {
		fmt.Printf("Ingesting: %s\n", rawURL)
		fmt.Printf("Tags: %v\n", tags)
	}

	if dryRun {
		fmt.Printf("DRY RUN — extracting and scanning only, not storing\n\n")
		// For dry run, we'd need to split the pipeline. For now, just run it fully
		// but print what would happen. TODO: expose extract+scan as separate step.
		fmt.Printf("(Full dry-run mode not yet implemented — use -v for verbose output)\n")
		os.Exit(0)
	}

	// Run the pipeline
	result, err := ingest.Pipeline(ctx, store, rawURL, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ingestion failed: %v\n", err)
		if result != nil && result.SafetyResult.RiskScore > 0 {
			fmt.Fprintf(os.Stderr, "Safety scan: risk_score=%.2f, %d flags\n",
				result.SafetyResult.RiskScore, len(result.SafetyResult.Flags))
			for _, f := range result.SafetyResult.Flags {
				fmt.Fprintf(os.Stderr, "  [%s] %s (offset %d)\n", f.Severity, f.Description, f.Offset)
			}
		}
		os.Exit(1)
	}

	// Output
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
		if verbose {
			for _, f := range result.SafetyResult.Flags {
				fmt.Printf("    [%s] %s (offset %d)\n", f.Severity, f.Description, f.Offset)
			}
		}
	} else {
		fmt.Printf("  Safety:     clean ✓\n")
	}

	if len(result.Warnings) > 0 {
		fmt.Printf("  Warnings:\n")
		for _, w := range result.Warnings {
			fmt.Printf("    - %s\n", w)
		}
	}
}
