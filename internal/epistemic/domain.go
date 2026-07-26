// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import "strings"

// Domain represents the epistemic domain of a claim — whether it's
// the kind of thing that CAN be fact-checked at all.
type Domain string

const (
	// DomainFactual — claims about the physical world, math, history, science.
	// These can be verified against evidence. "Pi = 3.14", "sevoflurane activates C1q"
	DomainFactual Domain = "factual"

	// DomainInferential — claims that follow from evidence but aren't directly proven.
	// "Iron depletion likely causes frustrated drive phenotype." Supportable but not certain.
	DomainInferential Domain = "inferential"

	// DomainOpinion — value judgments, preferences, aesthetics, politics.
	// NOT fact-checkable. "SQLite is underrated." "Go is better than Python."
	// These get recorded but NEVER enter the verification pipeline.
	DomainOpinion Domain = "opinion"

	// DomainDefinitional — tautologies, definitions, naming conventions.
	// True by definition, not worth verifying. "A mammal is a warm-blooded vertebrate."
	DomainDefinitional Domain = "definitional"
)

// domainSignals maps keywords/patterns that suggest a claim is opinion rather than fact.
// This is a heuristic — not perfect, but catches the obvious cases.
var opinionSignals = []string{
	"underrated",
	"overrated",
	"better",
	"worse",
	"best",
	"worst",
	"should",
	"ought",
	"prefer",
	"beautiful",
	"ugly",
	"stupid",
	"smart",
	"evil",
	"good",
	"bad",
	"right",
	"wrong",
	"moral",
	"immoral",
	"fair",
	"unfair",
}

// inferentialSignals suggest a claim is inference rather than hard fact.
var inferentialSignals = []string{
	"likely",
	"probably",
	"suggests",
	"may",
	"might",
	"could",
	"possibly",
	"hypothes",
	"plausible",
	"consistent_with",
	"associated",
	"correlated",
	"implies",
	"indicates",
}

// sessionNoiseTerms are subjects/objects that leak from session metadata
// into the extraction pipeline. These are never meaningful claims.
var sessionNoiseTerms = map[string]bool{
	"done":       true,
	"tool_uses":  true,
	"tool_use":   true,
	"response":   true,
	"message":    true,
	"ok":         true,
	"yes":        true,
	"no":         true,
	"true":       true,
	"false":      true,
	"null":       true,
	"undefined":  true,
	"interrupted": true,
	"completed":  true,
	"finished":   true,
}

// metaConfigPrefixes identify configuration-state triples that look like
// "enable_sleep|is|true" — technically correct assertions but not epistemic claims.
var metaConfigPrefixes = []string{
	"enable_",
	"disable_",
	"config_",
	"set_",
	"is_enabled",
	"is_disabled",
	"is_configured",
	"is_set",
	"is_true",
	"is_false",
}

// ClassifyDomain determines the epistemic domain of a triple.
// vagueMaxLen controls how short a bare (non-compound) term can be before
// it's considered too vague to carry epistemic content.
// This decides whether the claim enters the verification pipeline at all.
func ClassifyDomain(triple Triple, vagueMaxLen int) Domain {
	subj := normalize(triple.Subject)
	obj := normalize(triple.Object)
	combined := subj + " " + normalize(triple.Relation) + " " + obj

	// --- Garbage/noise filters (cheap, run first) ---

	// Tautology: subject == object is definitional noise ("done|is|done")
	if subj == obj {
		return DomainDefinitional
	}

	// Session-noise blocklist
	if sessionNoiseTerms[subj] || sessionNoiseTerms[obj] {
		return DomainDefinitional
	}

	// Meta/config patterns: "enable_sleep|is|true" etc.
	for _, prefix := range metaConfigPrefixes {
		if hasPrefix(subj, prefix) || hasPrefix(obj, prefix) {
			return DomainDefinitional
		}
	}

	// Structural specificity: if the object is a bare, short, uncompounded term
	// and the verb is "is" or "linked", it's too vague to be a meaningful claim.
	// Catches: "valkey|is|linked", "infra|linked|freebsd" (bare nouns with no specificity)
	rel := normalize(triple.Relation)
	if rel == "is" || rel == "linked" {
		if isStructurallyVague(obj, vagueMaxLen) {
			return DomainDefinitional
		}
		if isStructurallyVague(subj, vagueMaxLen) {
			return DomainDefinitional
		}
	}

	// --- Epistemic classification ---

	// Check for opinion signals
	for _, signal := range opinionSignals {
		if containsSubstring(combined, signal) {
			return DomainOpinion
		}
	}

	// Check for inferential signals
	for _, signal := range inferentialSignals {
		if containsSubstring(combined, signal) {
			return DomainInferential
		}
	}

	// Check if the relation itself is a value judgment
	if rel == "is" {
		for _, signal := range opinionSignals {
			if containsSubstring(obj, signal) {
				return DomainOpinion
			}
		}
	}

	// Default: factual (can be verified)
	return DomainFactual
}

// isStructurallyVague returns true if a term is too short and uncompounded to
// carry meaningful epistemic content. A compound term like "synaptic_elimination"
// is specific; a bare word like "linked" or "set" is not.
func isStructurallyVague(term string, maxLen int) bool {
	// Compound terms (contain underscore) are specific enough
	if strings.Contains(term, "_") {
		return false
	}
	// Short bare words (≤maxLen chars) with no compound structure are vague
	return len(term) <= maxLen
}

// hasPrefix checks if s starts with prefix.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// IsVerifiable returns true if a claim's domain allows fact-checking.
// Opinion and definitional claims are excluded from verification.
func IsVerifiable(domain Domain) bool {
	return domain == DomainFactual || domain == DomainInferential
}

func containsSubstring(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
