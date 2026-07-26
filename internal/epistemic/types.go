// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package epistemic implements semantic triple extraction, vocabulary reconciliation,
// and a persistent "wrongness registry" for detecting recurring incorrect assumptions.
package epistemic

import (
	"strings"
	"time"
)

// Triple represents a single semantic claim extracted from text.
type Triple struct {
	Subject  string `json:"subject"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
	Type     string `json:"type"` // "explicit" or "implicit"
}

// Canonical returns the hash key for this triple after verb normalization.
func (t Triple) Canonical() string {
	verb := NormalizeVerb(t.Relation)
	return normalize(t.Subject) + "|" + verb + "|" + normalize(t.Object)
}

// RegistryEntry represents a tracked epistemic claim in Redis.
type RegistryEntry struct {
	Canonical      string    `json:"canonical"`
	Subject        string    `json:"subject"`
	Verb           string    `json:"verb"`
	Object         string    `json:"object"`
	Status         Status    `json:"status"`
	Confidence     float64   `json:"confidence"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	EncounterCount int       `json:"encounter_count"`
	SourceEntries  []string  `json:"source_entries"`
	EvidenceFor    string    `json:"evidence_for"`
	EvidenceAgainst string  `json:"evidence_against"`
	VerifiedBy     string    `json:"verified_by"`
}

// Status represents the epistemic status of a claim.
type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusVerified  Status = "verified"
	StatusContested Status = "contested"
	StatusFalse     Status = "false"
)

// VerbCategory is one of the 5 super-categories all verbs collapse to.
type VerbCategory string

const (
	VerbCauses   VerbCategory = "causes"
	VerbPrevents VerbCategory = "prevents"
	VerbIs       VerbCategory = "is"
	VerbDistinct VerbCategory = "distinct"
	VerbLinked   VerbCategory = "linked"
)

// verbMap maps free-form relation strings to canonical verb categories.
var verbMap = map[string]VerbCategory{
	// causes family
	"causes":      VerbCauses,
	"triggers":    VerbCauses,
	"activates":   VerbCauses,
	"produces":    VerbCauses,
	"induces":     VerbCauses,
	"leads_to":    VerbCauses,
	"results_in":  VerbCauses,
	"generates":   VerbCauses,
	"stimulates":  VerbCauses,
	"increases":   VerbCauses,
	"enables":     VerbCauses,

	// prevents family
	"prevents":    VerbPrevents,
	"inhibits":    VerbPrevents,
	"blocks":      VerbPrevents,
	"reduces":     VerbPrevents,
	"suppresses":  VerbPrevents,
	"decreases":   VerbPrevents,
	"attenuates":  VerbPrevents,
	"protects":    VerbPrevents,

	// is family
	"is":          VerbIs,
	"is_a":        VerbIs,
	"same_as":     VerbIs,
	"is_part_of":  VerbIs,
	"contains":    VerbIs,
	"has_property": VerbIs,
	"instance_of": VerbIs,
	"subclass_of": VerbIs,

	// distinct family
	"distinct":      VerbDistinct,
	"distinct_from": VerbDistinct,
	"different_from": VerbDistinct,
	"contradicts":   VerbDistinct,
	"incompatible":  VerbDistinct,

	// linked family
	"linked":          VerbLinked,
	"associated_with": VerbLinked,
	"correlated_with": VerbLinked,
	"related_to":      VerbLinked,
	"co_occurs_with":  VerbLinked,
	"interacts_with":  VerbLinked,
}

// NormalizeVerb maps a free-form relation string to its canonical verb category.
// Returns the original string lowercased if no mapping is found.
func NormalizeVerb(rel string) string {
	lower := strings.ToLower(strings.TrimSpace(rel))
	if cat, ok := verbMap[lower]; ok {
		return string(cat)
	}
	// Try partial match (e.g., "does_not_cause" → strip prefix, try again)
	for k, v := range verbMap {
		if strings.Contains(lower, k) {
			return string(v)
		}
	}
	return lower
}

// normalize lowercases and trims a term for use in canonical keys.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
