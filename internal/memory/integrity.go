// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// IntegrityStatus constants for Entry.Integrity field.
const (
	IntegrityVerified   = "verified"   // content_hash matches recomputed hash
	IntegrityUnattested = "unattested" // no content_hash stored yet
	IntegrityFailed     = "FAILED"     // content_hash does NOT match — possible tampering
)

// VerifyEntryIntegrity checks a single entry's content_hash against its current content.
// Returns the integrity status string. Requires the raw Redis hash data (including content_hash field).
func VerifyEntryIntegrity(id, content, timestamp, storedHash string) string {
	if storedHash == "" {
		return IntegrityUnattested
	}

	// Recompute: SHA-256(id + "\n" + content + "\n" + timestamp)
	// Must match the algorithm in cmd/daemon/tsa.go hashBackfill()
	hashInput := fmt.Sprintf("%s\n%s\n%s", id, content, timestamp)
	computed := sha256.Sum256([]byte(hashInput))
	computedHex := hex.EncodeToString(computed[:])

	if computedHex == storedHash {
		return IntegrityVerified
	}
	return IntegrityFailed
}

// VerifyEntry checks integrity for an entry using the store's Redis client.
// Populates the entry's Integrity field in place.
func (s *RedisStore) VerifyEntry(ctx context.Context, entry *Entry) {
	if entry == nil || entry.ID == "" {
		return
	}

	// Skip meta/summary entries (never hashed by design)
	if strings.HasPrefix(entry.ID, "meta:") || strings.HasPrefix(entry.ID, "summary:") {
		return
	}

	// Fetch the stored content_hash and raw timestamp from Redis
	key := entryPrefix + entry.ID
	fields, err := s.client.HMGet(ctx, key, "content_hash", "timestamp").Result()
	if err != nil || len(fields) < 2 {
		return
	}

	storedHash, _ := fields[0].(string)
	timestamp, _ := fields[1].(string)

	entry.Integrity = VerifyEntryIntegrity(entry.ID, entry.Content, timestamp, storedHash)
}

// VerifyEntries checks integrity for a slice of entries.
func (s *RedisStore) VerifyEntries(ctx context.Context, entries []Entry) {
	for i := range entries {
		s.VerifyEntry(ctx, &entries[i])
	}
}

// VerifySearchResults checks integrity for search results.
func (s *RedisStore) VerifySearchResults(ctx context.Context, results []SearchResult) {
	for i := range results {
		s.VerifyEntry(ctx, &results[i].Entry)
	}
}
