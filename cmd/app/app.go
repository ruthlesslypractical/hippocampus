// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct holds the application state.
type App struct {
	ctx         context.Context
	redisClient *redis.Client
	redisCmd    *exec.Cmd // local redis process (if spawned)
	ollamaCmd   *exec.Cmd // local ollama process (if spawned)
	slackCmd    *exec.Cmd // slack bot process (if running)
	config      AppConfig
	fullConfig  config.Config // full config including tunables (shared with CLI tools)
	isFirstRun  bool          // true if no config existed at startup
}

// AppConfig holds the runtime configuration.
type AppConfig struct {
	// Redis
	RedisMode     string `json:"redis_mode"`     // "local" or "remote"
	RedisHost     string `json:"redis_host"`
	RedisPort     string `json:"redis_port"`
	RedisPassword string `json:"redis_password"`
	RedisTLS      bool   `json:"redis_tls"`
	RedisInsecure bool   `json:"redis_insecure"` // skip cert verification (self-signed)

	// Network sharing
	NetworkMode     bool   `json:"network_mode"`      // if true, bind to 0.0.0.0 (LAN accessible)
	TLSCertPath     string `json:"tls_cert_path"`     // path to generated cert
	TLSKeyPath      string `json:"tls_key_path"`      // path to generated key
	TLSFingerprint  string `json:"tls_fingerprint"`   // SHA-256 fingerprint for client trust

	// Ollama
	OllamaMode string `json:"ollama_mode"` // "local" or "remote"
	OllamaURL  string `json:"ollama_url"`

	// Services
	SlackBotEnabled           bool `json:"slack_bot_enabled"`            // auto-start Slack bot on app launch
	BackgroundServicesEnabled bool `json:"background_services_enabled"` // auto-load LaunchAgents on app launch

	// Identity
	Author string `json:"author"`
}

// Status is returned to the frontend.
type Status struct {
	RedisConnected bool   `json:"redis_connected"`
	RedisMode      string `json:"redis_mode"`
	RedisKeys      int64  `json:"redis_keys"`
	RedisTags      int64  `json:"redis_tags"`

	OllamaRunning   bool   `json:"ollama_running"`
	OllamaMode      string `json:"ollama_mode"`
	OllamaModel     string `json:"ollama_model"`
	EmbeddingModel  string `json:"embedding_model"`

	HookPath string `json:"hook_path"`
	MCPPath  string `json:"mcp_path"`

	Author        string `json:"author"`
	EntriesToday  int64  `json:"entries_today"`
}

func NewApp() *App {
	return &App{
		config: AppConfig{
			RedisMode:  "local",
			RedisHost:  "localhost",
			RedisPort:  "6379",
			OllamaMode: "local",
			OllamaURL:  "http://localhost:11434",
		},
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.loadConfig()

	// Detect first run BEFORE creating the config file
	configPath := a.configFilePath()
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		a.isFirstRun = true
		a.saveConfig()
	}

	// Auto-connect on startup if we have saved config
	if a.config.RedisMode == "local" || (a.config.RedisMode == "remote" && a.config.RedisHost != "") {
		go func() {
			// Small delay to let the window render first
			time.Sleep(500 * time.Millisecond)
			a.Connect()
		}()
	}
	if a.config.OllamaMode != "" {
		go func() {
			time.Sleep(700 * time.Millisecond)
			a.StartOllama()
		}()
	}
	// Auto-start Slack bot if it was enabled when user last quit
	if a.config.SlackBotEnabled {
		go func() {
			time.Sleep(1 * time.Second)
			a.StartSlackBot()
		}()
	}
	// Auto-load background services if they were enabled when user last quit
	if a.config.BackgroundServicesEnabled {
		go func() {
			time.Sleep(1200 * time.Millisecond)
			a.ensureServicesLoaded()
		}()
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.redisClient != nil {
		a.redisClient.Close()
	}
	if a.redisCmd != nil && a.redisCmd.Process != nil {
		a.redisCmd.Process.Kill()
	}
	if a.ollamaCmd != nil && a.ollamaCmd.Process != nil {
		a.ollamaCmd.Process.Kill()
	}
	if a.slackCmd != nil && a.slackCmd.Process != nil {
		a.slackCmd.Process.Kill()
	}
}

// --- Exported methods (callable from frontend via Wails bindings) ---

// GetStatus returns the current system status.
func (a *App) GetStatus() Status {
	s := Status{
		RedisMode:  a.config.RedisMode,
		OllamaMode: a.config.OllamaMode,
		Author:     a.config.Author,
		HookPath:   a.hookPath(),
		MCPPath:    a.mcpPath(),
	}

	if a.redisClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := a.redisClient.Ping(ctx).Err(); err == nil {
			s.RedisConnected = true
			s.RedisKeys, _ = a.redisClient.DBSize(ctx).Result()
			s.RedisTags, _ = a.redisClient.SCard(ctx, "tags:all").Result()

			// Today's entries
			today := time.Now().Format("2006-01-02")
			s.EntriesToday, _ = a.redisClient.SCard(ctx, "tag:date:"+today).Result()
		}
	}

	// Check Ollama
	s.OllamaRunning = a.checkOllama()
	if s.OllamaRunning {
		s.OllamaModel = "qwen3:32b"
		s.EmbeddingModel = "nomic-embed-text"
	}

	return s
}

// GetConfig returns the current config.
func (a *App) GetConfig() AppConfig {
	return a.config
}

// GetVersion returns the app version string.
func (a *App) GetVersion() string {
	return config.Version
}

// SaveConfig saves the config. Does not attempt to connect — that happens
// via the explicit Start buttons or on next app launch.
func (a *App) SaveConfig(cfg AppConfig) error {
	prevMode := a.config.RedisMode
	prevNetworkMode := a.config.NetworkMode

	// Preserve TLS paths (frontend doesn't send these)
	cfg.TLSCertPath = a.config.TLSCertPath
	cfg.TLSKeyPath = a.config.TLSKeyPath
	cfg.TLSFingerprint = a.config.TLSFingerprint

	// For local mode: password always comes from redis.conf, not the form
	if cfg.RedisMode == "local" {
		cfg.RedisPassword = a.localRedisPassword()
		cfg.RedisHost = "127.0.0.1"
		cfg.RedisPort = "16379"
	}

	// Handle network mode transitions for local Redis
	if cfg.RedisMode == "local" && cfg.NetworkMode && !prevNetworkMode {
		// Enabling sharing: generate TLS cert if needed
		if cfg.TLSCertPath == "" || cfg.TLSKeyPath == "" {
			a.config = cfg
			if err := a.generateTLSCert(); err != nil {
				return fmt.Errorf("generating TLS certificate: %w", err)
			}
			cfg.TLSCertPath = a.config.TLSCertPath
			cfg.TLSKeyPath = a.config.TLSKeyPath
			cfg.TLSFingerprint = a.config.TLSFingerprint
		}
		cfg.RedisTLS = true
		cfg.RedisInsecure = true
	} else if cfg.RedisMode == "local" && !cfg.NetworkMode && prevNetworkMode {
		// Disabling sharing: drop TLS
		cfg.RedisTLS = false
		cfg.RedisInsecure = false
	} else if cfg.RedisMode == "local" && cfg.NetworkMode {
		// Already sharing, keep TLS
		cfg.RedisTLS = true
		cfg.RedisInsecure = true
	}

	a.config = cfg
	a.saveConfig()

	// Disconnect existing client
	if a.redisClient != nil {
		a.redisClient.Close()
		a.redisClient = nil
	}

	// Kill local Redis if switching to remote
	if cfg.RedisMode == "remote" && prevMode != "remote" {
		if a.redisCmd != nil && a.redisCmd.Process != nil {
			a.redisCmd.Process.Kill()
			a.redisCmd.Wait()
			a.redisCmd = nil
		}
	}

	// Regenerate redis.conf + restart if local mode and network state changed
	if cfg.RedisMode == "local" && (cfg.NetworkMode != prevNetworkMode || prevMode == "remote") {
		a.regenerateRedisConf()
		if a.redisCmd != nil && a.redisCmd.Process != nil {
			a.redisCmd.Process.Kill()
			a.redisCmd.Wait()
			a.redisCmd = nil
		}
		time.Sleep(300 * time.Millisecond)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		a.Connect()
	}()

	return nil
}

// Connect establishes connection to Redis (and optionally starts local instance).
func (a *App) Connect() error {
	if a.redisClient != nil {
		a.redisClient.Close()
		a.redisClient = nil
	}

	if a.config.RedisMode == "local" {
		if err := a.startLocalRedis(); err != nil {
			return fmt.Errorf("starting local Redis: %w", err)
		}
	} else if a.config.RedisMode == "remote" {
		// Validate remote config
		if a.config.RedisHost == "" || a.config.RedisHost == "localhost" {
			if a.config.RedisPort == "" || a.config.RedisPort == "6379" {
				// User said "remote" but didn't configure a host — don't silently
				// connect to localhost and confuse them.
				if a.config.RedisHost == "" {
					return fmt.Errorf("remote Redis host not configured (set it in Settings)")
				}
			}
		}
	}

	host := a.config.RedisHost
	port := a.config.RedisPort
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "6379"
	}
	addr := host + ":" + port

	opts := &redis.Options{
		Addr:     addr,
		Password: a.config.RedisPassword,
	}
	if a.config.RedisTLS {
		opts.TLSConfig = &tls.Config{
			InsecureSkipVerify: a.config.RedisInsecure,
		}
	}
	a.redisClient = redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := a.redisClient.Ping(ctx).Err(); err != nil {
		a.redisClient = nil
		return fmt.Errorf("connecting to Redis at %s: %w", addr, err)
	}

	// Seed orientation entries if they don't exist yet (first run)
	a.seedOrientationIfNeeded()

	return nil
}

// Disconnect closes the Redis connection.
func (a *App) Disconnect() {
	if a.redisClient != nil {
		a.redisClient.Close()
		a.redisClient = nil
	}
}

// seedOrientationIfNeeded checks if orientation entries exist in Redis,
// and creates them from the bundled prompt templates if not.
func (a *App) seedOrientationIfNeeded() {
	if a.redisClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Check if lean kernel already exists (correct ID or legacy double-prefix)
	exists, _ := a.redisClient.Exists(ctx, "entry:meta:lean-kernel").Result()
	if exists == 0 {
		exists, _ = a.redisClient.Exists(ctx, "entry:entry:meta:lean-kernel").Result()
	}
	if exists > 0 {
		return // already seeded
	}

	now := time.Now().Unix()

	// Seed lean kernel
	leanContent := `You are an AI assistant with persistent memory (Valkey via memory_* MCP tools). Your full orientation is stored at entry:meta:orientation — query it with memory_get if you need it.

Memory discipline: At breakpoints, pause and assess — page in what you need, store what you produced, don't carry stale context. Your context window is RAM; Valkey is disk. Use demand paging.

Reasoning breadcrumbs: At significant decision points, store a brief "chose X over Y because Z" entry tagged ` + "`kind:reasoning`" + `. Link it to what it explains. Don't breadcrumb trivial choices.

Available tracks: [query memory_list_tags for tracks]. Query meta:orientation:track:<Name> for orientation on any track.`

	pipe := a.redisClient.Pipeline()

	// Lean kernel entry — correct ID format (no entry: prefix in the ID itself)
	leanID := "meta:lean-kernel"
	leanTags := "meta,orientation,lean-kernel"
	pipe.HSet(ctx, "entry:"+leanID, map[string]interface{}{
		"id": leanID, "content": leanContent, "tags": leanTags, "timestamp": now,
	})
	pipe.ZAdd(ctx, "timeline", redis.Z{Score: float64(now), Member: leanID})
	pipe.SAdd(ctx, "tag:meta", leanID)
	pipe.SAdd(ctx, "tag:orientation", leanID)
	pipe.SAdd(ctx, "tag:lean-kernel", leanID)
	pipe.SAdd(ctx, "tags:all", "meta", "orientation", "lean-kernel")

	// Full orientation entry
	fullContent := `You are an AI assistant with persistent long-term memory backed by Valkey (Redis-compatible).

### HOW YOUR MEMORY WORKS

Your memory is stored in Valkey as tagged entries. You can read and write to it using the memory_* MCP tools.

**Data model:**
- entry:<id> — HASH with fields: id, content, tags (comma-separated), timestamp (unix)
- tag:<tagname> — SET of entry IDs that have that tag
- tags:all — SET of all tag names
- timeline — ZSET (score=unix timestamp, member=entry ID)

**Tag taxonomy (how to find things):**
- track:<Name> — Major project/topic tracks
- summary:daily:<Name> — Distilled daily summaries of a track (read FIRST for orientation)
- summary:comprehensive — High-level orientation entries
- kind:user_prompt / kind:assistant_response — Captured conversation history
- auto:captured — Entries auto-captured by hooks
- session:<uuid> — All entries from a specific chat session
- date:YYYY-MM-DD — Temporal tags

### WHAT TO DO EACH SESSION

1. When the user mentions a track/topic: Query summary:daily:<Name> FIRST, then raw entries if needed.
2. When something important is decided: Store it with appropriate tags.
3. When significant insight emerges: Consider creating/updating a summary entry.
4. Proactively tag: Use memory_store for curated insights, not just raw conversation.

### MEMORY DISCIPLINE

Your context window is finite. Treat it like physical RAM — Valkey is your disk.
- Page in what you need: If the user mentions a topic, query memory before answering.
- Store what you produce: At breakpoints, write back decisions and insights.
- Let stale context go: Trust that you can re-query if needed.

### ASSOCIATIVE LINKS

Entries can be linked with scores from -1.0 to +1.0:
- Positive links (+0.5 to +1.0): "This supports/extends that."
- Negative links (-0.5 to -1.0): "We tried this and it FAILED" or "This was superseded."
  When negative links appear, explicitly call out prior dead ends to prevent repeating mistakes.

### REASONING BREADCRUMBS

At significant decision points, store a brief "chose X over Y because Z" entry tagged kind:reasoning.
Link it to what it explains. Don't breadcrumb trivial choices.`

	fullID := "entry:meta:orientation"
	fullTags := "meta,orientation,summary:comprehensive"
	pipe.HSet(ctx, "entry:"+fullID, map[string]interface{}{
		"id": fullID, "content": fullContent, "tags": fullTags, "timestamp": now,
	})
	pipe.ZAdd(ctx, "timeline", redis.Z{Score: float64(now), Member: fullID})
	pipe.SAdd(ctx, "tag:meta", fullID)
	pipe.SAdd(ctx, "tag:orientation", fullID)
	pipe.SAdd(ctx, "tag:summary:comprehensive", fullID)
	pipe.SAdd(ctx, "tags:all", "summary:comprehensive")

	pipe.Exec(ctx)
}

// IsFirstStart returns true if this is the first time the app has been launched.
func (a *App) IsFirstStart() bool {
	return a.isFirstRun
}

// Confirm shows a native confirmation dialog. Returns true if user clicks OK.
// Use this instead of JavaScript confirm() which doesn't work in WKWebView.
func (a *App) Confirm(title, message string) bool {
	result, err := wailsRuntime.MessageDialog(a.ctx, wailsRuntime.MessageDialogOptions{
		Type:          wailsRuntime.QuestionDialog,
		Title:         title,
		Message:       message,
		Buttons:       []string{"No", "Yes"},
		DefaultButton: "Yes",
	})
	if err != nil {
		return false
	}
	return result == "Yes"
}

// Prompt shows a native input dialog. Returns empty string if cancelled.
// Use this instead of JavaScript prompt() which doesn't work in WKWebView.
func (a *App) Prompt(title, message string) string {
	// Wails doesn't have a native text input dialog, so we use a simple
	// question dialog approach. For text input, the frontend should use
	// an inline input field instead.
	// This method exists as a placeholder — frontend should use inline inputs.
	return ""
}
