// Package consolidate implements background pairwise relevance discovery.
// It randomly samples entry pairs and uses a local LLM to compute
// associative link scores, passively building the link graph over time.
//
// This is analogous to memory consolidation during sleep — the system
// finds connections between entries that weren't explicitly linked.
package consolidate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/ollama"
)

// Config holds consolidator settings.
type Config struct {
	// RedisAddr is the Redis connection string.
	RedisAddr string
	// RedisPassword for AUTH.
	RedisPassword string
	// OllamaURL is the Ollama API endpoint.
	OllamaURL string
	// OllamaModel is the model to use for relevance scoring.
	OllamaModel string
	// PairsPerRun is how many pairs to evaluate per cycle.
	PairsPerRun int
	// MinScore is the minimum |score| to create a link.
	MinScore float64
	// CyclePause is the delay between evaluation cycles.
	CyclePause time.Duration
	// MaxEntries caps how many entries we sample from.
	MaxEntries int
	// DriftDelta is the minimum score change to trigger a link update.
	DriftDelta float64
	// ContentTruncation is the max chars per entry in LLM prompt.
	ContentTruncation int
	// MinContentLength is the minimum content length to consider linkable.
	MinContentLength int
	// Temperature is the LLM temperature for scoring.
	Temperature float64
	// MaxTokens is the max LLM response tokens.
	MaxTokens int
	// EvalTimeoutS is the per-pair LLM eval timeout in seconds.
	EvalTimeoutS int
	// DryRun logs but doesn't write links.
	DryRun bool
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		OllamaURL:         "http://localhost:11434",
		OllamaModel:       "qwen3:32b",
		PairsPerRun:       10,
		MinScore:          0.4,
		CyclePause:        30 * time.Second,
		MaxEntries:        500,
		DriftDelta:        0.2,
		ContentTruncation: 500,
		MinContentLength:  50,
		Temperature:       0.1,
		MaxTokens:         200,
		EvalTimeoutS:      60,
	}
}

// Result tracks what happened during one consolidation cycle.
type Result struct {
	PairsEvaluated int
	LinksCreated   int
	Errors         int
}

// entry is a minimal representation for consolidation.
type entry struct {
	ID      string
	Content string
	Tags    string
}

// linkScore is the LLM's assessment of a pair.
type linkScore struct {
	Score        float64 `json:"score"`
	RelationType string  `json:"relation_type"`
	Reasoning    string  `json:"reasoning"`
}

// Run executes one consolidation cycle: sample pairs, evaluate, link.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	defer client.Close()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}

	result := &Result{}

	ollamaClient := ollama.New(cfg.OllamaURL, cfg.OllamaModel, 0)

	// Sample candidate entries (avoid content:full, kind:prompt too-short entries)
	entries, err := sampleEntries(ctx, client, cfg)
	if err != nil {
		return nil, fmt.Errorf("sampling entries: %w", err)
	}

	if len(entries) < 2 {
		return result, nil
	}

	// Generate random pairs (prefer pairs sharing at least one tag)
	pairs := selectPairs(entries, cfg.PairsPerRun)

	for _, pair := range pairs {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		// Check if already linked — still evaluate to detect drift
		existingScore, existingErr := client.ZScore(ctx, "link:"+pair[0].ID, pair[1].ID).Result()
		alreadyLinked := existingErr == nil

		// Ask LLM to score the pair
		score, err := evaluatePair(ctx, cfg, ollamaClient, pair[0], pair[1])
		result.PairsEvaluated++
		if err != nil {
			result.Errors++
			log.Printf("  Error evaluating pair: %v", err)
			continue
		}

		absScore := absFloat(score.Score)

		// Decision: create, update, or skip
		if alreadyLinked {
			// Re-evaluation: only update if score drifted significantly
			delta := absFloat(score.Score - existingScore)
			if delta < cfg.DriftDelta {
				continue // Score is stable, no update needed
			}
			// Score drifted — update or remove
			if absScore < cfg.MinScore {
				// Relevance dropped below threshold — remove the link
				if !cfg.DryRun {
					client.ZRem(ctx, "link:"+pair[0].ID, pair[1].ID)
					client.ZRem(ctx, "link:"+pair[1].ID, pair[0].ID)
				}
				log.Printf("  Unlinked (decayed): %s ↔ %s (was %.2f, now %.2f)",
					truncate(pair[0].ID, 40), truncate(pair[1].ID, 40), existingScore, score.Score)
				result.LinksCreated++ // counts as an action
				continue
			}
			// Update with new score
			if cfg.DryRun {
				log.Printf("  [dry-run] Would update %s ↔ %s (%.2f → %.2f, %s)",
					pair[0].ID, pair[1].ID, existingScore, score.Score, score.Reasoning)
				continue
			}
			pipe := client.Pipeline()
			pipe.ZAdd(ctx, "link:"+pair[0].ID, redis.Z{Score: score.Score, Member: pair[1].ID})
			pipe.ZAdd(ctx, "link:"+pair[1].ID, redis.Z{Score: score.Score, Member: pair[0].ID})
			pipe.Exec(ctx)
			log.Printf("  Updated: %s ↔ %s (%.2f → %.2f)", truncate(pair[0].ID, 40), truncate(pair[1].ID, 40), existingScore, score.Score)
			result.LinksCreated++
			continue
		}

		// New link: only create if above threshold
		if absScore < cfg.MinScore {
			continue
		}

		if cfg.DryRun {
			log.Printf("  [dry-run] Would link %s ↔ %s (score: %.2f, type: %s, reason: %s)",
				pair[0].ID, pair[1].ID, score.Score, score.RelationType, score.Reasoning)
			continue
		}

		// Create the link
		pipe := client.Pipeline()
		pipe.ZAdd(ctx, "link:"+pair[0].ID, redis.Z{Score: score.Score, Member: pair[1].ID})
		pipe.ZAdd(ctx, "link:"+pair[1].ID, redis.Z{Score: score.Score, Member: pair[0].ID})
		if _, err := pipe.Exec(ctx); err != nil {
			result.Errors++
			continue
		}

		result.LinksCreated++
		log.Printf("  Linked: %s ↔ %s (%.2f, %s)", truncate(pair[0].ID, 40), truncate(pair[1].ID, 40), score.Score, score.RelationType)
	}

	return result, nil
}

// RunContinuous runs consolidation in a loop until context is cancelled.
func RunContinuous(ctx context.Context, cfg Config) {
	log.Printf("Consolidator starting (pairs/cycle: %d, pause: %s, min_score: %.2f)",
		cfg.PairsPerRun, cfg.CyclePause, cfg.MinScore)

	for {
		select {
		case <-ctx.Done():
			log.Println("Consolidator shutting down.")
			return
		default:
		}

		result, err := Run(ctx, cfg)
		if err != nil {
			log.Printf("Consolidation cycle error: %v", err)
		} else if result.PairsEvaluated > 0 {
			log.Printf("Cycle complete: %d evaluated, %d linked, %d errors",
				result.PairsEvaluated, result.LinksCreated, result.Errors)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.CyclePause):
		}
	}
}

// sampleEntries gets a random sample of entries suitable for comparison.
func sampleEntries(ctx context.Context, client *redis.Client, cfg Config) ([]entry, error) {
	// Get recent entries from timeline (last N)
	ids, err := client.ZRevRange(ctx, "timeline", 0, int64(cfg.MaxEntries)).Result()
	if err != nil {
		return nil, err
	}

	var entries []entry
	for _, id := range ids {
		data, err := client.HGetAll(ctx, "entry:"+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}

		tags := data["tags"]

		// Skip entries that aren't useful for linking
		if strings.Contains(tags, "content:full") {
			continue // Untrusted web content
		}
		if strings.Contains(tags, "kind:prompt") && len(data["content"]) < cfg.MinContentLength {
			continue // Short prompts ("yeah", "do it") aren't linkable
		}
		if strings.Contains(tags, "meta") && strings.Contains(tags, "orientation") {
			continue // Don't link orientation entries to random stuff
		}

		content := data["content"]
		if len(content) > cfg.ContentTruncation {
			content = content[:cfg.ContentTruncation] // Truncate for LLM prompt budget
		}

		entries = append(entries, entry{
			ID:      data["id"],
			Content: content,
			Tags:    tags,
		})
	}

	return entries, nil
}

// selectPairs generates pairs for evaluation, preferring pairs that share tags.
func selectPairs(entries []entry, count int) [][2]entry {
	if len(entries) < 2 {
		return nil
	}

	var pairs [][2]entry

	// First, try pairs that share at least one tag (more likely to be relevant)
	tagIndex := buildTagIndex(entries)
	for tag, members := range tagIndex {
		if len(pairs) >= count {
			break
		}
		// Skip very common tags (auto:captured, date:*, kind:*)
		if strings.HasPrefix(tag, "auto:") || strings.HasPrefix(tag, "date:") ||
			strings.HasPrefix(tag, "kind:") || strings.HasPrefix(tag, "session:") ||
			strings.HasPrefix(tag, "cwd:") {
			continue
		}
		if len(members) < 2 {
			continue
		}
		// Pick a random pair from this tag
		i := rand.Intn(len(members))
		j := rand.Intn(len(members))
		if i == j {
			j = (j + 1) % len(members)
		}
		pairs = append(pairs, [2]entry{members[i], members[j]})
	}

	// Fill remaining with random pairs
	for len(pairs) < count {
		i := rand.Intn(len(entries))
		j := rand.Intn(len(entries))
		if i == j {
			continue
		}
		pairs = append(pairs, [2]entry{entries[i], entries[j]})
	}

	return pairs
}

func buildTagIndex(entries []entry) map[string][]entry {
	idx := make(map[string][]entry)
	for _, e := range entries {
		for _, tag := range strings.Split(e.Tags, ",") {
			idx[tag] = append(idx[tag], e)
		}
	}
	return idx
}

// evaluatePair asks the LLM to score the relevance between two entries.
func evaluatePair(ctx context.Context, cfg Config, ollamaClient *ollama.Client, a, b entry) (*linkScore, error) {
	prompt := fmt.Sprintf(`You are a memory consolidation system. Given two memory entries, assess their relevance to each other.

Entry A [tags: %s]:
%s

Entry B [tags: %s]:
%s

Rate the associative relevance from -1.0 to +1.0:
- +0.5 to +1.0: Strongly related (supports, extends, or is about the same topic)
- +0.1 to +0.4: Weakly related (tangential connection)
- 0.0: Unrelated
- -0.1 to -0.4: Mildly contradictory or superseded
- -0.5 to -1.0: Strongly contradicts, or "we tried this and it failed"

Respond in JSON only:
{"score": <float>, "relation_type": "<supports|contradicts|extends|preceded_by|unrelated>", "reasoning": "<one sentence>"}`, a.Tags, a.Content, b.Tags, b.Content)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.EvalTimeoutS)*time.Second)
	defer cancel()

	opts := &ollama.GenerateOptions{
		Temperature: cfg.Temperature,
		NumPredict:  cfg.MaxTokens,
	}

	respText, err := ollamaClient.GenerateWithOptions(callCtx, prompt, cfg.OllamaModel, opts)
	if err != nil {
		return nil, err
	}

	// Extract JSON from response (may have thinking tags)
	jsonStr := extractJSON(respText)

	var score linkScore
	if err := json.Unmarshal([]byte(jsonStr), &score); err != nil {
		return nil, fmt.Errorf("parsing score JSON from: %s", jsonStr)
	}

	// Clamp score
	if score.Score > 1.0 {
		score.Score = 1.0
	}
	if score.Score < -1.0 {
		score.Score = -1.0
	}

	return &score, nil
}

func extractJSON(s string) string {
	// Strip <think> tags if present
	if idx := strings.Index(s, "</think>"); idx != -1 {
		s = s[idx+len("</think>"):]
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}
	return strings.TrimSpace(s)
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
