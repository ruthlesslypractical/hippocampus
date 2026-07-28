// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import (
	"context"
	"fmt"
	"strings"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
	"github.com/ruthlesslypractical/hippocampus/pkg/modelresponse"
)

// Extractor handles triple extraction from text using Ollama.
type Extractor struct {
	client *ollama.Client
	cfg    config.EpistemicConfig
}

// NewExtractor creates a new triple extractor.
func NewExtractor(client *ollama.Client, cfg config.EpistemicConfig) *Extractor {
	return &Extractor{client: client, cfg: cfg}
}

// Extract extracts semantic triples from a paragraph of text.
// vocabulary is the set of existing canonical terms to reconcile against.
func (e *Extractor) Extract(ctx context.Context, text string, vocabulary []string) ([]Triple, error) {
	// No truncation — let the model's context window handle it.
	// If responses get truncated, bump num_ctx in Ollama config.

	prompt := buildExtractionPrompt(text, vocabulary)

	resp, err := e.client.GenerateWithOptions(ctx, prompt, e.client.Model, &ollama.GenerateOptions{
		Temperature: 0.1,
		NumPredict:  4096,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama generate: %w", err)
	}

	return parseTriples(resp, e.cfg.ResponseTrunc)
}

// buildExtractionPrompt constructs the prompt with vocabulary injection.
func buildExtractionPrompt(text string, vocabulary []string) string {
	var vocabSection string
	if len(vocabulary) > 0 {
		vocabSection = fmt.Sprintf(`
Map subjects and objects to these EXISTING canonical terms where applicable:
%s
If no canonical match, create a new snake_case term.`, strings.Join(vocabulary, ", "))
	} else {
		vocabSection = "\nUse snake_case for all subjects and objects. Be specific and consistent."
	}

	return fmt.Sprintf(`/no_think
You are an epistemic analysis system.

STEP 1: Extract ONLY claims about how things work in the real world — causal mechanisms, factual assertions, scientific hypotheses, economic relationships, medical claims, or predictions about observable phenomena.

DO NOT extract:
- Code architecture or software design descriptions ("the runner captures stdout")
- Build/deploy status ("the binary is rebuilt")
- Configuration states ("enable_sleep is true")
- Session metadata or tool operation descriptions
- Project relationships ("app X uses library Y")
- Tautologies or definitions ("an integer is a number")

If the text is purely about software implementation with no claims about the external world, output an empty array: []

STEP 2: Normalize each triple:
- Verbs: use ONLY one of: causes, prevents, is, distinct, linked
- Subjects/Objects: %s

TEXT:
%s

Output ONLY a JSON array. No markdown, no explanation:
[{"subject":"...","relation":"...","object":"...","type":"explicit|implicit"}]`, vocabSection, text)
}

// parseTriples parses the JSON response from Ollama into Triple structs.
// parseTriples extracts JSON triples from a raw LLM response.
// responseTrunc controls how many chars of the raw response appear in error messages (0 = use default 300).
func parseTriples(response string, responseTrunc int) ([]Triple, error) {
	if responseTrunc <= 0 {
		responseTrunc = 300
	}
	response = strings.TrimSpace(response)

	// Strip markdown code fences if present
	if strings.HasPrefix(response, "```") {
		lines := strings.Split(response, "\n")
		var clean []string
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				continue
			}
			clean = append(clean, line)
		}
		response = strings.Join(clean, "\n")
	}

	// Strip think blocks if present
	if idx := strings.Index(response, "</think>"); idx != -1 {
		response = strings.TrimSpace(response[idx+8:])
	}

	// Find the JSON array in the response
	start := strings.Index(response, "[")
	end := strings.LastIndex(response, "]")

	if start == -1 {
		// No array at all — could be empty response or pure garbage
		// Check for empty array indicators
		if strings.Contains(response, "[]") {
			return nil, nil
		}
		return nil, fmt.Errorf("no JSON array found in response: %s",
			modelresponse.Truncate(response, responseTrunc))
	}

	if end == -1 || end <= start {
		// Truncated response — array started but never closed.
		// Attempt recovery: find the last complete object (ends with "}")
		// and close the array there.
		lastBrace := strings.LastIndex(response[start:], "}")
		if lastBrace == -1 {
			return nil, fmt.Errorf("truncated response with no complete objects: %s",
				modelresponse.Truncate(response, responseTrunc))
		}
		response = response[start:start+lastBrace+1] + "]"
	} else {
		response = response[start : end+1]
	}

	// Use modelresponse.ParseJSONArray which handles trailing-comma cleanup
	triples, err := modelresponse.ParseJSONArray[Triple](response)
	if err != nil {
		return nil, fmt.Errorf("unmarshal triples: %w (response: %s)", err,
			modelresponse.Truncate(response, responseTrunc))
	}

	// Simon Says filter: only approved verbs survive. Everything else → shredder.
	valid := filterValidVerbs(triples)

	return valid, nil
}

// Approved verb set. Simon did not say "investment_increased".
var approvedVerbs = map[string]bool{
	"causes":   true,
	"prevents": true,
	"is":       true,
	"distinct": true,
	"linked":   true,
}

func filterValidVerbs(triples []Triple) []Triple {
	var clean []Triple
	for _, t := range triples {
		if approvedVerbs[t.Relation] {
			clean = append(clean, t)
		}
		// else: silently shredded. You had ONE job.
	}
	return clean
}
