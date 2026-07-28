// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/pkg/modelresponse"
)

// OFC — Orbitofrontal Cortex module
// Maintains persistent DA (reward/error) and 5HT (ambient mood) signals
// that modulate the agent's behavioral output via prompt injection.

const (
	ofcDAKey  = "meta:chip:da"
	ofcSHTKey = "meta:chip:5ht"
)

// ofcSignal represents the classified sentiment of a user prompt.
type ofcSignal struct {
	polarity  int     // -1 negative, 0 neutral, 1 positive
	intensity float64 // 0.0 to 1.0 (mild → strong)
	implicit  bool    // true = inferred rather than explicit
}

// ofcUpdate reads the current prompt, detects reward signals, updates DA/5HT.
func ofcUpdate(ctx context.Context, client *redis.Client, prompt string, ofc config.OFCConfig, ollamaURL string) {
	baseline := ofc.SHTBaseline
	if baseline == 0 {
		baseline = 0.5
	}

	da := ofcLoadFloat(ctx, client, ofcDAKey, 0.0)
	sht := ofcLoadFloat(ctx, client, ofcSHTKey, baseline)

	// Detect signal: try model first, fall back to regex
	signal := ofcClassify(ctx, prompt, ofc, ollamaURL)

	// Apply signal to DA/5HT
	switch {
	case signal.polarity == 1 && !signal.implicit:
		da += ofc.DAExplicitPositive * signal.intensity
		sht += ofc.SHTPositive * signal.intensity
	case signal.polarity == 1 && signal.implicit:
		da += ofc.DAImplicitPositive * signal.intensity
		sht += ofc.SHTPositive * 0.5 * signal.intensity
	case signal.polarity == -1 && !signal.implicit:
		da += ofc.DAExplicitNegative * signal.intensity
		sht += ofc.SHTNegative * signal.intensity
	case signal.polarity == -1 && signal.implicit:
		da += ofc.DAImplicitNegative * signal.intensity
		sht += ofc.SHTNegative * 0.5 * signal.intensity
	}

	// Apply decay (mean reversion)
	daDecay := ofc.DADecay
	if daDecay == 0 {
		daDecay = 0.95
	}
	shtDecay := ofc.SHTDecay
	if shtDecay == 0 {
		shtDecay = 0.98
	}

	da *= daDecay
	sht = sht*shtDecay + baseline*(1-shtDecay)

	// Clamp
	da = clampF(da, -1.0, 1.0)
	sht = clampF(sht, 0.0, 1.0)

	// Store
	ofcStoreFloat(ctx, client, ofcDAKey, da)
	ofcStoreFloat(ctx, client, ofcSHTKey, sht)
}

// ofcClassify attempts model-based sentiment classification, falling back to regex.
func ofcClassify(ctx context.Context, prompt string, ofc config.OFCConfig, ollamaURL string) ofcSignal {
	// If no model configured, use regex directly
	if ofc.Model == "" {
		return ofcRegexFallback(prompt)
	}

	// Try Ollama classification with a tight timeout
	classifyTimeoutS := ofc.ClassifyTimeoutS
	if classifyTimeoutS <= 0 {
		classifyTimeoutS = 3
	}
	modelCtx, cancel := context.WithTimeout(ctx, time.Duration(classifyTimeoutS)*time.Second)
	defer cancel()

	signal, err := ofcModelClassify(modelCtx, prompt, ofc.Model, ollamaURL)
	if err != nil {
		// Model failed — fall back to regex
		return ofcRegexFallback(prompt)
	}
	return signal
}

// ofcModelClassify calls the configured Ollama model for sentiment analysis.
// The key insight: we're not scoring the emotional tone of the user's message —
// we're scoring whether they're ACCEPTING or REJECTING the previous assistant output.
// Tool errors, build failures, and exploration are invisible to this classifier
// because they only appear in assistant turns, not user turns.
func ofcModelClassify(ctx context.Context, prompt, model, ollamaURL string) (ofcSignal, error) {
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	client := ollama.New(ollamaURL, model, 1)

	classPrompt := fmt.Sprintf(`Classify whether this user message ACCEPTS, REJECTS, or REDIRECTS the previous assistant response.

Reply with EXACTLY one line in this format:
SIGNAL: <verdict> <intensity> <type>

Where:
- verdict: accept, reject, or redirect
- intensity: mild or strong
- type: explicit (user directly states acceptance/rejection) or implicit (inferred from behavior)

Definitions:
- ACCEPT: user confirms the output was correct, moves forward with it, or expresses satisfaction
- REJECT: user corrects the output, says it was wrong, asks to redo or undo it
- REDIRECT: user asks a new question, changes topic, or elaborates without accepting/rejecting

Examples:
- "Yeah." → SIGNAL: accept mild implicit
- "Yeah, let's do it." → SIGNAL: accept strong explicit
- "OK, next thing:" → SIGNAL: accept mild implicit
- "No, that's wrong" → SIGNAL: reject strong explicit
- "Hmm. Not quite." → SIGNAL: reject mild implicit
- "What about X?" → SIGNAL: redirect mild implicit
- "Wait, actually..." → SIGNAL: reject mild explicit
- "Perfect." → SIGNAL: accept strong explicit
- "Can you explain X?" → SIGNAL: redirect mild implicit

Message: %s`, prompt)

	result, err := client.GenerateWithOptions(ctx, classPrompt, model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  20,
	})
	if err != nil {
		return ofcSignal{}, err
	}

	return parseModelResponse(result)
}

// parseModelResponse extracts structured signal from model output using
// pkg/modelresponse.ParsePrefix. Handles both "SIGNAL:" (new) and
// "SENTIMENT:" (legacy) prefixes.
func parseModelResponse(response string) (ofcSignal, error) {
	// Try new SIGNAL: format first, fall back to legacy SENTIMENT:
	payload, err := modelresponse.ParsePrefix(response, "SIGNAL:")
	if err != nil {
		payload, err = modelresponse.ParsePrefix(response, "SENTIMENT:")
		if err != nil {
			return ofcSignal{}, fmt.Errorf("could not parse signal from model response: %s",
				modelresponse.Truncate(response, 200))
		}
	}

	parts := strings.Fields(payload)
	if len(parts) < 2 {
		return ofcSignal{}, fmt.Errorf("too few fields in signal payload: %q", payload)
	}

	var sig ofcSignal

	switch strings.ToLower(parts[0]) {
	case "accept", "positive":
		sig.polarity = 1
	case "reject", "negative":
		sig.polarity = -1
	case "redirect", "neutral":
		sig.polarity = 0
	default:
		sig.polarity = 0
	}

	switch strings.ToLower(parts[1]) {
	case "strong":
		sig.intensity = 1.0
	case "mild":
		sig.intensity = 0.5
	default:
		sig.intensity = 0.7
	}

	if len(parts) >= 3 {
		sig.implicit = strings.ToLower(parts[2]) == "implicit"
	}

	return sig, nil
}

// --- Regex fallback (used when model is unavailable) ---

// Accept signals: user is moving forward with the prior output
var acceptPatterns = []string{
	"yeah", "yes", "yep", "yup", "good", "nice", "correct", "perfect", "exactly",
	"that worked", "ship it", "do it", "nailed it", "spot on", "looks good",
	"love it", "great", "right", "bingo", "ok", "cool", "sweet", "let's do it",
}

// Reject signals: user is correcting or pushing back on the prior output
var rejectPatterns = []string{
	"no", "wrong", "not that", "try again", "undo", "revert",
	"that broke", "doesn't work", "that's not", "nope", "incorrect",
	"that failed", "still broken", "same error", "didn't work", "not quite",
	"wait actually", "hmm no", "that's wrong",
}

// ofcRegexFallback uses pattern matching when the model is unavailable.
// Maps accept→positive, reject→negative, everything else→neutral.
func ofcRegexFallback(prompt string) ofcSignal {
	promptLower := strings.ToLower(prompt)
	words := strings.Fields(promptLower)

	for _, pat := range rejectPatterns {
		if strings.Contains(promptLower, pat) {
			return ofcSignal{polarity: -1, intensity: 1.0, implicit: false}
		}
	}

	for _, pat := range acceptPatterns {
		if strings.Contains(promptLower, pat) {
			return ofcSignal{polarity: 1, intensity: 1.0, implicit: false}
		}
	}

	// Implicit accept: very short prompt with no question/correction markers
	// "OK." / "Yah." / "Sure." — user is proceeding, not redirecting
	if len(words) <= 3 && !strings.Contains(promptLower, "?") &&
		!strings.Contains(promptLower, "why") && !strings.Contains(promptLower, "what") {
		return ofcSignal{polarity: 1, intensity: 0.3, implicit: true}
	}

	// Everything else is a redirect (neutral) — new question, new topic, elaboration
	return ofcSignal{polarity: 0, intensity: 0, implicit: true}
}

// ofcFormatBlock generates the [NEUROMODULATOR STATE] injection text.
func ofcFormatBlock(ctx context.Context, client *redis.Client, ofc config.OFCConfig) string {
	baseline := ofc.SHTBaseline
	if baseline == 0 {
		baseline = 0.5
	}

	da := ofcLoadFloat(ctx, client, ofcDAKey, 0.0)
	sht := ofcLoadFloat(ctx, client, ofcSHTKey, baseline)

	var out strings.Builder
	out.WriteString("\n[NEUROMODULATOR STATE]\n")
	out.WriteString(fmt.Sprintf("DA: %+.2f | 5HT: %.2f\n", da, sht))
	out.WriteString(ofcDADirective(da))
	out.WriteString(ofcSHTDirective(sht))
	out.WriteString("[END NEUROMODULATOR STATE]\n")

	return out.String()
}

func ofcDADirective(da float64) string {
	switch {
	case da > 0.3:
		return "DA directive: You're on a streak. Trust your instincts, be decisive, don't over-deliberate.\n"
	case da > 0.1:
		return "DA directive: Things are going well. Continue current approach.\n"
	case da > -0.1:
		return "DA directive: Neutral. No strong reward signal.\n"
	case da > -0.3:
		return "DA directive: Recent setback. Double-check your work before committing. Consider alternatives.\n"
	default:
		return "DA directive: Multiple recent failures. STOP and reassess. Ask clarifying questions. Don't repeat the same approach — try something fundamentally different.\n"
	}
}

func ofcSHTDirective(sht float64) string {
	switch {
	case sht > 0.7:
		return "5HT directive: Good mood. Be confident, direct, even playful where appropriate.\n"
	case sht > 0.4:
		return "5HT directive: Normal operating mode. Balanced tone.\n"
	case sht > 0.2:
		return "5HT directive: Below baseline. Be measured. Acknowledge difficulty honestly rather than forcing optimism.\n"
	default:
		return "5HT directive: Low mood. Something has been systemically difficult. Be honest about frustration. Suggest stepping back if appropriate.\n"
	}
}

// --- Helpers ---

func ofcLoadFloat(ctx context.Context, client *redis.Client, key string, defaultVal float64) float64 {
	val, err := client.Get(ctx, key).Result()
	if err != nil {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

func ofcStoreFloat(ctx context.Context, client *redis.Client, key string, val float64) {
	client.Set(ctx, key, fmt.Sprintf("%.4f", val), 0)
}

func clampF(v, min, max float64) float64 {
	return math.Max(min, math.Min(max, v))
}
