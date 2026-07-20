package config

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

// Version is the single source of truth for the Hippocampus version.
const Version = "1.0.0"

// Config holds all configuration for Hippocampus.
type Config struct {
	Redis         RedisConfig         `json:"redis"`
	Memory        MemoryConfig        `json:"memory"`
	Ollama        OllamaConfig        `json:"ollama"`
	Ingest        IngestConfig        `json:"ingest"`
	Consolidation ConsolidationConfig `json:"consolidation"`
	Hook          HookConfig          `json:"hook"`
	MCP           MCPConfig           `json:"mcp"`
	Slack         SlackConfig         `json:"slack"`
	WorkingSet    WorkingSetConfig    `json:"working_set"`
	Author        string              `json:"author"` // Attribution tag for multi-user (e.g. "alice"). Empty = no attribution.
}

// RedisConfig holds Redis/Valkey connection settings.
type RedisConfig struct {
	Addr     string `json:"addr"`              // host:port
	Password string `json:"password,omitempty"` // AUTH password
	Username string `json:"username,omitempty"` // ACL username (Valkey 6+)
	DB       int    `json:"db"`                // database number

	// TLS settings
	TLS         bool   `json:"tls"`                    // enable TLS
	TLSCert     string `json:"tls_cert,omitempty"`     // client cert path
	TLSKey      string `json:"tls_key,omitempty"`      // client key path
	TLSCA       string `json:"tls_ca,omitempty"`       // CA cert path
	TLSInsecure bool   `json:"tls_insecure,omitempty"` // skip server cert verification

	// Connection tuning
	MaxRetries   int `json:"max_retries,omitempty"`    // default 3
	DialTimeoutS int `json:"dial_timeout_s,omitempty"` // default 5
	PoolSize     int `json:"pool_size,omitempty"`      // default 10
}

// MemoryConfig holds memory system tuning.
type MemoryConfig struct {
	MaxSearchResults int `json:"max_search_results"` // MCP search default limit

	// Hook recall settings (what gets injected into the model's context)
	RecallMaxChars   int `json:"recall_max_chars"`   // max total chars injected
	RecallMaxEntries int `json:"recall_max_entries"` // max entries per prompt
	RecallScanDepth  int `json:"recall_scan_depth"`  // how many recent entries to scan in naive fallback (default 100)

	// Hook store settings (what gets persisted from conversations)
	StoreMaxChars       int `json:"store_max_chars"`        // max chars stored per entry (0 = unlimited)
	StoreMinPromptLen   int `json:"store_min_prompt_len"`   // prompts shorter than this are not stored
	StoreMinResponseLen int `json:"store_min_response_len"` // responses shorter than this are not stored

	// Confidence decay — older unreinforced memories fade during recall
	DecayHalfLifeDays float64 `json:"decay_half_life_days"` // half-life in days (0 = no decay, default 30)

	// Summarizer settings
	SummarizeMaxEntries    int `json:"summarize_max_entries"`      // max entries per track in --all mode (default 200)
	SummarizeMaxInputChars int `json:"summarize_max_input_chars"`  // max chars fed to LLM for summarization (default 50000)
	ClassifyMaxChars       int `json:"classify_max_chars"`         // max chars per entry for classification (default 500)
	CrossTrackMaxChars     int `json:"cross_track_max_chars"`      // max chars per summary for cross-track analysis (default 3000)
}

// OllamaConfig holds local LLM settings for summarization and embedding.
type OllamaConfig struct {
	BaseURL             string `json:"base_url"`             // e.g. http://localhost:11434
	Model               string `json:"model"`                // e.g. qwen3:32b (for summarization)
	TimeoutMinutes      int    `json:"timeout_minutes"`      // HTTP timeout for generation (default 10)
	EmbeddingModel      string `json:"embedding_model"`      // e.g. nomic-embed-text (for vector search)
	EmbeddingDimensions int    `json:"embedding_dimensions"` // vector dimensions (default 768)
}

// IngestConfig holds web ingestion pipeline settings.
type IngestConfig struct {
	// Safety thresholds
	RejectThreshold   float64 `json:"reject_threshold"`   // Risk score >= this → reject (default 0.8)
	SanitizeThreshold float64 `json:"sanitize_threshold"` // Risk score >= this → sanitize (default 0.5)
	SafetyThreshold   float64 `json:"safety_threshold"`   // Below this = considered safe (default 0.5)

	// Instruction density heuristic
	DensityRatio    float64 `json:"density_ratio"`     // Instruction sentence ratio trigger (default 0.3)
	DensityMinCount int     `json:"density_min_count"` // Min instruction-like sentences to trigger (default 3)

	// Content weights (recall priority)
	WebContentWeight float64 `json:"web_content_weight"` // Weight for full web content entries (default 0.3)
	StubWeight       float64 `json:"stub_weight"`        // Weight for stub/pointer entries (default 0.6)

	// Extraction settings
	FetchTimeoutS int   `json:"fetch_timeout_s"` // HTTP fetch timeout in seconds (default 30)
	MaxBodyBytes  int64 `json:"max_body_bytes"`  // Max HTML download size (default 10MB)
	UserAgent     string `json:"user_agent,omitempty"` // Custom User-Agent (empty = default browser UA)

	// Chunking
	MaxChunkSize int `json:"max_chunk_size"` // Max characters per chunk (default 3000)
	MinChunkSize int `json:"min_chunk_size"` // Min chunk size before merging (default 200)
}

// ConsolidationConfig holds pairwise relevance discovery settings.
type ConsolidationConfig struct {
	PairsPerRun       int     `json:"pairs_per_run"`       // Pairs evaluated per cycle (default 5)
	MinScore          float64 `json:"min_score"`           // Minimum |score| to create link (default 0.4)
	DriftDelta        float64 `json:"drift_delta"`         // Score change threshold to update existing link (default 0.2)
	CyclePauseS       int     `json:"cycle_pause_s"`       // Seconds between cycles (default 600)
	MaxEntries        int     `json:"max_entries"`         // Max entries sampled (default 500)
	ContentTruncation int     `json:"content_truncation"`  // Chars per entry in LLM prompt (default 500)
	MinContentLength  int     `json:"min_content_length"`  // Min content length to consider linkable (default 50)
	Temperature       float64 `json:"temperature"`         // LLM temperature for scoring (default 0.1)
	MaxTokens         int     `json:"max_tokens"`          // Max LLM response tokens (default 200)
	EvalTimeoutS      int     `json:"eval_timeout_s"`      // Per-pair LLM eval timeout in seconds (default 60)
}

// HookConfig holds recall/store hook settings.
type HookConfig struct {
	TimeoutS       int `json:"timeout_s"`        // Redis operation timeout (default 5)
	BootPhaseTTLH  int `json:"boot_phase_ttl_h"` // Hours before re-injecting full orientation (default 24)
	MaxLinkHops    int `json:"max_link_hops"`    // Associative link traversal depth (default 3)
	HookTimeoutMs  int `json:"hook_timeout_ms"`  // Timeout written to generated agent config (default 3000)
}

// MCPConfig holds MCP server settings.
type MCPConfig struct {
	DefaultSearchLimit    int `json:"default_search_limit"`     // Default results for memory_search (default 10)
	DefaultTagLimit       int `json:"default_tag_limit"`        // Default results for memory_by_tags (default 20)
	DefaultTimeRangeLimit int `json:"default_time_range_limit"` // Default results for memory_time_range (default 20)
}

// SlackConfig holds Slack bot integration settings.
type SlackConfig struct {
	BotToken string         `json:"bot_token,omitempty"` // xoxb-... Bot User OAuth Token
	AppToken string         `json:"app_token,omitempty"` // xapp-... App-Level Token (Socket Mode)
	Channels []SlackChannel `json:"channels,omitempty"`  // Channels to monitor
}

// SlackChannel represents a single monitored Slack channel or DM.
type SlackChannel struct {
	ID       string   `json:"id"`                // Slack channel ID (e.g. C0123ABC)
	Name     string   `json:"name"`              // Human-readable name (e.g. #engineering)
	Mode     string   `json:"mode,omitempty"`    // "archive" (default, silent) or "active" (responds to mentions)
	Tags     []string `json:"tags,omitempty"`    // Auto-applied tags for all messages from this channel
	Backfill bool     `json:"backfill,omitempty"` // If true, incrementally ingest channel history
}

// WorkingSetConfig holds settings for the Working Set Tracker sidecar.
type WorkingSetConfig struct {
	Enabled       bool   `json:"enabled"`                  // Enable working set tracking
	Model         string `json:"model"`                    // Ollama model for sidecar summarization (e.g. qwen3:1.7b)
	MaxBullets    int    `json:"max_bullets"`              // Max bullet points in working set (default 5)
	MaxChars      int    `json:"max_chars"`                // Max chars in working set (default 500)
	InheritTTLH   int    `json:"inherit_ttl_h"`            // Hours to inherit from previous session (default 24)
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Redis: RedisConfig{
			Addr:         "localhost:6379",
			DB:           0,
			MaxRetries:   3,
			DialTimeoutS: 5,
			PoolSize:     10,
		},
		Memory: MemoryConfig{
			MaxSearchResults:       20,
			RecallMaxChars:         8000,
			RecallMaxEntries:       10,
			RecallScanDepth:        100,
			StoreMaxChars:          0, // unlimited
			StoreMinPromptLen:      20,
			StoreMinResponseLen:    100,
			DecayHalfLifeDays:      30,
			SummarizeMaxEntries:    200,
			SummarizeMaxInputChars: 50000,
			ClassifyMaxChars:       500,
			CrossTrackMaxChars:     3000,
		},
		Ollama: OllamaConfig{
			BaseURL:             "http://localhost:11434",
			Model:               "qwen3:32b",
			TimeoutMinutes:      10,
			EmbeddingModel:      "nomic-embed-text",
			EmbeddingDimensions: 768,
		},
		Ingest: IngestConfig{
			RejectThreshold:   0.8,
			SanitizeThreshold: 0.5,
			SafetyThreshold:   0.5,
			DensityRatio:      0.3,
			DensityMinCount:   3,
			WebContentWeight:  0.3,
			StubWeight:        0.6,
			FetchTimeoutS:     30,
			MaxBodyBytes:      10 * 1024 * 1024,
			MaxChunkSize:      3000,
			MinChunkSize:      200,
		},
		Consolidation: ConsolidationConfig{
			PairsPerRun:       5,
			MinScore:          0.4,
			DriftDelta:        0.2,
			CyclePauseS:       600,
			MaxEntries:        500,
			ContentTruncation: 500,
			MinContentLength:  50,
			Temperature:       0.1,
			MaxTokens:         200,
			EvalTimeoutS:      60,
		},
		Hook: HookConfig{
			TimeoutS:      5,
			BootPhaseTTLH: 24,
			MaxLinkHops:   3,
			HookTimeoutMs: 3000,
		},
		MCP: MCPConfig{
			DefaultSearchLimit:    10,
			DefaultTagLimit:       20,
			DefaultTimeRangeLimit: 20,
		},
		WorkingSet: WorkingSetConfig{
			Enabled:     false,
			Model:       "qwen3:1.7b",
			MaxBullets:  5,
			MaxChars:    500,
			InheritTTLH: 24,
		},
	}
}

// FindConfigPath returns the path to the Hippocampus config file by checking,
// in order: HIPPOCAMPUS_CONFIG env var, macOS Application Support, ~/.config,
// CWD, and /etc. If none exist, it returns a platform-appropriate default.
func FindConfigPath() string {
	if envPath := os.Getenv("HIPPOCAMPUS_CONFIG"); envPath != "" {
		return envPath
	}

	homeDir, _ := os.UserHomeDir()
	candidates := []string{
		homeDir + "/Library/Application Support/Hippocampus/config.json", // macOS app
		homeDir + "/.config/hippocampus/config.json",                     // Unix/Linux
		"config.json",                                                    // CWD fallback
		"/etc/hippocampus/config.json",                                   // system-wide
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	// Default to macOS path on darwin, Unix otherwise
	if runtime.GOOS == "darwin" {
		return homeDir + "/Library/Application Support/Hippocampus/config.json"
	}
	return homeDir + "/.config/hippocampus/config.json"
}

// Load reads config from a JSON file, falling back to defaults for missing fields.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config: %w", err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}

// Save writes config to a JSON file.
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}
