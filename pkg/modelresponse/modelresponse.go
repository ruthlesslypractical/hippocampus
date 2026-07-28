// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package modelresponse provides utilities for parsing structured responses
// from local LLM (Ollama) calls. It handles the common patterns:
//
//   - Prefix-keyed lines: "SIGNAL: accept mild implicit"
//   - JSON objects: {"verdict": "accept", "confidence": 0.9}
//   - JSON arrays: [{"subject": "x", "verb": "causes", "object": "y"}]
//
// All parsers accept configurable truncation limits to avoid logging huge
// model outputs — no magic numbers, all limits come from the caller.
package modelresponse

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParsePrefix extracts the payload from a line matching "PREFIX: <payload>".
// Matching is case-insensitive on the prefix. Returns the trimmed payload string.
//
// Example:
//
//	payload, err := ParsePrefix(response, "SIGNAL:")
//	// payload = "accept mild implicit"
func ParsePrefix(response, prefix string) (string, error) {
	lines := strings.Split(strings.TrimSpace(response), "\n")
	upperPrefix := strings.ToUpper(strings.TrimSpace(prefix))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), upperPrefix) {
			return strings.TrimSpace(line[len(prefix):]), nil
		}
	}
	return "", fmt.Errorf("response did not contain expected %q prefix", prefix)
}

// ParseJSON extracts the first valid JSON object from response and unmarshals
// it into T. Handles LLM preamble/postamble — only the JSON object matters.
//
// Example:
//
//	type VerifyResult struct { Verdict string; Confidence float64 }
//	result, err := ParseJSON[VerifyResult](response)
func ParseJSON[T any](response string) (T, error) {
	var zero T
	start := strings.Index(response, "{")
	end := strings.LastIndex(response, "}")
	if start == -1 || end <= start {
		return zero, fmt.Errorf("no JSON object found in response")
	}
	if err := json.Unmarshal([]byte(response[start:end+1]), &zero); err != nil {
		return zero, fmt.Errorf("unmarshal JSON object: %w", err)
	}
	return zero, nil
}

// ParseJSONArray extracts the first valid JSON array from response and
// unmarshals it into []T. Handles LLM preamble/postamble.
// If the array has a trailing comma (a common LLM artifact), it is cleaned
// before parsing.
//
// Example:
//
//	type Triple struct { Subject, Verb, Object string }
//	triples, err := ParseJSONArray[Triple](response)
func ParseJSONArray[T any](response string) ([]T, error) {
	start := strings.Index(response, "[")
	end := strings.LastIndex(response, "]")
	if start == -1 || end <= start {
		return nil, fmt.Errorf("no JSON array found in response")
	}
	raw := response[start : end+1]

	var result []T
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		// Common LLM artifact: trailing comma before closing bracket
		// e.g. [{"a":"b"},{"c":"d"},]
		cleaned := strings.TrimRight(strings.TrimSpace(raw[:len(raw)-1]), ",") + "]"
		if err2 := json.Unmarshal([]byte(cleaned), &result); err2 != nil {
			return nil, fmt.Errorf("unmarshal JSON array: %w", err)
		}
	}
	return result, nil
}

// ParseJSONWithFallbackArray tries ParseJSON first, then ParseJSONArray.
// Useful when the LLM sometimes wraps a single object in an array.
func ParseJSONWithFallbackArray[T any](response string) (T, error) {
	// Try object first
	if result, err := ParseJSON[T](response); err == nil {
		return result, nil
	}
	// Try extracting first element of array
	var zero T
	type wrapper []T
	arr, err := ParseJSONArray[T](response)
	if err != nil || len(arr) == 0 {
		return zero, fmt.Errorf("no valid JSON object or array found in response")
	}
	return arr[0], nil
}

// Truncate returns s truncated to maxLen characters, with "…" appended
// if truncation occurred. Used for error messages to avoid logging huge outputs.
// maxLen must be > 0.
func Truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
