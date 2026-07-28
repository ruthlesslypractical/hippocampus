// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
	"github.com/ruthlesslypractical/hippocampus/pkg/modelresponse"
)

// Verifier runs the multi-pass verification pipeline on epistemic entries
// that have accumulated enough encounters to warrant fact-checking.
type Verifier struct {
	rdb      *redis.Client
	client   *ollama.Client
	registry *Registry
	cfg      config.EpistemicConfig
	dryRun   bool
}

// NewVerifier creates a new verification pipeline.
func NewVerifier(rdb *redis.Client, ollamaClient *ollama.Client, cfg config.EpistemicConfig, dryRun bool) *Verifier {
	return &Verifier{
		rdb:      rdb,
		client:   ollamaClient,
		registry: NewRegistry(rdb),
		cfg:      cfg,
		dryRun:   dryRun,
	}
}

// VerifyResult holds the outcome of the 3-pass verification.
type VerifyResult struct {
	Canonical       string
	NewStatus       Status
	Confidence      float64
	EvidenceFor     string
	EvidenceAgainst string
}

// RunVerification processes epistemic entries that have encounter_count >= minEncounters
// and status == "unknown". Runs the 3-pass pipeline on up to maxEntries.
func (v *Verifier) RunVerification(ctx context.Context, minEncounters int, maxEntries int, force bool) error {
	slog.Info("epistemic verification started", "min_encounters", minEncounters, "max_entries", maxEntries, "force", force)

	// Get candidates for verification
	candidates, err := v.getCandidates(ctx, minEncounters, maxEntries, force)
	if err != nil {
		return fmt.Errorf("get candidates: %w", err)
	}

	if len(candidates) == 0 {
		slog.Info("no entries ready for verification")
		return nil
	}

	slog.Info("found verification candidates", "count", len(candidates))

	verified := 0
	for _, entry := range candidates {
		if ctx.Err() != nil {
			break
		}

		slog.Info("verifying triple", "triple", entry.Canonical, "encounters", entry.EncounterCount)

		// Gather context: pull the source entries that contain this claim
		sourceContext := v.gatherSourceContext(ctx, entry)

		// Run the 3-pass pipeline
		result, err := v.verifyTriple(ctx, entry, sourceContext)
		if err != nil {
			slog.Warn("verification failed", "triple", entry.Canonical, "err", err)
			continue
		}

		if v.dryRun {
			slog.Info("verification result", "dry_run", true, "triple", entry.Canonical, "status", result.NewStatus, "confidence", result.Confidence)
			if result.EvidenceFor != "" {
				slog.Info("evidence for", "dry_run", true, "triple", entry.Canonical, "evidence", util.Truncate(result.EvidenceFor, 100))
			}
			if result.EvidenceAgainst != "" {
				slog.Info("evidence against", "dry_run", true, "triple", entry.Canonical, "evidence", util.Truncate(result.EvidenceAgainst, 100))
			}
		} else {
			// Auto-prune: contested + high confidence = unfalsifiable noise.
			// The verifier can argue both sides equally well, which means the
			// triple is either too vague to have a truth value or is a definitional
			// tautology that leaked through extraction. Remove from verification pool.
			if result.NewStatus == StatusContested && result.Confidence >= v.cfg.AutoPruneConf {
				slog.Info("auto-pruned", "triple", entry.Canonical, "confidence", result.Confidence, "reason", "unfalsifiable")
				err = v.demoteToDefinitional(ctx, entry.Canonical)
				if err != nil {
					slog.Warn("auto-prune failed", "triple", entry.Canonical, "err", err)
				}
				verified++
				continue
			}

			err = v.registry.UpdateStatus(ctx, entry.Canonical,
				result.NewStatus, result.Confidence,
				result.EvidenceFor, result.EvidenceAgainst,
				"verifier:3pass")
			if err != nil {
				slog.Warn("status update failed", "triple", entry.Canonical, "err", err)
				continue
			}
			slog.Info("verified", "triple", entry.Canonical, "status", result.NewStatus, "confidence", result.Confidence)
		}

		verified++
	}

	slog.Info("verification complete", "processed", verified, "total", len(candidates))
	return nil
}

// RunRandomRecheck pulls N random entries from ANY status and re-verifies them.
// This is the "immune patrol" — periodically re-examining old assumptions.
// If re-verification agrees with prior status, confidence is reinforced.
// If it disagrees, status is updated and the change is logged.
func (v *Verifier) RunRandomRecheck(ctx context.Context, n int) error {
	slog.Info("epistemic random recheck started", "sample_size", n)

	// Pull from all statuses
	var allEntries []RegistryEntry
	for _, status := range []Status{StatusUnknown, StatusVerified, StatusContested, StatusFalse} {
		entries, err := v.registry.GetByStatus(ctx, status)
		if err != nil {
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	if len(allEntries) == 0 {
		slog.Info("registry empty, nothing to recheck")
		return nil
	}

	// Random sample (Fisher-Yates shuffle, take first N)
	shuffled := shuffleEntries(allEntries)
	if len(shuffled) > n {
		shuffled = shuffled[:n]
	}

	slog.Info("sampled entries for recheck", "count", len(shuffled))

	rechecked := 0
	changed := 0
	reinforced := 0

	for _, entry := range shuffled {
		if ctx.Err() != nil {
			break
		}

		priorStatus := entry.Status
		priorConfidence := entry.Confidence

		slog.Info("rechecking triple", "triple", entry.Canonical, "prior_status", priorStatus, "prior_confidence", priorConfidence)

		sourceContext := v.gatherSourceContext(ctx, entry)
		result, err := v.verifyTriple(ctx, entry, sourceContext)
		if err != nil {
			slog.Warn("recheck failed", "triple", entry.Canonical, "err", err)
			continue
		}

		if v.dryRun {
			if result.NewStatus == priorStatus {
				slog.Info("recheck reinforced", "dry_run", true, "triple", entry.Canonical, "status", priorStatus, "prior_confidence", priorConfidence, "new_confidence", result.Confidence)
			} else {
				slog.Info("recheck changed", "dry_run", true, "triple", entry.Canonical, "prior_status", priorStatus, "new_status", result.NewStatus, "prior_confidence", priorConfidence, "new_confidence", result.Confidence)
			}
		} else {
			verifiedBy := "verifier:recheck"
			if result.NewStatus == priorStatus {
				// Reinforcement: boost confidence slightly
				newConf := result.Confidence
				if newConf < priorConfidence+v.cfg.ReinforceBoost {
					newConf = priorConfidence + v.cfg.ReinforceBoost
				}
				if newConf > 1.0 {
					newConf = 1.0
				}
				result.Confidence = newConf
				reinforced++
				slog.Info("recheck reinforced", "triple", entry.Canonical, "status", priorStatus, "prior_confidence", priorConfidence, "new_confidence", result.Confidence)
			} else {
				// Status change — log the transition
				result.EvidenceAgainst = fmt.Sprintf("[was %s@%.2f] %s",
					priorStatus, priorConfidence, result.EvidenceAgainst)
				verifiedBy = fmt.Sprintf("verifier:recheck:was_%s", priorStatus)
				changed++
				slog.Info("recheck changed", "triple", entry.Canonical, "prior_status", priorStatus, "new_status", result.NewStatus)
			}

			err = v.registry.UpdateStatus(ctx, entry.Canonical,
				result.NewStatus, result.Confidence,
				result.EvidenceFor, result.EvidenceAgainst,
				verifiedBy)
			if err != nil {
				slog.Warn("status update failed", "triple", entry.Canonical, "err", err)
				continue
			}
		}

		rechecked++
	}

	slog.Info("random recheck complete", "processed", rechecked, "reinforced", reinforced, "changed", changed)
	return nil
}

// shuffleEntries returns a randomly shuffled copy of the entries slice.
func shuffleEntries(entries []RegistryEntry) []RegistryEntry {
	// Use a simple PRNG based on current time nanoseconds
	// (good enough for shuffling, not crypto)
	result := make([]RegistryEntry, len(entries))
	copy(result, entries)
	seed := time.Now().UnixNano()
	for i := len(result) - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1442695040888963407 // LCG
		j := int((seed >> 33) % int64(i+1))
		if j < 0 {
			j = -j
		}
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// verifyTriple runs the 3-pass verification on a single claim.
func (v *Verifier) verifyTriple(ctx context.Context, entry RegistryEntry, sourceContext string) (VerifyResult, error) {
	claim := fmt.Sprintf("%s %s %s", entry.Subject, entry.Verb, entry.Object)

	// Pass 1: Support assessment
	supportPrompt := fmt.Sprintf(`/no_think
TASK: Assess whether this claim can be supported by evidence.

CLAIM: "%s"

CONTEXT:
%s

Rate confidence that claim is TRUE (0.0-1.0). Cite strongest supporting evidence in 1-2 sentences.

RESPOND WITH ONLY THIS JSON FORMAT, NOTHING ELSE:
{"confidence": 0.0, "evidence": "supporting evidence here"}`, claim, util.Truncate(sourceContext, 2000))

	supportResp, err := v.client.GenerateWithOptions(ctx, supportPrompt, v.client.Model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  512,
	})
	if err != nil {
		return VerifyResult{}, fmt.Errorf("pass 1 (support): %w", err)
	}
	support := parseVerifyResponse(supportResp, v.cfg.ResponseTrunc)

	// Pass 2: Counter-evidence assessment
	counterPrompt := fmt.Sprintf(`/no_think
TASK: Find reasons why this claim might be WRONG.

CLAIM: "%s"

CONTEXT:
%s

Rate confidence that claim is FALSE (0.0-1.0). Cite strongest counter-evidence in 1-2 sentences.

RESPOND WITH ONLY THIS JSON FORMAT, NOTHING ELSE:
{"confidence": 0.0, "evidence": "counter-evidence here"}`, claim, util.Truncate(sourceContext, 2000))

	counterResp, err := v.client.GenerateWithOptions(ctx, counterPrompt, v.client.Model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  512,
	})
	if err != nil {
		return VerifyResult{}, fmt.Errorf("pass 2 (counter): %w", err)
	}
	counter := parseVerifyResponse(counterResp, v.cfg.ResponseTrunc)

	// Pass 3: Reconciliation — weigh both sides
	result := reconcile(entry, support, counter)

	return result, nil
}

// verifyPassResult holds a single pass's output.
type verifyPassResult struct {
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

func parseVerifyResponse(response string, responseTrunc int) verifyPassResult {
	if responseTrunc <= 0 {
		responseTrunc = 300
	}
	response = strings.TrimSpace(response)

	// Try ParseJSON first (handles preamble/postamble cleanly)
	if result, err := modelresponse.ParseJSON[verifyPassResult](response); err == nil {
		return result
	}

	// Fallback: try to extract a confidence number from free-form text
	// Look for patterns like "0.7", "0.85", "confidence: 0.6"
	confidence := 0.5
	for i := 0; i < len(response)-2; i++ {
		if response[i] == '0' && i+1 < len(response) && response[i+1] == '.' {
			end := i + 2
			for end < len(response) && response[end] >= '0' && response[end] <= '9' {
				end++
			}
			if end > i+2 {
				var val float64
				fmt.Sscanf(response[i:end], "%f", &val)
				if val > 0 && val <= 1.0 {
					confidence = val
					break
				}
			}
		}
	}

	// Use first 150 chars of response as evidence text
	evidence := response
	if len(evidence) > 150 {
		evidence = evidence[:150] + "..."
	}

	return verifyPassResult{Confidence: confidence, Evidence: evidence}
}

// reconcile takes the support and counter assessments and determines final status.
func reconcile(entry RegistryEntry, support, counter verifyPassResult) VerifyResult {
	result := VerifyResult{
		Canonical:       entry.Canonical,
		EvidenceFor:     support.Evidence,
		EvidenceAgainst: counter.Evidence,
	}

	// Decision logic:
	// - If support is high AND counter is low → verified
	// - If support is low AND counter actively contradicts → false
	// - If both are high OR both are moderate → contested
	// - If both are low OR counter is "no evidence found" → unknown

	supportConf := support.Confidence
	counterConf := counter.Confidence

	// Detect "absence of evidence" vs "evidence of absence"
	// If the counter-evidence text says "does not provide", "no evidence",
	// "not cited", "not demonstrated" — that's absence, not contradiction.
	counterIsAbsence := isAbsenceOfEvidence(counter.Evidence)
	supportIsAbsence := isAbsenceOfEvidence(support.Evidence)

	// If BOTH sides are just saying "no evidence in context" → unknown (need more sources)
	if counterIsAbsence && supportIsAbsence {
		result.NewStatus = StatusUnknown
		result.Confidence = 0.3
		return result
	}

	// If counter is high but it's just "no evidence found" (not active contradiction) → unknown
	if counterConf >= 0.7 && counterIsAbsence && supportConf <= 0.3 {
		result.NewStatus = StatusUnknown
		result.Confidence = 0.35
		return result
	}

	switch {
	case supportConf >= 0.7 && counterConf <= 0.3:
		result.NewStatus = StatusVerified
		result.Confidence = supportConf
	case counterConf >= 0.7 && supportConf <= 0.3:
		// Only mark FALSE if counter has actual contradicting evidence
		result.NewStatus = StatusFalse
		result.Confidence = counterConf
	case supportConf >= 0.5 && counterConf >= 0.5:
		// Both sides have evidence — it's contested
		result.NewStatus = StatusContested
		result.Confidence = (supportConf + counterConf) / 2
	case supportConf < 0.4 && counterConf < 0.4:
		// Neither side is confident — leave as unknown
		result.NewStatus = StatusUnknown
		result.Confidence = 0.3
	default:
		// Moderate support, low-moderate counter — lean toward verified but flag
		if supportConf > counterConf {
			result.NewStatus = StatusVerified
			result.Confidence = supportConf - counterConf
		} else {
			result.NewStatus = StatusContested
			result.Confidence = 0.5
		}
	}

	return result
}

// isAbsenceOfEvidence detects when a verification response is saying
// "I couldn't find evidence" rather than "I found contradicting evidence."
func isAbsenceOfEvidence(evidence string) bool {
	lower := strings.ToLower(evidence)
	absenceMarkers := []string{
		"does not provide",
		"does not include",
		"does not cite",
		"no evidence",
		"no direct evidence",
		"not provide",
		"not cited",
		"not demonstrated",
		"not found",
		"no specific evidence",
		"no studies",
		"only discuss",
		"only demonstrate",
		"context does not",
	}
	for _, marker := range absenceMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// getCandidates returns epistemic entries eligible for verification.
func (v *Verifier) getCandidates(ctx context.Context, minEncounters int, maxEntries int, force bool) ([]RegistryEntry, error) {
	var targetStatus []Status
	if force {
		// Re-verify everything (all statuses)
		targetStatus = []Status{StatusUnknown, StatusVerified, StatusContested, StatusFalse}
	} else {
		// Only verify unknowns
		targetStatus = []Status{StatusUnknown}
	}

	var candidates []RegistryEntry
	for _, status := range targetStatus {
		entries, err := v.registry.GetByStatus(ctx, status)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.EncounterCount >= minEncounters {
				// Pre-flight filter: demote obvious garbage before spending Ollama calls
				if v.isPreFlightGarbage(e) {
					if !v.dryRun {
						slog.Info("pre-flight pruned", "triple", e.Canonical, "reason", "structural garbage")
						v.demoteToDefinitional(ctx, e.Canonical)
					} else {
						slog.Info("pre-flight pruned", "dry_run", true, "triple", e.Canonical, "reason", "structural garbage")
					}
					continue
				}
				candidates = append(candidates, e)
			}
		}
	}

	// Sort by encounter count descending (most-seen first = highest priority)
	sortByEncounters(candidates)

	// Cap
	if len(candidates) > maxEntries {
		candidates = candidates[:maxEntries]
	}

	return candidates, nil
}

// isPreFlightGarbage checks if an existing registry entry is obviously garbage
// that should be pruned without spending an Ollama call on verification.
// This catches entries that were extracted before the domain.go filters existed.
func (v *Verifier) isPreFlightGarbage(entry RegistryEntry) bool {
	subj := entry.Subject
	obj := entry.Object
	verb := entry.Verb

	// Tautology: subject == object
	if subj == obj {
		return true
	}

	// Session-noise terms
	noiseTerms := map[string]bool{
		"done": true, "tool_uses": true, "tool_use": true,
		"response": true, "message": true, "ok": true,
		"yes": true, "no": true, "true": true, "false": true,
		"null": true, "undefined": true, "interrupted": true,
		"completed": true, "finished": true, "enabled": true, "disabled": true,
	}
	if noiseTerms[subj] || noiseTerms[obj] {
		return true
	}

	// Config prefix patterns
	configPrefixes := []string{"enable_", "disable_", "config_", "set_"}
	for _, prefix := range configPrefixes {
		if len(subj) >= len(prefix) && subj[:len(prefix)] == prefix {
			return true
		}
		if len(obj) >= len(prefix) && obj[:len(prefix)] == prefix {
			return true
		}
	}

	// Structural vagueness: bare short term + is/linked verb
	if verb == "is" || verb == "linked" {
		if isVagueWithLen(subj, v.cfg.VagueMaxLen) || isVagueWithLen(obj, v.cfg.VagueMaxLen) {
			return true
		}
	}

	return false
}

// isVagueWithLen returns true if a term is too short and uncompounded to be meaningful.
func isVagueWithLen(term string, maxLen int) bool {
	if strings.Contains(term, "_") {
		return false
	}
	return len(term) <= maxLen
}

// gatherSourceContext pulls content from the source entries that contain this claim.
func (v *Verifier) gatherSourceContext(ctx context.Context, entry RegistryEntry) string {
	var chunks []string
	for _, sourceID := range entry.SourceEntries {
		sourceID = strings.TrimSpace(sourceID)
		if sourceID == "" {
			continue
		}
		// Try with entry: prefix
		vals, err := v.rdb.HGetAll(ctx, "entry:"+sourceID).Result()
		if err != nil || len(vals) == 0 {
			vals, _ = v.rdb.HGetAll(ctx, sourceID).Result()
		}
		if content, ok := vals["content"]; ok && content != "" {
			// Truncate individual sources
			if len(content) > v.cfg.SourceContextMax {
				content = content[:v.cfg.SourceContextMax] + "..."
			}
			chunks = append(chunks, content)
		}
		// Cap at MaxSourceEntries
		if len(chunks) >= v.cfg.MaxSourceEntries {
			break
		}
	}
	return strings.Join(chunks, "\n---\n")
}



// sortByEncounters sorts entries by encounter count, highest first.
func sortByEncounters(entries []RegistryEntry) {
	for i := 0; i < len(entries)-1; i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].EncounterCount > entries[i].EncounterCount {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

// demoteToDefinitional removes a triple from the verification pool entirely.
// It moves it to a "pruned" status set so it's never re-verified, and records
// the reason. The entry stays in the registry for historical record but won't
// appear in verification candidates or recall-time warnings.
func (v *Verifier) demoteToDefinitional(ctx context.Context, canonical string) error {
	key := keyPrefix + canonical

	// Get current status for removal from old set
	oldStatus, _ := v.rdb.HGet(ctx, key, "status").Result()

	pipe := v.rdb.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"status":      "pruned",
		"verified_by": "verifier:auto-prune:unfalsifiable",
	})
	// Remove from old status set
	if oldStatus != "" {
		pipe.SRem(ctx, statusSetPrefix+oldStatus, canonical)
	}
	// Add to pruned set (never re-verified)
	pipe.SAdd(ctx, statusSetPrefix+"pruned", canonical)
	_, err := pipe.Exec(ctx)
	return err
}
