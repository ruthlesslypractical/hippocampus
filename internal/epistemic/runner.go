// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
)

// Runner orchestrates the epistemic extraction pipeline.
type Runner struct {
	rdb       *redis.Client
	extractor *Extractor
	registry  *Registry
	vocab     *Vocab
	cfg       config.EpistemicConfig
	dryRun    bool
}

// NewRunner creates a new epistemic pipeline runner.
func NewRunner(rdb *redis.Client, ollamaClient *ollama.Client, cfg config.EpistemicConfig, dryRun bool) *Runner {
	return &Runner{
		rdb:       rdb,
		extractor: NewExtractor(ollamaClient, cfg),
		registry:  NewRegistry(rdb),
		vocab:     NewVocab(rdb),
		cfg:       cfg,
		dryRun:    dryRun,
	}
}

// RunExtraction processes entries from the timeline, extracting epistemic triples.
// It works BACKWARDS in time (newest first), processing entries tagged as
// assistant responses that haven't been fact-checked yet.
// If force is true, it re-processes entries even if already checked.
func (r *Runner) RunExtraction(ctx context.Context, limit int, force bool) error {
	slog.Info("Epistemic extraction: scanning entries", "limit", limit, "force", force)

	// Get entries from timeline, newest first
	entries, err := r.getUncheckedEntries(ctx, limit, force)
	if err != nil {
		return fmt.Errorf("get entries: %w", err)
	}

	if len(entries) == 0 {
		slog.Info("No entries to process")
		return nil
	}

	slog.Info("Found entries to process", "count", len(entries))

	processed := 0
	totalTriples := 0

	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}

		// Skip very short entries (< MinEntryLen chars — probably just "OK" or "Done")
		if len(entry.content) < r.cfg.MinEntryLen {
			continue
		}

		// Get relevant vocabulary for reconciliation
		keywords := r.extractKeywords(entry.content)
		vocab, _ := r.vocab.GetRelevantTerms(ctx, keywords, r.cfg.MaxVocabTerms)

		// Extract triples
		triples, err := r.extractor.Extract(ctx, entry.content, vocab)
		if err != nil {
			slog.Warn("Extraction failed", "entry_id", entry.id, "error", err)
			continue
		}

		if len(triples) == 0 {
			continue
		}

		if r.dryRun {
			slog.Info("Dry run extraction", "entry_id", entry.id, "triples_count", len(triples))
			for _, t := range triples {
				domain := ClassifyDomain(t, r.cfg.VagueMaxLen)
				marker := ""
				if !IsVerifiable(domain) {
					marker = " [SKIP:" + string(domain) + "]"
				}
				slog.Info("Dry run triple", "canonical", t.Canonical(), "subject", t.Subject, "object", t.Object, "type", t.Type, "skip_domain", marker)
			}
		} else {
			// Record each triple in the registry (only verifiable claims)
			for _, triple := range triples {
				domain := ClassifyDomain(triple, r.cfg.VagueMaxLen)
				if !IsVerifiable(domain) {
					// Opinion/definitional claims: don't fact-check, just skip.
					continue
				}

				isNew, err := r.registry.Record(ctx, triple, entry.id)
				if err != nil {
					slog.Warn("Registry write failed", "error", err)
					continue
				}
				if isNew {
					slog.Info("New triple recorded", "canonical", triple.Canonical())
				}
			}

			// Record vocabulary terms
			r.vocab.RecordTerms(ctx, triples)

			// Mark entry as fact-checked
			r.rdb.SAdd(ctx, "epistemic:checked", entry.id)
		}

		processed++
		totalTriples += len(triples)

		if processed%10 == 0 {
			slog.Info("Extraction progress", "processed", processed, "total", len(entries), "triples", totalTriples)
		}
	}

	slog.Info("Epistemic extraction complete", "entries_processed", processed, "triples_extracted", totalTriples)

	// Print stats
	stats, _ := r.registry.Stats(ctx)
	vocabSize, _ := r.vocab.Size(ctx)
	slog.Info("Registry stats",
		"unknown", stats[StatusUnknown],
		"verified", stats[StatusVerified],
		"contested", stats[StatusContested],
		"false", stats[StatusFalse],
		"vocab_terms", vocabSize,
	)

	return nil
}

// epistemic entry from timeline
type timelineEntry struct {
	id        string
	content   string
	tags      []string
	timestamp int64
}

// getUncheckedEntries returns assistant response entries that haven't been
// fact-checked yet, sorted newest-first (ZREVRANGEBYSCORE on timeline).
func (r *Runner) getUncheckedEntries(ctx context.Context, limit int, force bool) ([]timelineEntry, error) {
	// Get entries from timeline, newest first
	results, err := r.rdb.ZRevRangeByScoreWithScores(ctx, "timeline", &redis.ZRangeBy{
		Min:   "-inf",
		Max:   "+inf",
		Count: int64(limit * 3), // over-fetch to account for filtering
	}).Result()
	if err != nil {
		return nil, err
	}

	// Set of already-checked entry IDs
	var checked map[string]bool
	if !force {
		checkedMembers, _ := r.rdb.SMembers(ctx, "epistemic:checked").Result()
		checked = make(map[string]bool, len(checkedMembers))
		for _, id := range checkedMembers {
			checked[id] = true
		}
	}

	var entries []timelineEntry
	for _, z := range results {
		id, ok := z.Member.(string)
		if !ok {
			continue
		}

		// Skip if already checked (unless force)
		if !force && checked[id] {
			continue
		}

		// Get entry content and tags
		vals, err := r.rdb.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(vals) == 0 {
			// Try without prefix (some entries use bare ID as key)
			vals, err = r.rdb.HGetAll(ctx, id).Result()
			if err != nil || len(vals) == 0 {
				continue
			}
		}

		content := vals["content"]
		tags := strings.Split(vals["tags"], ",")

		// Only process assistant responses (where the model made claims)
		isAssistantResponse := false
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if tag == "kind:assistant_response" || tag == "kind:assistantmessage" {
				isAssistantResponse = true
				break
			}
		}
		if !isAssistantResponse {
			continue
		}

		// Skip summaries and meta entries
		isMeta := false
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if strings.HasPrefix(tag, "summary:") || strings.HasPrefix(tag, "meta") {
				isMeta = true
				break
			}
		}
		if isMeta {
			continue
		}

		entries = append(entries, timelineEntry{
			id:        id,
			content:   content,
			tags:      tags,
			timestamp: int64(z.Score),
		})

		if len(entries) >= limit {
			break
		}
	}

	return entries, nil
}

// extractKeywords pulls simple keywords from text for vocabulary lookup.
// This is a fast heuristic — no LLM call needed.
func (r *Runner) extractKeywords(text string) []string {
	// Split on whitespace and punctuation, filter short words
	words := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || 
		       r == '.' || r == ':' || r == ';' || r == '(' || 
		       r == ')' || r == '[' || r == ']' || r == '{' || 
		       r == '}' || r == '"' || r == '\'' || r == '`' ||
		       r == '!' || r == '?'
	})

	seen := make(map[string]bool)
	var keywords []string

	// Common stopwords to skip
	stopwords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "shall": true, "can": true, "need": true,
		"that": true, "this": true, "these": true, "those": true,
		"it": true, "its": true, "of": true, "in": true, "on": true,
		"at": true, "to": true, "for": true, "with": true, "from": true,
		"by": true, "as": true, "or": true, "and": true, "but": true,
		"not": true, "no": true, "if": true, "then": true, "than": true,
		"so": true, "up": true, "out": true, "about": true, "into": true,
		"through": true, "during": true, "before": true, "after": true,
		"above": true, "below": true, "between": true, "under": true,
		"again": true, "further": true, "once": true, "here": true,
		"there": true, "when": true, "where": true, "why": true, "how": true,
		"all": true, "each": true, "every": true, "both": true, "few": true,
		"more": true, "most": true, "other": true, "some": true, "such": true,
		"only": true, "own": true, "same": true, "also": true, "very": true,
		"just": true, "because": true, "which": true, "while": true,
	}

	for _, w := range words {
		w = strings.ToLower(w)
		// Skip short, stopwords, or already seen
		if len(w) < r.cfg.MinKeywordLen || stopwords[w] || seen[w] {
			continue
		}
		// Skip pure numbers
		isNum := true
		for _, ch := range w {
			if ch < '0' || ch > '9' {
				isNum = false
				break
			}
		}
		if isNum {
			continue
		}
		seen[w] = true
		keywords = append(keywords, w)
	}

	// Cap at MaxKeywords
	if len(keywords) > r.cfg.MaxKeywords {
		keywords = keywords[:r.cfg.MaxKeywords]
	}

	return keywords
}

// FormatWarning formats an epistemic registry entry as a recall-time warning string.
func FormatWarning(entry RegistryEntry) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚠️ EPISTEMIC WARNING: \"%s|%s|%s\" is %s.\n",
		entry.Subject, entry.Verb, entry.Object, strings.ToUpper(string(entry.Status))))
	if entry.EvidenceFor != "" {
		sb.WriteString(fmt.Sprintf("  Evidence for: %s\n", util.Truncate(entry.EvidenceFor, 100)))
	}
	if entry.EvidenceAgainst != "" {
		sb.WriteString(fmt.Sprintf("  Evidence against: %s\n", util.Truncate(entry.EvidenceAgainst, 100)))
	}
	sb.WriteString(fmt.Sprintf("  Seen %d times since %s.\n",
		entry.EncounterCount, entry.FirstSeen.Format("2006-01-02")))
	return sb.String()
}


