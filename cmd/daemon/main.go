// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/epistemic"
	"github.com/ruthlesslypractical/hippocampus/internal/logging"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
)

// ollamaClients holds pre-created Ollama clients for each subsystem.
// Separate clients allow routing to different servers for load distribution.
type ollamaClients struct {
	Classifier *ollama.Client
	Extractor  *ollama.Client
	Verifier   *ollama.Client
	Linker     *ollama.Client
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("hippocampus-daemon v%s\n", config.Version)
		return
	}

	cfgPath := config.FindConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Guard against empty queue key (can happen if config was saved before this field existed)
	if cfg.Daemon.QueueKey == "" {
		cfg.Daemon.QueueKey = "ingest:queue"
	}

	cleanupLog := logging.Setup(cfg, "daemon")
	defer cleanupLog()

	slog.Info("daemon starting", "config", cfgPath)
	slog.Info("daemon config",
		"gpu_concurrency", cfg.Daemon.GPUConcurrency,
		"queue", cfg.Daemon.QueueKey,
		"classifier", cfg.Daemon.Classifier.Enabled,
		"extractor", cfg.Daemon.Extractor.Enabled,
		"verifier", cfg.Daemon.Verifier.Enabled,
		"linker", cfg.Daemon.Linker.Enabled,
		"condenser", cfg.Daemon.Condenser.Enabled,
	)

	// Log Ollama endpoint resolution
	classifyURL, classifyModel := cfg.ResolveOllama(cfg.Daemon.Classifier.Ollama)
	extractURL, extractModel := cfg.ResolveOllama(cfg.Daemon.Extractor.Ollama)
	verifyURL, verifyModel := cfg.ResolveOllama(cfg.Daemon.Verifier.Ollama)
	linkerURL, linkerModel := cfg.ResolveOllama(cfg.Daemon.Linker.Ollama)
	slog.Debug("ollama endpoints", "classifier", classifyURL+"/"+classifyModel, "extractor", extractURL+"/"+extractModel, "verifier", verifyURL+"/"+verifyModel, "linker", linkerURL+"/"+linkerModel)

	clients := &ollamaClients{
		Classifier: ollama.New(classifyURL, classifyModel, cfg.Ollama.TimeoutMinutes),
		Extractor:  ollama.New(extractURL, extractModel, cfg.Ollama.TimeoutMinutes),
		Verifier:   ollama.New(verifyURL, verifyModel, cfg.Ollama.TimeoutMinutes),
		Linker:     ollama.New(linkerURL, linkerModel, cfg.Ollama.TimeoutMinutes),
	}

	// Connect to Redis
	redactedPass := ""
	if cfg.Redis.Password != "" {
		if len(cfg.Redis.Password) > 4 {
			redactedPass = cfg.Redis.Password[:4] + "..."
		} else {
			redactedPass = "****"
		}
	}
	slog.Debug("redis connecting", "addr", cfg.Redis.Addr, "password", redactedPass)

	rdb := cfg.Redis.NewRedisClient()
	defer rdb.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis connection failed", "err", err)
		os.Exit(1)
	}
	slog.Info("redis connected", "addr", cfg.Redis.Addr)

	// Config reload function
	reloadConfig := func() config.DaemonConfig {
		fresh, err := config.Load(cfgPath)
		if err != nil {
			slog.Warn("config reload failed, using cached", "err", err)
			return cfg.Daemon
		}
		return fresh.Daemon
	}

	// Launch priority dispatcher
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDispatcher(ctx, rdb, &cfg, reloadConfig, clients)
		// If dispatcher returns (e.g., binary self-update), cancel context
		// so main() unblocks and the process exits cleanly.
		cancel()
	}()

	// Wait for shutdown signal OR dispatcher self-exit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
		cancel()
	case <-ctx.Done():
		// Dispatcher exited on its own (self-update)
		slog.Info("dispatcher exited, shutting down")
	}
	wg.Wait()
	slog.Info("daemon stopped")
}

// --- Priority Dispatcher ---

type jobType int

const (
	jobLiveIngest  jobType = iota // Priority 1: from ingest:queue
	jobVerify                     // Priority 2: triple crossed encounter threshold
	jobBacklog                    // Priority 3: historical entries (finite)
	jobCondense                  // Priority 4: per-entry condensation (finite backfill, then ongoing)
	jobConsolidate                // Priority 5: pairwise relevance (infinite idle loop)
)

func (j jobType) String() string {
	switch j {
	case jobLiveIngest:
		return "live"
	case jobVerify:
		return "verify"
	case jobBacklog:
		return "backlog"
	case jobCondense:
		return "condense"
	case jobConsolidate:
		return "consolidate"
	default:
		return "unknown"
	}
}

// runDispatcher is the unified priority scheduler.
// It fills GPU slots with the highest-priority available work.
func runDispatcher(ctx context.Context, rdb *redis.Client, cfg *config.Config, reloadConfig func() config.DaemonConfig, clients *ollamaClients) {
	// One-time migration: convert old ZSET links (link:<id>) to new HASH format (links:<id>)
	migrateLegacyLinks(ctx, rdb)

	// Self-update detection: record our binary's mod time at startup.
	// If the binary on disk changes (app update, make install, etc.), exit cleanly.
	// Launchd's KeepAlive will restart us with the new binary.
	selfPath, _ := os.Executable()
	var startMtime time.Time
	if stat, err := os.Stat(selfPath); err == nil {
		startMtime = stat.ModTime()
	}
	lastUpdateCheck := time.Now()

	concurrency := cfg.Daemon.GPUConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	slog.Info("dispatcher started", "gpu_concurrency", concurrency)

	backlogCursor := "+inf"
	backlogDone := false

	// Exponential backoff for Ollama failures
	var consecutiveFailures atomic.Int32
	failureThreshold := cfg.Daemon.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	maxBackoffS := cfg.Daemon.MaxBackoffS
	if maxBackoffS <= 0 {
		maxBackoffS = 3600
	}
	maxBackoff := time.Duration(maxBackoffS) * time.Second
	backoffBaseS := cfg.Daemon.BackoffBaseS
	if backoffBaseS <= 0 {
		backoffBaseS = 10
	}

	for {
		select {
		case <-ctx.Done():
			// Drain semaphore — wait for in-flight jobs to finish
			for i := 0; i < concurrency; i++ {
				sem <- struct{}{}
			}
			slog.Info("dispatcher stopped")
			return
		default:
		}

		// Self-update check: every N seconds, see if our binary was replaced
		selfUpdateCheckS := cfg.Daemon.SelfUpdateCheckS
		if selfUpdateCheckS <= 0 {
			selfUpdateCheckS = 30
		}
		if time.Since(lastUpdateCheck) > time.Duration(selfUpdateCheckS)*time.Second {
			lastUpdateCheck = time.Now()
			if stat, err := os.Stat(selfPath); err == nil {
				if stat.ModTime().After(startMtime) {
					slog.Info("binary updated on disk, exiting for restart", "old_mtime", startMtime, "new_mtime", stat.ModTime())
					return
				}
			}
		}

		// If Ollama appears to be down, back off before trying again
		if failures := int(consecutiveFailures.Load()); failures >= failureThreshold {
			backoff := time.Duration(1<<(failures-failureThreshold)) * time.Duration(backoffBaseS) * time.Second
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			slog.Warn("ollama appears down, backing off", "consecutive_failures", failures, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		// Acquire a GPU slot
		select {
		case sem <- struct{}{}:
			// Got a slot
		case <-ctx.Done():
			return
		}

		daemonCfg := reloadConfig()
		job, id := pickNextJob(ctx, rdb, cfg, &daemonCfg, &backlogCursor, &backlogDone)

		if job == -1 {
			// Nothing to do — release slot and wait
			<-sem
			idlePollS := cfg.Daemon.IdlePollS
			if idlePollS <= 0 {
				idlePollS = 2
			}
			time.Sleep(time.Duration(idlePollS) * time.Second)
			consecutiveFailures.Store(0) // no work isn't a failure
			continue
		}

		// Dispatch
		go func(jt jobType, entryID string, dc config.DaemonConfig) {
			defer func() { <-sem }()

			var jobErr error
			switch jt {
			case jobLiveIngest:
				slog.Info("processing", "job", jt, "entry_id", entryID)
				jobErr = processEntry(ctx, rdb, cfg, entryID, dc, clients)
				if jobErr != nil {
					slog.Error("job failed", "job", jt, "entry_id", entryID, "err", jobErr)
				}
			case jobVerify:
				slog.Info("verifying", "job", jt, "triple", entryID)
				runInlineVerify(ctx, rdb, cfg, entryID, clients.Verifier)
			case jobBacklog:
				slog.Info("processing", "job", jt, "entry_id", entryID)
				jobErr = processEntry(ctx, rdb, cfg, entryID, dc, clients)
				if jobErr != nil {
					slog.Error("job failed", "job", jt, "entry_id", entryID, "err", jobErr)
				}
				markProcessed(ctx, rdb, entryID)
			case jobCondense:
				slog.Debug("condensing entry", "job", jt, "entry_id", entryID)
				runCondense(ctx, rdb, cfg, entryID, clients.Linker)
			case jobConsolidate:
				slog.Debug("consolidating", "job", jt, "entry_id", entryID)
				runConsolidation(ctx, rdb, cfg, entryID, clients.Linker)
			}

			// Track Ollama health via consecutive failures
			if jobErr != nil && isOllamaError(jobErr) {
				consecutiveFailures.Add(1)
			} else if jobErr == nil {
				// Successful Ollama call — connection is healthy, reset backoff
				consecutiveFailures.Store(0)
			}
			// Non-Ollama errors (parse failures, etc.) don't affect backoff
		}(job, id, daemonCfg)
	}
}

// stripAutoTags removes all daemon-assigned tags from a meta/infrastructure entry.
// Meta entries should only have: meta, orientation, summary:comprehensive, working-set, lean-kernel.
// Everything else (track_auto, classified, classified:auto, track, date, etc.) gets stripped.
func stripAutoTags(ctx context.Context, rdb *redis.Client, entryID string) {
	tagsStr, err := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
	if err != nil {
		return
	}

	tags := strings.Split(tagsStr, ",")
	var keepTags []string
	var removedTags []string

	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		// Keep only meta-related tags
		if tag == "meta" || tag == "orientation" || tag == "working-set" ||
			tag == "lean-kernel" || strings.HasPrefix(tag, "summary:comprehensive") ||
			strings.HasPrefix(tag, "meta:") {
			keepTags = append(keepTags, tag)
		} else {
			removedTags = append(removedTags, tag)
		}
	}

	if len(removedTags) == 0 {
		return // nothing to strip
	}

	// Update entry tags
	rdb.HSet(ctx, "entry:"+entryID, "tags", strings.Join(keepTags, ","))

	// Remove entry from the old tag sets
	for _, tag := range removedTags {
		rdb.SRem(ctx, "tag:"+tag, entryID)
	}

	slog.Info("stripped auto-tags from meta entry", "entry_id", entryID, "removed", len(removedTags))
}

// isOllamaError checks if an error is likely an Ollama connectivity issue
// (as opposed to a data/parse error that backoff won't fix).
func isOllamaError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connect:") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "Ollama returned 5")
}

// pickNextJob selects the highest-priority available work.
func pickNextJob(ctx context.Context, rdb *redis.Client, cfg *config.Config, dc *config.DaemonConfig, backlogCursor *string, backlogDone *bool) (jobType, string) {
	// Priority 1: Live ingest queue
	if dc.Classifier.Enabled || dc.Extractor.Enabled {
		id, err := rdb.RPop(ctx, cfg.Daemon.QueueKey).Result()
		if err == nil && id != "" {
			return jobLiveIngest, id
		}
	}

	// Priority 2: Verification ready
	if dc.Verifier.Enabled {
		hash, err := rdb.SPop(ctx, "epistemic:verify:ready").Result()
		if err == nil && hash != "" {
			return jobVerify, hash
		}
	}

	// Priority 3: Backlog (finite — reaches the beginning)
	if !*backlogDone && (dc.Classifier.Enabled || dc.Extractor.Enabled) {
		batch, newCursor, err := getUnprocessedEntries(ctx, rdb, 1, *backlogCursor)
		if err == nil && len(batch) > 0 {
			*backlogCursor = newCursor
			return jobBacklog, batch[0]
		}
		if err == nil && len(batch) == 0 {
			*backlogDone = true
			slog.Info("backlog exhausted")
		}
	}

	// Priority 4: Per-message condensation (finite backfill, then ongoing for new entries)
	if dc.Condenser.Enabled {
		id := pickCondenseCandidate(ctx, rdb, cfg)
		if id != "" {
			return jobCondense, id
		}
	}

	// Priority 5: Consolidation (infinite idle loop)
	if dc.Linker.Enabled {
		id := pickConsolidationCandidate(ctx, rdb)
		if id != "" {
			return jobConsolidate, id
		}
	}

	return -1, ""
}

// runInlineVerify executes verification on a triple using the verifier subsystem.
func runInlineVerify(ctx context.Context, rdb *redis.Client, cfg *config.Config, hash string, ollamaClient *ollama.Client) {
	verifier := epistemic.NewVerifier(rdb, ollamaClient, cfg.Epistemic, false)
	if err := verifier.RunVerification(ctx, cfg.Epistemic.MinEncounters, 1, true); err != nil {
		slog.Warn("inline verification failed", "triple", hash, "err", err)
	}
}

// pickCondenseCandidate finds an entry that qualifies for per-message condensation
// but doesn't have a summary yet.
func pickCondenseCandidate(ctx context.Context, rdb *redis.Client, cfg *config.Config) string {
	// Pick a random entry from the timeline
	entries, err := rdb.ZRandMember(ctx, "timeline", 5).Result()
	if err != nil || len(entries) == 0 {
		return ""
	}

	cc := cfg.Daemon.Condenser
	minUser := cc.MinUserChars
	if minUser <= 0 {
		minUser = 300
	}
	minAssistant := cc.MinAssistantChars
	if minAssistant <= 0 {
		minAssistant = 1500
	}
	minOther := cc.MinOtherChars
	if minOther <= 0 {
		minOther = 500
	}

	for _, id := range entries {
		// Skip if already has a summary
		hasSummary, _ := rdb.HExists(ctx, "entry:"+id, "summary").Result()
		if hasSummary {
			continue
		}

		// Check content length and type
		content, _ := rdb.HGet(ctx, "entry:"+id, "content").Result()
		tags, _ := rdb.HGet(ctx, "entry:"+id, "tags").Result()

		if content == "" {
			continue
		}

		// Threshold check based on entry type
		isUser := strings.Contains(tags, "kind:user_prompt") || strings.Contains(tags, "kind:prompt")
		isAssistant := strings.Contains(tags, "kind:assistant_response") || strings.Contains(tags, "kind:assistantmessage")

		qualifies := false
		if isUser && len(content) > minUser {
			qualifies = true
		} else if isAssistant && len(content) > minAssistant {
			qualifies = true
		} else if !isUser && !isAssistant && len(content) > minOther {
			qualifies = true
		}

		if qualifies {
			return id
		}
	}

	return ""
}

// runCondense generates a 1-2 sentence summary for a single entry and stores it.
func runCondense(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string, ollamaClient *ollama.Client) {
	content, _ := rdb.HGet(ctx, "entry:"+entryID, "content").Result()
	if content == "" {
		return
	}

	// Truncate input for the condenser (don't send 10K to condense)
	maxInput := cfg.Daemon.Condenser.MaxInputChars
	if maxInput <= 0 {
		maxInput = 2000
	}
	if len(content) > maxInput {
		content = content[:maxInput]
	}

	prompt := fmt.Sprintf(`Summarize this in 1-2 sentences (max 200 characters). Capture the key point or decision. No preamble.

Rules:
- If it's a data dump, log output, or terminal session: state the TYPE of data and KEY METRICS (e.g. "VBrain training epoch 2037: loss 0.05, NE active, fully myelinated")
- If it's a reference list or citations: describe WHAT the list covers, not individual items
- If it's a code snippet or config: state what it configures and the key parameters
- If it's prose/argument: distill the thesis or conclusion
- NEVER just say "training continues" — always include the distinguishing numbers or state

%s`, content)

	resp, err := ollamaClient.Generate(ctx, prompt)
	if err != nil {
		slog.Debug("condense failed", "entry_id", entryID, "err", err)
		return
	}

	summary := strings.TrimSpace(resp)
	// Strip thinking tags if present
	if idx := strings.Index(summary, "</think>"); idx != -1 {
		summary = strings.TrimSpace(summary[idx+len("</think>"):])
	}

	// Cap at configured max
	maxOut := cfg.Daemon.Condenser.MaxOutputChars
	if maxOut <= 0 {
		maxOut = 250
	}
	if len(summary) > maxOut {
		summary = summary[:maxOut]
	}

	// Store summary as a field on the entry
	rdb.HSet(ctx, "entry:"+entryID, "summary", summary)
	slog.Info("entry condensed", "entry_id", entryID, "summary_len", len(summary))
}

// pickConsolidationCandidate finds a random entry for pairwise relevance scoring.
func pickConsolidationCandidate(ctx context.Context, rdb *redis.Client) string {
	entries, err := rdb.ZRandMember(ctx, "timeline", 1).Result()
	if err != nil || len(entries) == 0 {
		return ""
	}
	// Skip if recently consolidated (cooldown)
	done, _ := rdb.SIsMember(ctx, "consolidate:recent", entries[0]).Result()
	if done {
		return ""
	}
	return entries[0]
}

// runConsolidation does pairwise relevance scoring for an entry.
func runConsolidation(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string, linker *ollama.Client) {
	// Mark as recently processed (cooldown via TTL on the set)
	rdb.SAdd(ctx, "consolidate:recent", entryID)
	cooldownTTLS := cfg.Consolidation.CooldownTTLS
	if cooldownTTLS <= 0 {
		cooldownTTLS = 3600
	}
	rdb.Expire(ctx, "consolidate:recent", time.Duration(cooldownTTLS)*time.Second)

	// 1. Bump temporal neighbors (free, no Ollama call)
	linkTemporalNeighbors(ctx, rdb, cfg, entryID)

	// 2. Opportunistically re-evaluate one existing link for this entry
	if linker != nil {
		reEvaluateRandomLink(ctx, rdb, cfg, entryID, linker)
	}

	// 3. Discovery mode: pick a completely random second entry and ask "are these connected?"
	//    Like smelling a madeleine — most pairings will be noise, but cross-track
	//    discoveries emerge from random collisions that would never happen via co-recall.
	if linker != nil {
		discoverRandomLink(ctx, rdb, cfg, entryID, linker)
	}
}


// reEvaluateRandomLink picks one existing link for the entry, asks the LLM to score it,
// and dissolves the link if it falls below MinScore.
func reEvaluateRandomLink(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string, linker *ollama.Client) {
	// Get all links for this entry
	links, err := rdb.HGetAll(ctx, "links:"+entryID).Result()
	if err != nil || len(links) == 0 {
		return
	}

	// Pick a random link to re-evaluate
	var targetID, oldValue string
	for k, v := range links {
		targetID = k
		oldValue = v
		break // maps are randomized in Go — this gives us a pseudo-random pick
	}

	// Get content of both entries
	contentA, _ := rdb.HGet(ctx, "entry:"+entryID, "content").Result()
	contentB, _ := rdb.HGet(ctx, "entry:"+targetID, "content").Result()
	if contentA == "" || contentB == "" {
		return
	}

	// Skip (dissolve) links where either entry is too short to evaluate meaningfully.
	// These are conversational filler that should never have been linked.
	minLen := cfg.Consolidation.DiscoveryMinLen
	if minLen <= 0 {
		minLen = 200
	}
	if len(contentA) < minLen || len(contentB) < minLen {
		rdb.HDel(ctx, "links:"+entryID, targetID)
		rdb.HDel(ctx, "links:"+targetID, entryID)
		slog.Info("link dissolved (short content)", "entry_a", entryID, "entry_b", targetID, "len_a", len(contentA), "len_b", len(contentB))
		return
	}

	// Truncate for LLM (don't send 10K of content for a relevance check)
	maxChars := cfg.Consolidation.ContentTruncation
	if maxChars <= 0 {
		maxChars = 500
	}
	if len(contentA) > maxChars {
		contentA = contentA[:maxChars]
	}
	if len(contentB) > maxChars {
		contentB = contentB[:maxChars]
	}

	// Ask LLM to score relevance
	prompt := fmt.Sprintf(`Rate the relevance between these two knowledge entries on a scale of 0.0 to 1.0.
0.0 = completely unrelated
0.5 = tangentially related (share a broad topic)
0.8 = directly related (same specific concept)
1.0 = one directly extends/supports the other

Respond with ONLY a number (e.g. "0.7"). No explanation.

Entry A:
%s

Entry B:
%s
`, contentA, contentB)

	resp, err := linker.Generate(ctx, prompt)
	if err != nil {
		slog.Debug("consolidation: re-evaluation failed", "entry", entryID, "target", targetID, "err", err)
		return
	}

	// Parse the score
	resp = strings.TrimSpace(resp)
	// Handle thinking models that wrap in <think> tags
	if idx := strings.Index(resp, "</think>"); idx != -1 {
		resp = strings.TrimSpace(resp[idx+len("</think>"):])
	}
	var newScore float64
	if _, err := fmt.Sscanf(resp, "%f", &newScore); err != nil {
		slog.Debug("consolidation: could not parse score", "response", resp, "err", err)
		return
	}

	// Clamp
	if newScore < 0 {
		newScore = 0
	}
	if newScore > 1 {
		newScore = 1
	}

	// Parse old score
	oldScore, oldType := parseDaemonLinkValue(oldValue)

	minScore := cfg.Consolidation.MinScore
	if minScore <= 0 {
		minScore = 0.4
	}
	driftDelta := cfg.Consolidation.DriftDelta
	if driftDelta <= 0 {
		driftDelta = 0.2
	}

	// Decision: dissolve, update, or leave alone
	if newScore < minScore {
		// Same-session protection: entries from the same session get a second chance.
		// Instead of immediate dissolution, halve the score. Two consecutive low evals = dissolve.
		tagsA, _ := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
		tagsB, _ := rdb.HGet(ctx, "entry:"+targetID, "tags").Result()
		sessionA := extractSession(tagsA)
		sessionB := extractSession(tagsB)
		if sessionA != "" && sessionA == sessionB && oldScore > 0 {
			// Same session, had a positive score — demote rather than dissolve
			demoted := oldScore / 2
			if demoted < 0.1 {
				// Already demoted once and still failing — dissolve for real
				rdb.HDel(ctx, "links:"+entryID, targetID)
				rdb.HDel(ctx, "links:"+targetID, entryID)
				slog.Info("link dissolved (same-session, second strike)", "entry_a", entryID, "entry_b", targetID, "old_score", oldScore, "new_score", newScore)
			} else {
				newValue := fmt.Sprintf("%.4f|%s", demoted, oldType)
				rdb.HSet(ctx, "links:"+entryID, targetID, newValue)
				rdb.HSet(ctx, "links:"+targetID, entryID, newValue)
				slog.Info("link demoted (same-session protection)", "entry_a", entryID, "entry_b", targetID, "old_score", oldScore, "demoted_to", demoted)
			}
		} else {
			// Different sessions or score already 0 — dissolve immediately
			rdb.HDel(ctx, "links:"+entryID, targetID)
			rdb.HDel(ctx, "links:"+targetID, entryID)
			slog.Info("link dissolved", "entry_a", entryID, "entry_b", targetID, "old_score", oldScore, "new_score", newScore, "threshold", minScore)
		}
	} else if abs(newScore-oldScore) > driftDelta {
		// Score drifted significantly — update
		newValue := fmt.Sprintf("%.4f|%s", newScore, oldType)
		rdb.HSet(ctx, "links:"+entryID, targetID, newValue)
		rdb.HSet(ctx, "links:"+targetID, entryID, newValue)
		slog.Info("link updated", "entry_a", entryID, "entry_b", targetID, "old_score", oldScore, "new_score", newScore)
	}
	// Otherwise: within drift tolerance, leave it alone
}

// discoverRandomLink picks a completely random entry from the timeline and asks the LLM
// whether it's connected to entryID. This is the "madeleine moment" — random collisions
// that discover cross-track links no co-recall system would ever find.
func discoverRandomLink(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string, linker *ollama.Client) {
	// Pick a random entry from the timeline
	candidates, err := rdb.ZRandMember(ctx, "timeline", 1).Result()
	if err != nil || len(candidates) == 0 {
		return
	}
	targetID := candidates[0]

	// Don't link to self
	if targetID == entryID {
		return
	}

	// Don't re-discover an existing link
	exists, _ := rdb.HExists(ctx, "links:"+entryID, targetID).Result()
	if exists {
		return
	}

	// Get content of both entries
	contentA, _ := rdb.HGet(ctx, "entry:"+entryID, "content").Result()
	contentB, _ := rdb.HGet(ctx, "entry:"+targetID, "content").Result()
	if contentA == "" || contentB == "" {
		return
	}

	// Skip very short entries (operational noise like "yup do that" or "got it")
	// Discovery needs substantial content to find real conceptual connections.
	discoveryMinLen := cfg.Consolidation.DiscoveryMinLen
	if discoveryMinLen <= 0 {
		discoveryMinLen = 200
	}
	if len(contentA) < discoveryMinLen || len(contentB) < discoveryMinLen {
		return
	}

	// Truncate for LLM
	maxChars := cfg.Consolidation.ContentTruncation
	if maxChars <= 0 {
		maxChars = 500
	}
	if len(contentA) > maxChars {
		contentA = contentA[:maxChars]
	}
	if len(contentB) > maxChars {
		contentB = contentB[:maxChars]
	}

	// Ask LLM to score conceptual relevance — tuned to reject operational/structural similarity
	prompt := fmt.Sprintf(`Do these two knowledge entries share a meaningful CONCEPTUAL connection?

Score 0.0-1.0 where:
0.0 = unrelated
0.3 = same broad project but different topics (NOT a link)
0.5 = share a specific concept, technique, or insight
0.7 = clearly related — same mechanism, pattern, or problem
0.9 = one directly informs, extends, or contradicts the other
1.0 = same core idea expressed differently

IMPORTANT: Being from the same project is NOT enough. "Both are deployment steps" is NOT a connection. "Both mention files" is NOT a connection. Look for shared IDEAS, HYPOTHESES, TECHNIQUES, or INSIGHTS that would transfer knowledge between them.

Do NOT default to 0.5. If the connection is weak, score it 0.3-0.4. If it is strong, score it 0.7+. Use the full range.

Respond with ONLY a number (e.g. "0.7"). No explanation.

Entry A:
%s

Entry B:
%s
`, contentA, contentB)

	resp, err := linker.Generate(ctx, prompt)
	if err != nil {
		slog.Debug("discovery: evaluation failed", "entry_a", entryID, "entry_b", targetID, "err", err)
		return
	}

	// Parse the score
	resp = strings.TrimSpace(resp)
	if idx := strings.Index(resp, "</think>"); idx != -1 {
		resp = strings.TrimSpace(resp[idx+len("</think>"):])
	}
	var score float64
	if _, err := fmt.Sscanf(resp, "%f", &score); err != nil {
		slog.Debug("discovery: could not parse score", "response", resp, "err", err)
		return
	}

	// Clamp
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	// Only create a link if it meets the minimum threshold
	minScore := cfg.Consolidation.MinScore
	if minScore <= 0 {
		minScore = 0.4
	}
	if score < minScore {
		return // noise — discard silently
	}

	// Discovery! Create bidirectional link
	value := fmt.Sprintf("%.4f|%s", score, "discovery")
	rdb.HSet(ctx, "links:"+entryID, targetID, value)
	rdb.HSet(ctx, "links:"+targetID, entryID, value)
	slog.Info("link discovered", "entry_a", entryID, "entry_b", targetID, "score", score)
}

// parseDaemonLinkValue parses "score|type" from the links HASH.
func parseDaemonLinkValue(value string) (float64, string) {
	parts := strings.SplitN(value, "|", 2)
	if len(parts) != 2 {
		var s float64
		fmt.Sscanf(value, "%f", &s)
		return s, "unknown"
	}
	var score float64
	fmt.Sscanf(parts[0], "%f", &score)
	return score, parts[1]
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// processEntry runs the full ingestion pipeline on a single entry.
func processEntry(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string, daemonCfg config.DaemonConfig, clients *ollamaClients) error {
	// Skip system infrastructure entries — orientations, working sets, manifests.
	// These are not conversation content and should never be classified, extracted, or linked.
	if strings.HasPrefix(entryID, "meta:") || strings.HasPrefix(entryID, "entry:meta:") {
		// Opportunistically strip any auto-tags that shouldn't be there
		stripAutoTags(ctx, rdb, entryID)
		return nil
	}

	// Load entry content from Redis
	content, err := loadEntryContent(ctx, rdb, entryID)
	if err != nil {
		return fmt.Errorf("loading entry: %w", err)
	}
	if content == "" {
		return nil // skip empty
	}

	fusedDone := false

	// Fused path: if both classifier AND extractor are enabled, use a single Ollama call.
	if daemonCfg.Classifier.Enabled && daemonCfg.Extractor.Enabled {
		result, err := classifyAndExtract(ctx, rdb, cfg, entryID, content, clients.Extractor)
		if err != nil {
			slog.Error("fused classify+extract failed, falling back to separate", "entry_id", entryID, "err", err)
		} else {
			fusedDone = true
			// Apply classification
			if result.Track != "" {
				applyTrackTag(ctx, rdb, entryID, result.Track)
			}
			// Store triples
			if len(result.Triples) > 0 {
				storeTriples(ctx, rdb, cfg, result.Triples, entryID)
				if daemonCfg.Verifier.Enabled {
					for _, t := range result.Triples {
						checkAndVerify(ctx, rdb, cfg, t)
					}
				}
			}
		}
	}

	// Separate paths (fallback on fused error, or when only one subsystem is enabled)
	if !fusedDone {
		if daemonCfg.Classifier.Enabled {
			track, err := classifyEntry(ctx, rdb, cfg, entryID, content, clients.Classifier)
			if err != nil {
				slog.Error("classification failed", "entry_id", entryID, "err", err)
			} else if track != "" {
				applyTrackTag(ctx, rdb, entryID, track)
			}
		}

		if daemonCfg.Extractor.Enabled {
			triples, err := extractTriples(ctx, rdb, cfg, content, clients.Extractor)
			if err != nil {
				slog.Error("extraction failed", "entry_id", entryID, "err", err)
			} else if len(triples) > 0 {
				storeTriples(ctx, rdb, cfg, triples, entryID)
				if daemonCfg.Verifier.Enabled {
					for _, t := range triples {
						checkAndVerify(ctx, rdb, cfg, t)
					}
				}
			}
		}
	}

	// 3. Opportunistic linking (if enabled)
	if daemonCfg.Linker.Enabled {
		linkFromCoRecall(ctx, rdb, cfg, entryID)
		linkTemporalNeighbors(ctx, rdb, cfg, entryID)
	}

	// Publish activity for UI status display
	rdb.Set(ctx, "daemon:last_processed", fmt.Sprintf("%d", time.Now().Unix()), 0)
	rdb.Set(ctx, "daemon:last_entry_id", entryID, 0)

	return nil
}

// fusedResult holds the output of a combined classify+extract call.
type fusedResult struct {
	Track   string
	Triples []Triple
}

// classifyAndExtract performs classification and epistemic extraction in a single Ollama call.
func classifyAndExtract(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID, content string, ollamaClient *ollama.Client) (*fusedResult, error) {
	// Check for explicit track tag — skip LLM classification if present
	entryTags, _ := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
	explicitTrack := ""
	for _, tag := range strings.Split(entryTags, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "track:") && !strings.HasPrefix(tag, "track_auto:") {
			explicitTrack = strings.TrimPrefix(tag, "track:")
			break
		}
	}

	// Load windowed context for classification
	window := loadClassifyWindow(ctx, rdb, entryID, 2, 2, cfg.Memory.ClassifyMaxChars)

	// Load track manifests
	manifestContent, err := rdb.HGet(ctx, "entry:meta:track-manifests", "content").Result()
	if err != nil {
		manifestContent = "{}" // degrade gracefully
	}

	// Load vocabulary for extraction
	vocab := loadVocabulary(ctx, rdb, cfg.Epistemic.MaxVocabTerms)

	// Build fused prompt
	prompt := buildFusedPrompt(entryID, content, window, manifestContent, vocab, cfg.Memory.ClassifyMaxChars, cfg.Epistemic.MaxTextLen, explicitTrack)

	resp, err := ollamaClient.GenerateWithOptions(ctx, prompt, ollamaClient.Model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  4096,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama fused call: %w", err)
	}

	return parseFusedResponse(resp, explicitTrack)
}

func buildFusedPrompt(entryID, content string, window classifyWindow, manifestJSON string, vocab []string, classifyMaxChars, extractMaxChars int, explicitTrack string) string {
	var b strings.Builder

	// No content truncation — let the model's context window be the limit.
	// If responses get truncated, bump num_ctx in Ollama config rather than limiting input.
	extractContent := content

	// Cap vocabulary to avoid bloating the prompt (these are just reconciliation hints)
	maxVocab := 50
	if len(vocab) > maxVocab {
		vocab = vocab[:maxVocab]
	}

	b.WriteString("/no_think\n")
	b.WriteString("You are an analysis system for a personal knowledge base. Perform TWO tasks on the entry below.\n\n")

	// --- Task 1: Classification ---
	if explicitTrack == "" {
		b.WriteString("## TASK 1: Track Classification\n\n")
		b.WriteString("Assign this entry to one or more project tracks:\n\n")
		b.WriteString(manifestJSON)
		b.WriteString("\n\n")

		// Window context
		if len(window.Before) > 0 {
			b.WriteString("Preceding context:\n")
			for _, e := range window.Before {
				trackTags := extractTrackTags(e.Tags)
				b.WriteString(fmt.Sprintf("  [%s] %s\n", trackTags, util.Truncate(e.Content, classifyMaxChars/2)))
			}
			b.WriteString("\n")
		}
		if len(window.After) > 0 {
			b.WriteString("Following context:\n")
			for _, e := range window.After {
				trackTags := extractTrackTags(e.Tags)
				b.WriteString(fmt.Sprintf("  [%s] %s\n", trackTags, util.Truncate(e.Content, classifyMaxChars/2)))
			}
			b.WriteString("\n")
		}

		b.WriteString("Rules:\n")
		b.WriteString("- If user explicitly says \"track:<Name>\", that is authoritative.\n")
		b.WriteString("- Short messages inherit from surrounding context.\n")
		b.WriteString("- Multi-track OK if content substantively involves both.\n")
		b.WriteString("- If no track fits, use \"none\".\n\n")
	} else {
		b.WriteString(fmt.Sprintf("## TASK 1: Track Classification\n\nAlready classified by user as: %s (skip this task, confirm in output)\n\n", explicitTrack))
	}

	// --- Task 2: Epistemic Extraction ---
	b.WriteString("## TASK 2: Epistemic Extraction\n\n")
	b.WriteString("Extract ONLY claims about how things work in the real world — causal mechanisms, factual assertions, scientific hypotheses, economic relationships, medical claims, predictions.\n\n")
	b.WriteString("DO NOT extract: code architecture, build status, config states, session metadata, project relationships, tautologies.\n")
	b.WriteString("If no real-world claims exist, return empty triples array.\n\n")

	// Vocabulary
	if len(vocab) > 0 {
		b.WriteString("Existing vocabulary (use where applicable): ")
		b.WriteString(strings.Join(vocab, ", "))
		b.WriteString("\nOtherwise use snake_case.\n\n")
	} else {
		b.WriteString("Use snake_case for all subjects and objects.\n\n")
	}

	b.WriteString("Verbs: ONLY one of: causes, prevents, is, distinct, linked\n\n")

	// --- The entry ---
	b.WriteString("## Entry\n\n")
	b.WriteString(fmt.Sprintf("ID: %s\n\n%s\n\n", entryID, extractContent))

	// --- Output format ---
	b.WriteString(`## Output

Respond with ONLY this JSON (no markdown, no explanation):
{
  "tracks": ["TrackName"],
  "confidence": 0.85,
  "triples": [{"subject":"...","relation":"...","object":"...","type":"explicit|implicit"}]
}

If no triples, use "triples": []. If no track fits, use "tracks": ["none"].
`)

	return b.String()
}

func parseFusedResponse(resp string, explicitTrack string) (*fusedResult, error) {
	resp = strings.TrimSpace(resp)

	type fusedJSON struct {
		Tracks     []string `json:"tracks"`
		Confidence float64  `json:"confidence"`
		Triples    []Triple `json:"triples"`
	}

	// Find JSON object in response
	start := strings.Index(resp, "{")
	end := strings.LastIndex(resp, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object in fused response: %.200s", resp)
	}

	var fr fusedJSON
	if err := json.Unmarshal([]byte(resp[start:end+1]), &fr); err != nil {
		return nil, fmt.Errorf("parsing fused JSON: %w (raw: %.500s)", err, resp[start:end+1])
	}

	result := &fusedResult{
		Triples: fr.Triples,
	}

	// Determine track
	if explicitTrack != "" {
		result.Track = explicitTrack
	} else if len(fr.Tracks) > 0 && fr.Tracks[0] != "none" {
		// TODO: multi-track support — for now take first
		result.Track = fr.Tracks[0]
	}

	return result, nil
}

// --- Stubs for implementation ---

func loadEntryContent(ctx context.Context, rdb *redis.Client, entryID string) (string, error) {
	content, err := rdb.HGet(ctx, "entry:"+entryID, "content").Result()
	if err == redis.Nil {
		return "", nil
	}
	return content, err
}

func getUnprocessedEntries(ctx context.Context, rdb *redis.Client, limit int, cursor string) ([]string, string, error) {
	// Scan timeline backwards from cursor (newest → oldest).
	// cursor is the max score to search below (exclusive on repeated calls).
	maxScore := cursor
	if cursor == "+inf" {
		maxScore = "+inf"
	}

	// Fetch more than limit to account for already-processed entries
	entries, err := rdb.ZRevRangeByScore(ctx, "timeline", &redis.ZRangeBy{
		Min:   "-inf",
		Max:   maxScore,
		Count: int64(limit * 3),
	}).Result()
	if err != nil {
		return nil, cursor, err
	}

	var unprocessed []string
	var lastScore string
	for _, id := range entries {
		isMember, _ := rdb.SIsMember(ctx, "daemon:processed", id).Result()
		if !isMember {
			unprocessed = append(unprocessed, id)
			if len(unprocessed) >= limit {
				// Get the score of the last entry to use as next cursor
				score, _ := rdb.ZScore(ctx, "timeline", id).Result()
				lastScore = fmt.Sprintf("(%f", score) // "(" prefix = exclusive
				break
			}
		}
	}

	if lastScore == "" && len(entries) > 0 {
		// Didn't fill the batch but got some entries — use last entry's score
		lastEntry := entries[len(entries)-1]
		score, _ := rdb.ZScore(ctx, "timeline", lastEntry).Result()
		lastScore = fmt.Sprintf("(%f", score)
	}

	if lastScore == "" {
		lastScore = cursor // no progress
	}

	return unprocessed, lastScore, nil
}

func markProcessed(ctx context.Context, rdb *redis.Client, entryID string) {
	rdb.SAdd(ctx, "daemon:processed", entryID)
}

func classifyEntry(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID, content string, ollamaClient *ollama.Client) (string, error) {
	// Check if entry already has an explicit track: tag — if so, use it as signal but still classify
	entryTags, _ := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
	for _, tag := range strings.Split(entryTags, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "track:") && !strings.HasPrefix(tag, "track_auto:") {
			// Has explicit tag — daemon still classifies for its own record but won't override
			// Return the explicit track as the "answer" without calling LLM
			return strings.TrimPrefix(tag, "track:"), nil
		}
	}

	// Load windowed context: 2 before + entry + 2 after
	window := loadClassifyWindow(ctx, rdb, entryID, 2, 2, cfg.Memory.ClassifyMaxChars)

	// Load track manifests
	manifestContent, err := rdb.HGet(ctx, "entry:meta:track-manifests", "content").Result()
	if err != nil {
		return "", fmt.Errorf("track manifests not found: %w", err)
	}

	// Build prompt
	prompt := buildDaemonClassifyPrompt(entryID, content, window, manifestContent, cfg.Memory.ClassifyMaxChars)

	resp, err := ollamaClient.GenerateWithOptions(ctx, prompt, ollamaClient.Model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  512,
	})
	if err != nil {
		return "", fmt.Errorf("ollama classify: %w", err)
	}

	return parseDaemonClassifyResponse(resp)
}

// classifyWindow holds context entries for the sliding window.
type classifyWindow struct {
	Before []windowEntry
	After  []windowEntry
}

type windowEntry struct {
	ID      string
	Content string
	Tags    string
}

func loadClassifyWindow(ctx context.Context, rdb *redis.Client, entryID string, beforeN, afterN, maxChars int) classifyWindow {
	var w classifyWindow

	// Get entry's score (timestamp) from timeline
	score, err := rdb.ZScore(ctx, "timeline", entryID).Result()
	if err != nil {
		return w
	}

	// Get entries before (lower scores)
	if beforeN > 0 {
		before, _ := rdb.ZRevRangeByScore(ctx, "timeline", &redis.ZRangeBy{
			Min:   "-inf",
			Max:   fmt.Sprintf("(%f", score), // exclusive
			Count: int64(beforeN),
		}).Result()
		for _, id := range before {
			content, _ := rdb.HGet(ctx, "entry:"+id, "content").Result()
			tags, _ := rdb.HGet(ctx, "entry:"+id, "tags").Result()
			if len(content) > maxChars {
				content = content[:maxChars] + "..."
			}
			w.Before = append(w.Before, windowEntry{ID: id, Content: content, Tags: tags})
		}
	}

	// Get entries after (higher scores)
	if afterN > 0 {
		after, _ := rdb.ZRangeByScore(ctx, "timeline", &redis.ZRangeBy{
			Min:   fmt.Sprintf("(%f", score), // exclusive
			Max:   "+inf",
			Count: int64(afterN),
		}).Result()
		for _, id := range after {
			content, _ := rdb.HGet(ctx, "entry:"+id, "content").Result()
			tags, _ := rdb.HGet(ctx, "entry:"+id, "tags").Result()
			if len(content) > maxChars {
				content = content[:maxChars] + "..."
			}
			w.After = append(w.After, windowEntry{ID: id, Content: content, Tags: tags})
		}
	}

	return w
}

func buildDaemonClassifyPrompt(entryID, content string, window classifyWindow, manifestJSON string, maxChars int) string {
	var b strings.Builder

	b.WriteString("/no_think\n")
	b.WriteString("You are a track classifier for a personal knowledge system.\n\n")
	b.WriteString("## Available Tracks\n\n")
	b.WriteString(manifestJSON)
	b.WriteString("\n\n")

	// Context before
	if len(window.Before) > 0 {
		b.WriteString("## Preceding Context (for continuity)\n\n")
		for _, e := range window.Before {
			trackTags := extractTrackTags(e.Tags)
			b.WriteString(fmt.Sprintf("  [%s] %s\n", trackTags, util.Truncate(e.Content, maxChars/2)))
		}
		b.WriteString("\n")
	}

	// Target entry
	b.WriteString("## Entry to Classify\n\n")
	b.WriteString(fmt.Sprintf("ID: %s\nContent: %s\n\n", entryID, content))

	// Context after
	if len(window.After) > 0 {
		b.WriteString("## Following Context (for continuity)\n\n")
		for _, e := range window.After {
			trackTags := extractTrackTags(e.Tags)
			b.WriteString(fmt.Sprintf("  [%s] %s\n", trackTags, util.Truncate(e.Content, maxChars/2)))
		}
		b.WriteString("\n")
	}

	b.WriteString(`## Instructions

Assign this entry to one or more tracks from the list above.
- If the user explicitly mentions "track:<Name>" in the content, that is authoritative.
- Short messages ("OK", "yeah", "interesting") inherit from surrounding context.
- If it genuinely doesn't fit any track, respond with "none".
- Multi-track is allowed if content substantively involves both.

Respond with ONLY a JSON object:
{"tracks": ["TrackName"], "confidence": 0.85, "reason": "brief explanation"}
`)

	return b.String()
}

func parseDaemonClassifyResponse(resp string) (string, error) {
	resp = strings.TrimSpace(resp)

	// Try to parse JSON response
	type classifyResp struct {
		Tracks     []string `json:"tracks"`
		Confidence float64  `json:"confidence"`
		Reason     string   `json:"reason"`
	}

	// Find JSON in response
	start := strings.Index(resp, "{")
	end := strings.LastIndex(resp, "}")
	if start == -1 || end == -1 || end <= start {
		// Fallback: just return the first word
		if parts := strings.Fields(resp); len(parts) > 0 {
			return strings.ToLower(parts[0]), nil
		}
		return "", fmt.Errorf("unparseable classify response: %.200s", resp)
	}

	var cr classifyResp
	if err := json.Unmarshal([]byte(resp[start:end+1]), &cr); err != nil {
		return "", fmt.Errorf("parsing classify JSON: %w", err)
	}

	if len(cr.Tracks) == 0 || cr.Tracks[0] == "none" {
		return "", nil
	}

	// Return first track for now (multi-track support later)
	// TODO: return all tracks + confidence for hysteresis logic
	return cr.Tracks[0], nil
}

func extractTrackTags(tagsStr string) string {
	var tracks []string
	for _, tag := range strings.Split(tagsStr, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "track:") || strings.HasPrefix(tag, "track_auto:") {
			tracks = append(tracks, tag)
		}
	}
	if len(tracks) == 0 {
		return "unclassified"
	}
	return strings.Join(tracks, ", ")
}



func applyTrackTag(ctx context.Context, rdb *redis.Client, entryID, track string) {
	if track == "" || track == "none" {
		return
	}

	// Use track_auto: prefix for daemon-inferred tags
	tag := "track_auto:" + track

	// Get existing tags
	existing, err := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
	if err != nil {
		return
	}

	tags := strings.Split(existing, ",")

	// Check if this auto-tag already exists
	for _, t := range tags {
		if strings.TrimSpace(t) == tag {
			return // already tagged
		}
	}

	// Add the auto tag
	tags = append(tags, tag)

	// Mark as classified by daemon
	hasClassified := false
	for _, t := range tags {
		if strings.TrimSpace(t) == "classified" {
			hasClassified = true
			break
		}
	}
	if !hasClassified {
		tags = append(tags, "classified", "classified:auto")
	}

	// Write back
	rdb.HSet(ctx, "entry:"+entryID, "tags", strings.Join(tags, ","))
	rdb.SAdd(ctx, "tag:"+tag, entryID)
	rdb.SAdd(ctx, "tags:all", tag)
	rdb.SAdd(ctx, "tag:classified", entryID)
	rdb.SAdd(ctx, "tag:classified:auto", entryID)
	slog.Info("classified", "entry_id", entryID, "track", tag)
}

// Triple is an alias for the epistemic package's Triple type.
type Triple = epistemic.Triple

func extractTriples(ctx context.Context, rdb *redis.Client, cfg *config.Config, content string, ollamaClient *ollama.Client) ([]Triple, error) {
	if len(content) < cfg.Epistemic.MinEntryLen {
		return nil, nil
	}

	extractor := epistemic.NewExtractor(ollamaClient, cfg.Epistemic)

	// Get vocabulary for reconciliation
	vocab := loadVocabulary(ctx, rdb, cfg.Epistemic.MaxVocabTerms)

	return extractor.Extract(ctx, content, vocab)
}

func loadVocabulary(ctx context.Context, rdb *redis.Client, maxTerms int) []string {
	terms, err := rdb.SRandMemberN(ctx, "vocab:tier2:terms", int64(maxTerms)).Result()
	if err != nil {
		return nil
	}
	return terms
}

func storeTriples(ctx context.Context, rdb *redis.Client, cfg *config.Config, triples []Triple, sourceEntryID string) {
	for _, t := range triples {
		hash := fmt.Sprintf("%s|%s|%s", t.Subject, t.Relation, t.Object)
		key := "epistemic:" + hash

		// Check if already exists
		exists, _ := rdb.Exists(ctx, key).Result()
		if exists > 0 {
			// Increment encounter count, append source
			rdb.HIncrBy(ctx, key, "encounter_count", 1)
			rdb.HSet(ctx, key, "last_seen", fmt.Sprintf("%d", time.Now().Unix()))
			// Append source entry
			existing, _ := rdb.HGet(ctx, key, "source_entries").Result()
			if !strings.Contains(existing, sourceEntryID) {
				if existing != "" {
					existing += ","
				}
				rdb.HSet(ctx, key, "source_entries", existing+sourceEntryID)
			}
		} else {
			// New triple
			rdb.HSet(ctx, key, map[string]interface{}{
				"canonical":       hash,
				"subject":         t.Subject,
				"verb":            t.Relation,
				"object":          t.Object,
				"status":          "unknown",
				"confidence":      "0.5",
				"encounter_count": "1",
				"first_seen":      fmt.Sprintf("%d", time.Now().Unix()),
				"last_seen":       fmt.Sprintf("%d", time.Now().Unix()),
				"source_entries":  sourceEntryID,
				"evidence_for":    "",
				"evidence_against": "",
				"verified_by":     "",
			})
			// Add to status set
			rdb.SAdd(ctx, "epistemic:status:unknown", hash)
			// Add to subject/object indices
			rdb.SAdd(ctx, "epistemic:by_subject:"+t.Subject, hash)
			rdb.SAdd(ctx, "epistemic:by_object:"+t.Object, hash)
			// Add to vocabulary
			rdb.SAdd(ctx, "vocab:tier2:terms", t.Subject, t.Object)
		}
	}
	if len(triples) > 0 {
		slog.Info("triples extracted", "count", len(triples), "entry_id", sourceEntryID)
	}
}

func checkAndVerify(ctx context.Context, rdb *redis.Client, cfg *config.Config, t Triple) {
	hash := fmt.Sprintf("%s|%s|%s", t.Subject, t.Relation, t.Object)
	key := "epistemic:" + hash

	countStr, err := rdb.HGet(ctx, key, "encounter_count").Result()
	if err != nil {
		return
	}

	count := 0
	fmt.Sscanf(countStr, "%d", &count)

	if count < cfg.Epistemic.MinEncounters {
		return
	}

	// Check if already verified
	status, _ := rdb.HGet(ctx, key, "status").Result()
	if status != "unknown" {
		return // already verified/contested/false
	}

	// Mark as ready for verification — the verifier will pick it up on next run.
	// We could run inline verification here, but it's expensive (2 Ollama calls per triple).
	// Instead, add to a priority verification queue so the next --verify run handles it first.
	rdb.SAdd(ctx, "epistemic:verify:ready", hash)
	slog.Info("triple ready for verification", "triple", hash, "encounters", count)
}

func linkFromCoRecall(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string) {
	// Check recalled:<entryID> set, bump co-recall counts, auto-link at threshold
	recalledKey := fmt.Sprintf("recalled:%s", entryID)
	recalled, err := rdb.SMembers(ctx, recalledKey).Result()
	if err != nil || len(recalled) == 0 {
		return
	}

	// Filter out short entries — they carry no semantic content worth linking
	minLen := cfg.Consolidation.DiscoveryMinLen
	if minLen <= 0 {
		minLen = 200
	}

	var substantialRecalled []string
	for _, id := range recalled {
		content, _ := rdb.HGet(ctx, "entry:"+id, "content").Result()
		if len(content) >= minLen {
			substantialRecalled = append(substantialRecalled, id)
		}
	}

	// Also check the source entry
	sourceContent, _ := rdb.HGet(ctx, "entry:"+entryID, "content").Result()
	if len(sourceContent) < minLen {
		rdb.Del(ctx, recalledKey)
		return
	}

	for _, recalledID := range substantialRecalled {
		bumpCoRecall(ctx, rdb, cfg, entryID, recalledID)
	}

	// Also link recalled entries to each other (they were co-relevant)
	for i := 0; i < len(substantialRecalled); i++ {
		for j := i + 1; j < len(substantialRecalled); j++ {
			bumpCoRecall(ctx, rdb, cfg, substantialRecalled[i], substantialRecalled[j])
		}
	}

	// Clean up: co-recall data is now permanent in corecall:counts, drop the ephemeral set
	rdb.Del(ctx, recalledKey)
}

func linkTemporalNeighbors(ctx context.Context, rdb *redis.Client, cfg *config.Config, entryID string) {
	// Get entry's timestamp from timeline
	score, err := rdb.ZScore(ctx, "timeline", entryID).Result()
	if err != nil {
		return
	}

	// Get 2 entries before and 2 after
	before, _ := rdb.ZRevRangeByScore(ctx, "timeline", &redis.ZRangeBy{
		Min:   "-inf",
		Max:   fmt.Sprintf("(%f", score),
		Count: 2,
	}).Result()

	after, _ := rdb.ZRangeByScore(ctx, "timeline", &redis.ZRangeBy{
		Min:   fmt.Sprintf("(%f", score),
		Max:   "+inf",
		Count: 2,
	}).Result()

	// Bump co-recall for temporal neighbors that share the same track AND session
	entryTags, _ := rdb.HGet(ctx, "entry:"+entryID, "tags").Result()
	entryTrack := extractPrimaryTrack(entryTags)
	entrySession := extractSession(entryTags)

	neighbors := append(before, after...)
	for _, neighborID := range neighbors {
		if neighborID == entryID {
			continue
		}
		neighborTags, _ := rdb.HGet(ctx, "entry:"+neighborID, "tags").Result()
		neighborTrack := extractPrimaryTrack(neighborTags)
		neighborSession := extractSession(neighborTags)

		// Only link temporal neighbors that share a track AND session
		// (avoids cross-session noise from parallel windows interleaving on the timeline)
		if entryTrack == "" || neighborTrack == "" || entryTrack != neighborTrack {
			continue
		}
		if entrySession != "" && neighborSession != "" && entrySession != neighborSession {
			continue
		}
		bumpCoRecall(ctx, rdb, cfg, entryID, neighborID)
	}
}

// extractPrimaryTrack returns the first track tag found (explicit or auto).
func extractPrimaryTrack(tagsStr string) string {
	for _, tag := range strings.Split(tagsStr, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "track:") && !strings.HasPrefix(tag, "track_auto:") {
			return strings.TrimPrefix(tag, "track:")
		}
		if strings.HasPrefix(tag, "track_auto:") {
			return strings.TrimPrefix(tag, "track_auto:")
		}
	}
	return ""
}

// extractSession returns the session ID from a comma-separated tag string.
func extractSession(tagsStr string) string {
	for _, tag := range strings.Split(tagsStr, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "session:") {
			return strings.TrimPrefix(tag, "session:")
		}
	}
	return ""
}

func bumpCoRecall(ctx context.Context, rdb *redis.Client, cfg *config.Config, idA, idB string) {
	// Canonical key ordering (alphabetical)
	a, b := idA, idB
	if a > b {
		a, b = b, a
	}
	pairKey := a + "|" + b

	count, err := rdb.HIncrBy(ctx, "corecall:counts", pairKey, 1).Result()
	if err != nil {
		return
	}

	// Auto-link at threshold (score 0.0 = "related but unscored")
	if count == int64(cfg.Daemon.CorecallThreshold) {
		// Check if link already exists
		linkKey := fmt.Sprintf("links:%s", a)
		exists, _ := rdb.HExists(ctx, linkKey, b).Result()
		if !exists {
			// Create bidirectional link with score 0.0 and type "corecall"
			rdb.HSet(ctx, fmt.Sprintf("links:%s", a), b, "0.0|corecall")
			rdb.HSet(ctx, fmt.Sprintf("links:%s", b), a, "0.0|corecall")
			slog.Info("auto-linked", "entry_a", a, "entry_b", b, "corecall_count", count)
		}
	}
}

// migrateLegacyLinks converts old ZSET-based links (link:<id>) to the new HASH format (links:<id>).
// Runs once at daemon startup; falls through immediately if no legacy keys exist.
func migrateLegacyLinks(ctx context.Context, rdb *redis.Client) {
	// Phase 1: Migrate old "link:*" ZSET keys to "links:*" HASH format
	keys, err := rdb.Keys(ctx, "link:*").Result()
	if err != nil {
		slog.Warn("legacy link migration: failed to scan keys", "err", err)
		return
	}

	// Filter to only ZSET keys (skip link:meta:* which are STRING keys)
	var zsetKeys []string
	for _, k := range keys {
		t, _ := rdb.Type(ctx, k).Result()
		if t == "zset" {
			zsetKeys = append(zsetKeys, k)
		}
	}

	if len(zsetKeys) > 0 {
		slog.Info("legacy link migration phase 1: old prefix", "zset_keys", len(zsetKeys))
		var migrated, deleted int

		for _, key := range zsetKeys {
			// Extract entry ID from key (link:<id> → <id>)
			id := strings.TrimPrefix(key, "link:")

			members, err := rdb.ZRangeWithScores(ctx, key, 0, -1).Result()
			if err != nil {
				slog.Warn("legacy link migration: failed to read ZSET", "key", key, "err", err)
				continue
			}

			for _, z := range members {
				targetID := z.Member.(string)
				score := z.Score

				// Check for relation type metadata
				relType, _ := rdb.Get(ctx, "link:meta:"+id+":"+targetID).Result()
				if relType == "" {
					relType = "legacy"
				}

				value := fmt.Sprintf("%.4f|%s", score, relType)

				// Write to new HASH format (bidirectional)
				rdb.HSet(ctx, "links:"+id, targetID, value)
				rdb.HSet(ctx, "links:"+targetID, id, value)
				migrated++

				// Clean up relation metadata
				rdb.Del(ctx, "link:meta:"+id+":"+targetID)
				rdb.Del(ctx, "link:meta:"+targetID+":"+id)
			}

			// Delete the old ZSET key
			rdb.Del(ctx, key)
			deleted++
		}

		slog.Info("legacy link migration phase 1: complete", "keys_deleted", deleted, "links_migrated", migrated)
	}

	// Phase 2: Migrate "links:*" keys that are ZSETs (created by old classifier)
	// to the correct HASH format. Uses SCAN to avoid blocking on large keyspaces.
	var cursor uint64
	var zsetLinksKeys []string
	for {
		var batch []string
		var err error
		batch, cursor, err = rdb.Scan(ctx, cursor, "links:*", 500).Result()
		if err != nil {
			slog.Warn("legacy link migration phase 2: scan failed", "err", err)
			break
		}
		for _, k := range batch {
			t, _ := rdb.Type(ctx, k).Result()
			if t == "zset" {
				zsetLinksKeys = append(zsetLinksKeys, k)
			}
		}
		if cursor == 0 {
			break
		}
	}

	if len(zsetLinksKeys) == 0 {
		return
	}

	slog.Info("legacy link migration phase 2: links:* ZSETs", "keys", len(zsetLinksKeys))
	var migrated2, deleted2 int

	for _, key := range zsetLinksKeys {
		id := strings.TrimPrefix(key, "links:")

		members, err := rdb.ZRangeWithScores(ctx, key, 0, -1).Result()
		if err != nil {
			slog.Warn("legacy link migration phase 2: failed to read ZSET", "key", key, "err", err)
			continue
		}

		// Delete the ZSET first so we can create the HASH with the same key
		rdb.Del(ctx, key)
		deleted2++

		for _, z := range members {
			targetID := z.Member.(string)
			score := z.Score
			value := fmt.Sprintf("%.4f|%s", score, "legacy")

			// Write to HASH format (bidirectional)
			rdb.HSet(ctx, "links:"+id, targetID, value)
			rdb.HSet(ctx, "links:"+targetID, id, value)
			migrated2++
		}
	}

	slog.Info("legacy link migration phase 2: complete", "keys_deleted", deleted2, "links_migrated", migrated2)
}
