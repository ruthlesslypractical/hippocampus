package memory

import "context"

// Store defines the interface for the memory system.
// All tags are just strings — tracks, summaries, topics, temporal scopes, etc.
// Tags do all the organizational work. No fixed schema beyond Entry.
type Store interface {
	// Put stores an entry. If ID already exists, it's overwritten.
	Put(ctx context.Context, entry Entry) error

	// Get retrieves a single entry by ID.
	Get(ctx context.Context, id string) (Entry, error)

	// Delete removes an entry by ID.
	Delete(ctx context.Context, id string) error

	// Search finds entries matching a query string (full-text search).
	// Falls back to naive substring scan if RediSearch is not available.
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)

	// ByTags returns entries that have ALL of the specified tags.
	ByTags(ctx context.Context, tags []string, limit int, offset int) ([]Entry, error)

	// ByAnyTag returns entries that have ANY of the specified tags.
	ByAnyTag(ctx context.Context, tags []string, limit int, offset int) ([]Entry, error)

	// AddTags adds tags to an existing entry.
	AddTags(ctx context.Context, id string, tags []string) error

	// RemoveTags removes tags from an existing entry.
	RemoveTags(ctx context.Context, id string, tags []string) error

	// ListTags returns all known tags with their entry counts.
	ListTags(ctx context.Context) ([]TagInfo, error)

	// EntriesByTimeRange returns entries within a time window, optionally filtered by tags.
	EntriesByTimeRange(ctx context.Context, start, end int64, tags []string, limit int) ([]Entry, error)

	// Link creates a bidirectional associative link between two entries.
	// Score ranges from -1.0 (anti-relevant) to +1.0 (strongly relevant).
	// RelationType is optional: "supports", "contradicts", "extends", "preceded_by", or "".
	Link(ctx context.Context, idA, idB string, score float64, relationType string) error

	// Unlink removes a bidirectional link between two entries.
	Unlink(ctx context.Context, idA, idB string) error

	// Links returns all links for an entry, sorted by |score| descending.
	Links(ctx context.Context, id string) ([]Link, error)

	// TopLinks returns the top N links for an entry by |score|.
	TopLinks(ctx context.Context, id string, n int) ([]Link, error)

	// RenameTag renames a tag across all entries. Moves members between tag sets,
	// updates each entry's tag string, and updates the global tag registry.
	RenameTag(ctx context.Context, oldTag, newTag string) (int, error)

	// Close releases resources.
	Close() error
}
