// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package epistemic

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyPrefix       = "epistemic:"
	vocabTier2Key   = "vocab:tier2:terms"
	vocabTier2Entry = "vocab:tier2:"
	statusSetPrefix = "epistemic:status:"
	bySubjectPrefix = "epistemic:by_subject:"
	byObjectPrefix  = "epistemic:by_object:"
)

// Registry manages the epistemic hash store in Redis.
type Registry struct {
	rdb *redis.Client
}

// NewRegistry creates a new epistemic registry backed by Redis.
func NewRegistry(rdb *redis.Client) *Registry {
	return &Registry{rdb: rdb}
}

// Record stores or updates a triple encounter in the registry.
// Returns true if this is a new entry, false if it was an existing encounter.
func (r *Registry) Record(ctx context.Context, triple Triple, sourceEntryID string) (bool, error) {
	canonical := triple.Canonical()
	key := keyPrefix + canonical
	verb := NormalizeVerb(triple.Relation)
	subject := normalize(triple.Subject)
	object := normalize(triple.Object)
	now := time.Now()

	// Check if entry exists
	exists, err := r.rdb.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("check exists: %w", err)
	}

	if exists == 0 {
		// New entry
		pipe := r.rdb.Pipeline()
		pipe.HSet(ctx, key, map[string]interface{}{
			"canonical":       canonical,
			"subject":         subject,
			"verb":            verb,
			"object":          object,
			"status":          string(StatusUnknown),
			"confidence":      "0.5",
			"first_seen":      now.Unix(),
			"last_seen":       now.Unix(),
			"encounter_count": 1,
			"source_entries":  sourceEntryID,
			"evidence_for":    "",
			"evidence_against": "",
			"verified_by":     "",
		})
		// Index by status
		pipe.SAdd(ctx, statusSetPrefix+string(StatusUnknown), canonical)
		// Index by subject/object for recall-time lookup
		pipe.SAdd(ctx, bySubjectPrefix+subject, canonical)
		pipe.SAdd(ctx, byObjectPrefix+object, canonical)
		_, err = pipe.Exec(ctx)
		return true, err
	}

	// Existing entry — bump encounter count and last_seen
	pipe := r.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "encounter_count", 1)
	pipe.HSet(ctx, key, "last_seen", now.Unix())
	// Append source entry to list (comma-separated)
	pipe.HGet(ctx, key, "source_entries")
	results, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return false, fmt.Errorf("update entry: %w", err)
	}

	// Append source entry ID
	if len(results) >= 3 {
		if existing, err := results[2].(*redis.StringCmd).Result(); err == nil {
			entries := existing + "," + sourceEntryID
			r.rdb.HSet(ctx, key, "source_entries", entries)
		}
	}

	return false, nil
}

// GetBySubject returns all epistemic entries where the subject matches a keyword.
func (r *Registry) GetBySubject(ctx context.Context, subject string) ([]RegistryEntry, error) {
	subject = normalize(subject)
	canonicals, err := r.rdb.SMembers(ctx, bySubjectPrefix+subject).Result()
	if err != nil {
		return nil, err
	}
	return r.getEntries(ctx, canonicals)
}

// GetByObject returns all epistemic entries where the object matches a keyword.
func (r *Registry) GetByObject(ctx context.Context, object string) ([]RegistryEntry, error) {
	object = normalize(object)
	canonicals, err := r.rdb.SMembers(ctx, byObjectPrefix+object).Result()
	if err != nil {
		return nil, err
	}
	return r.getEntries(ctx, canonicals)
}

// GetByStatus returns all entries with a given status.
func (r *Registry) GetByStatus(ctx context.Context, status Status) ([]RegistryEntry, error) {
	canonicals, err := r.rdb.SMembers(ctx, statusSetPrefix+string(status)).Result()
	if err != nil {
		return nil, err
	}
	return r.getEntries(ctx, canonicals)
}

// MatchKeywords checks if any of the given keywords appear as subjects or objects
// in contested/false epistemic entries. Returns matching entries for warning injection.
func (r *Registry) MatchKeywords(ctx context.Context, keywords []string) ([]RegistryEntry, error) {
	var matches []RegistryEntry
	for _, kw := range keywords {
		kw = normalize(kw)
		// Check subjects
		canonicals, _ := r.rdb.SMembers(ctx, bySubjectPrefix+kw).Result()
		for _, c := range canonicals {
			entry, err := r.getEntry(ctx, c)
			if err == nil && (entry.Status == StatusFalse || entry.Status == StatusContested) {
				matches = append(matches, entry)
			}
		}
		// Check objects
		canonicals, _ = r.rdb.SMembers(ctx, byObjectPrefix+kw).Result()
		for _, c := range canonicals {
			entry, err := r.getEntry(ctx, c)
			if err == nil && (entry.Status == StatusFalse || entry.Status == StatusContested) {
				matches = append(matches, entry)
			}
		}
	}
	return dedupEntries(matches), nil
}

// UpdateStatus changes the status of an epistemic entry.
func (r *Registry) UpdateStatus(ctx context.Context, canonical string, status Status, confidence float64, evidenceFor, evidenceAgainst, verifiedBy string) error {
	key := keyPrefix + canonical
	oldStatus, _ := r.rdb.HGet(ctx, key, "status").Result()

	pipe := r.rdb.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"status":           string(status),
		"confidence":       fmt.Sprintf("%.3f", confidence),
		"evidence_for":     evidenceFor,
		"evidence_against":  evidenceAgainst,
		"verified_by":      verifiedBy,
	})
	// Move between status sets
	if oldStatus != "" {
		pipe.SRem(ctx, statusSetPrefix+oldStatus, canonical)
	}
	pipe.SAdd(ctx, statusSetPrefix+string(status), canonical)
	_, err := pipe.Exec(ctx)
	return err
}

// Stats returns counts of entries by status.
func (r *Registry) Stats(ctx context.Context) (map[Status]int64, error) {
	stats := make(map[Status]int64)
	for _, s := range []Status{StatusUnknown, StatusVerified, StatusContested, StatusFalse} {
		count, err := r.rdb.SCard(ctx, statusSetPrefix+string(s)).Result()
		if err != nil {
			return nil, err
		}
		stats[s] = count
	}
	return stats, nil
}

func (r *Registry) getEntries(ctx context.Context, canonicals []string) ([]RegistryEntry, error) {
	var entries []RegistryEntry
	for _, c := range canonicals {
		entry, err := r.getEntry(ctx, c)
		if err == nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (r *Registry) getEntry(ctx context.Context, canonical string) (RegistryEntry, error) {
	key := keyPrefix + canonical
	vals, err := r.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(vals) == 0 {
		return RegistryEntry{}, fmt.Errorf("entry not found: %s", canonical)
	}

	count, _ := strconv.Atoi(vals["encounter_count"])
	confidence, _ := strconv.ParseFloat(vals["confidence"], 64)
	firstSeen, _ := strconv.ParseInt(vals["first_seen"], 10, 64)
	lastSeen, _ := strconv.ParseInt(vals["last_seen"], 10, 64)

	return RegistryEntry{
		Canonical:       vals["canonical"],
		Subject:         vals["subject"],
		Verb:            vals["verb"],
		Object:          vals["object"],
		Status:          Status(vals["status"]),
		Confidence:      confidence,
		FirstSeen:       time.Unix(firstSeen, 0),
		LastSeen:        time.Unix(lastSeen, 0),
		EncounterCount:  count,
		SourceEntries:   strings.Split(vals["source_entries"], ","),
		EvidenceFor:     vals["evidence_for"],
		EvidenceAgainst: vals["evidence_against"],
		VerifiedBy:      vals["verified_by"],
	}, nil
}

func dedupEntries(entries []RegistryEntry) []RegistryEntry {
	seen := make(map[string]bool)
	var result []RegistryEntry
	for _, e := range entries {
		if !seen[e.Canonical] {
			seen[e.Canonical] = true
			result = append(result, e)
		}
	}
	return result
}

// PurgePruned removes all entries with status "pruned" from Redis entirely.
// This reclaims keys: the epistemic hash, subject/object index sets, and status set membership.
// Returns the number of entries purged.
func (r *Registry) PurgePruned(ctx context.Context) (int, error) {
	// Get all pruned canonicals
	canonicals, err := r.rdb.SMembers(ctx, statusSetPrefix+"pruned").Result()
	if err != nil {
		return 0, fmt.Errorf("get pruned set: %w", err)
	}

	if len(canonicals) == 0 {
		return 0, nil
	}

	purged := 0
	for _, canonical := range canonicals {
		key := keyPrefix + canonical

		// Get subject/object for index cleanup
		vals, err := r.rdb.HGetAll(ctx, key).Result()
		if err != nil || len(vals) == 0 {
			// Key already gone — just remove from status set
			r.rdb.SRem(ctx, statusSetPrefix+"pruned", canonical)
			purged++
			continue
		}

		subject := vals["subject"]
		object := vals["object"]

		pipe := r.rdb.Pipeline()
		// Remove the hash entry itself
		pipe.Del(ctx, key)
		// Remove from subject index
		if subject != "" {
			pipe.SRem(ctx, bySubjectPrefix+subject, canonical)
		}
		// Remove from object index
		if object != "" {
			pipe.SRem(ctx, byObjectPrefix+object, canonical)
		}
		// Remove from pruned status set
		pipe.SRem(ctx, statusSetPrefix+"pruned", canonical)
		_, err = pipe.Exec(ctx)
		if err != nil {
			continue
		}
		purged++
	}

	return purged, nil
}
