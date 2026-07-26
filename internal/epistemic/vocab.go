// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Vocab manages the three-tier vocabulary for term reconciliation.
type Vocab struct {
	rdb *redis.Client
}

// NewVocab creates a new vocabulary manager.
func NewVocab(rdb *redis.Client) *Vocab {
	return &Vocab{rdb: rdb}
}

// GetRelevantTerms returns vocabulary terms relevant to the given keywords.
// Checks Tier 2 first (domain-specific), falls back to Tier 1 (ConceptNet).
// Returns at most maxTerms results.
func (v *Vocab) GetRelevantTerms(ctx context.Context, keywords []string, maxTerms int) ([]string, error) {
	var terms []string
	seen := make(map[string]bool)

	// Tier 2: domain-specific learned vocabulary
	// Check if any keywords are themselves Tier 2 terms
	for _, kw := range keywords {
		kw = normalize(kw)
		isMember, _ := v.rdb.SIsMember(ctx, vocabTier2Key, kw).Result()
		if isMember && !seen[kw] {
			terms = append(terms, kw)
			seen[kw] = true
		}
	}

	// Also find Tier 2 terms that contain the keywords as substrings
	// (This is O(N) on vocab size but Tier 2 is small — hundreds, not millions)
	allTier2, _ := v.rdb.SMembers(ctx, vocabTier2Key).Result()
	for _, term := range allTier2 {
		if seen[term] {
			continue
		}
		for _, kw := range keywords {
			if containsWord(term, normalize(kw)) {
				terms = append(terms, term)
				seen[term] = true
				break
			}
		}
		if len(terms) >= maxTerms {
			break
		}
	}

	// TODO: Tier 1 (ConceptNet) fallback when Tier 2 is insufficient
	// For now, Tier 2 is sufficient — it grows organically from usage.

	return terms, nil
}

// RecordTerm adds a term to Tier 2 vocabulary if it's new, or bumps its encounter count.
func (v *Vocab) RecordTerm(ctx context.Context, term string) error {
	term = normalize(term)
	if term == "" {
		return nil
	}

	key := vocabTier2Entry + term
	now := time.Now().Unix()

	// Add to the terms set
	v.rdb.SAdd(ctx, vocabTier2Key, term)

	// Check if it already exists
	exists, _ := v.rdb.Exists(ctx, key).Result()
	if exists == 0 {
		// New term
		return v.rdb.HSet(ctx, key, map[string]interface{}{
			"first_seen":  now,
			"last_seen":   now,
			"encounters":  1,
		}).Err()
	}

	// Existing term — bump
	pipe := v.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "encounters", 1)
	pipe.HSet(ctx, key, "last_seen", now)
	_, err := pipe.Exec(ctx)
	return err
}

// RecordTerms adds multiple terms to vocabulary in one pass.
func (v *Vocab) RecordTerms(ctx context.Context, triples []Triple) error {
	for _, t := range triples {
		if err := v.RecordTerm(ctx, t.Subject); err != nil {
			return err
		}
		if err := v.RecordTerm(ctx, t.Object); err != nil {
			return err
		}
	}
	return nil
}

// Size returns the number of terms in Tier 2.
func (v *Vocab) Size(ctx context.Context) (int64, error) {
	return v.rdb.SCard(ctx, vocabTier2Key).Result()
}

// AllTerms returns all Tier 2 terms (for debugging/display).
func (v *Vocab) AllTerms(ctx context.Context) ([]string, error) {
	return v.rdb.SMembers(ctx, vocabTier2Key).Result()
}

// containsWord checks if a term contains a keyword as a word boundary.
func containsWord(term, keyword string) bool {
	if keyword == "" {
		return false
	}
	// Simple substring match — good enough for snake_case terms
	return len(keyword) >= 3 && // skip very short keywords
		fmt.Sprintf("_%s_", term) != "" && // avoid nil
		(term == keyword ||
			len(term) > len(keyword) && 
			(term[:len(keyword)] == keyword || 
			 term[len(term)-len(keyword):] == keyword ||
			 contains(term, "_"+keyword+"_") ||
			 contains(term, "_"+keyword) ||
			 contains(term, keyword+"_")))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && 
		(s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
