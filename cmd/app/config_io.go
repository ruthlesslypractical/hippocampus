// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// diskConfig is the canonical config format shared with CLI tools
// (hippocampus-mcp, hippocampus-hook, hippocampus-summarize).
type diskConfig struct {
	Redis struct {
		Addr        string `json:"addr"`
		Password    string `json:"password,omitempty"`
		TLS         bool   `json:"tls,omitempty"`
		TLSInsecure bool   `json:"tls_insecure,omitempty"`
	} `json:"redis"`
	Ollama struct {
		BaseURL             string `json:"base_url"`
		Model               string `json:"model"`
		EmbeddingModel      string `json:"embedding_model"`
		EmbeddingDimensions int    `json:"embedding_dimensions"`
	} `json:"ollama"`
	Author string `json:"author,omitempty"`

	// App-specific fields (ignored by CLI tools, preserved on disk)
	App struct {
		RedisMode                 string `json:"redis_mode,omitempty"`
		OllamaMode                string `json:"ollama_mode,omitempty"`
		NetworkMode               bool   `json:"network_mode,omitempty"`
		TLSCertPath               string `json:"tls_cert_path,omitempty"`
		TLSKeyPath                string `json:"tls_key_path,omitempty"`
		TLSFingerprint            string `json:"tls_fingerprint,omitempty"`
		SlackBotEnabled           bool   `json:"slack_bot_enabled,omitempty"`
		BackgroundServicesEnabled bool   `json:"background_services_enabled,omitempty"`
	} `json:"app,omitempty"`
}

func (a *App) loadConfig() {
	path := a.configFilePath()

	// Load full config (tunables etc.) via the shared config package
	fullCfg, _ := config.Load(path)
	a.fullConfig = fullCfg

	data, err := os.ReadFile(path)
	if err != nil {
		return // no config yet, use defaults
	}

	// Try canonical format first
	var dc diskConfig
	if err := json.Unmarshal(data, &dc); err == nil && dc.Redis.Addr != "" {
		// Parse addr into host:port
		host, port, err := net.SplitHostPort(dc.Redis.Addr)
		if err != nil {
			host = dc.Redis.Addr
			port = "6379"
		}
		a.config.RedisHost = host
		a.config.RedisPort = port
		a.config.RedisPassword = dc.Redis.Password
		a.config.RedisTLS = dc.Redis.TLS
		a.config.RedisInsecure = dc.Redis.TLSInsecure
		a.config.OllamaURL = dc.Ollama.BaseURL
		a.config.Author = dc.Author
		a.config.RedisMode = dc.App.RedisMode
		a.config.OllamaMode = dc.App.OllamaMode
		a.config.NetworkMode = dc.App.NetworkMode
		a.config.TLSCertPath = dc.App.TLSCertPath
		a.config.TLSKeyPath = dc.App.TLSKeyPath
		a.config.TLSFingerprint = dc.App.TLSFingerprint
		a.config.SlackBotEnabled = dc.App.SlackBotEnabled
		a.config.BackgroundServicesEnabled = dc.App.BackgroundServicesEnabled
		// Apply defaults for mode fields
		if a.config.RedisMode == "" {
			if host == "localhost" || host == "127.0.0.1" || host == "" {
				a.config.RedisMode = "local"
			} else {
				a.config.RedisMode = "remote"
			}
		}
		if a.config.OllamaMode == "" {
			a.config.OllamaMode = "local"
		}
		return
	}

	// Fallback: legacy flat format (migrate on next save)
	json.Unmarshal(data, &a.config)
}

func (a *App) saveConfig() {
	path := a.configFilePath()
	os.MkdirAll(filepath.Dir(path), 0o755)

	host := a.config.RedisHost
	port := a.config.RedisPort

	// When using bundled Redis, force localhost + bundled port
	if a.config.RedisMode == "local" {
		host = "127.0.0.1"
		if port == "" || port == "6379" {
			port = "16379"
		}
	}

	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "6379"
	}

	ollamaURL := a.config.OllamaURL
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	// Sync AppConfig connection settings INTO the full config
	a.fullConfig.Redis.Addr = host + ":" + port
	a.fullConfig.Redis.Password = a.config.RedisPassword
	a.fullConfig.Redis.TLS = a.config.RedisTLS
	a.fullConfig.Redis.TLSInsecure = a.config.RedisInsecure
	a.fullConfig.Ollama.BaseURL = ollamaURL
	if a.fullConfig.Ollama.Model == "" {
		a.fullConfig.Ollama.Model = "qwen3:32b"
	}
	if a.fullConfig.Ollama.EmbeddingModel == "" {
		a.fullConfig.Ollama.EmbeddingModel = "nomic-embed-text"
	}
	if a.fullConfig.Ollama.EmbeddingDimensions == 0 {
		a.fullConfig.Ollama.EmbeddingDimensions = 768
	}
	a.fullConfig.Author = a.config.Author

	// Write full config with an "app" section for GUI-specific fields
	type fullDiskConfig struct {
		config.Config
		App struct {
			RedisMode                 string `json:"redis_mode,omitempty"`
			OllamaMode                string `json:"ollama_mode,omitempty"`
			NetworkMode               bool   `json:"network_mode,omitempty"`
			TLSCertPath               string `json:"tls_cert_path,omitempty"`
			TLSKeyPath                string `json:"tls_key_path,omitempty"`
			TLSFingerprint            string `json:"tls_fingerprint,omitempty"`
			SlackBotEnabled           bool   `json:"slack_bot_enabled,omitempty"`
			BackgroundServicesEnabled bool   `json:"background_services_enabled,omitempty"`
		} `json:"app,omitempty"`
	}

	var out fullDiskConfig
	out.Config = a.fullConfig
	out.App.RedisMode = a.config.RedisMode
	out.App.OllamaMode = a.config.OllamaMode
	out.App.NetworkMode = a.config.NetworkMode
	out.App.TLSCertPath = a.config.TLSCertPath
	out.App.TLSKeyPath = a.config.TLSKeyPath
	out.App.TLSFingerprint = a.config.TLSFingerprint
	out.App.SlackBotEnabled = a.config.SlackBotEnabled
	out.App.BackgroundServicesEnabled = a.config.BackgroundServicesEnabled

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o600)
}

func (a *App) configFilePath() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Hippocampus", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hippocampus", "config.json")
}

func (a *App) dataDir() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Hippocampus", "data")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "hippocampus", "data")
}

func (a *App) logsDir() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "Hippocampus")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "hippocampus", "logs")
}

func (a *App) appSupportDir() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Hippocampus")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hippocampus")
}

func (a *App) mcpPath() string {
	execPath, _ := os.Executable()
	appDir := filepath.Dir(filepath.Dir(execPath))
	bundled := filepath.Join(appDir, "Resources", "hippocampus-mcp")
	if _, err := os.Stat(bundled); err == nil {
		return bundled
	}
	return filepath.Join(filepath.Dir(execPath), "hippocampus-mcp")
}

func (a *App) hookPath() string {
	// Inside .app bundle
	execPath, _ := os.Executable()
	appDir := filepath.Dir(filepath.Dir(execPath)) // up from MacOS/ to Contents/
	bundled := filepath.Join(appDir, "Resources", "hippocampus-hook")
	if _, err := os.Stat(bundled); err == nil {
		return bundled
	}
	// Fallback: next to the executable
	return filepath.Join(filepath.Dir(execPath), "hippocampus-hook")
}

func (a *App) bundledBinaryPath(name string) string {
	execPath, _ := os.Executable()
	appDir := filepath.Dir(filepath.Dir(execPath))
	return filepath.Join(appDir, "Resources", name)
}

// Tunables is the frontend-facing struct for the settings sliders UI.
// Contains only the tunable fields grouped by section.
type Tunables struct {
	Ingest        config.IngestConfig        `json:"ingest"`
	Consolidation config.ConsolidationConfig `json:"consolidation"`
	Hook          config.HookConfig          `json:"hook"`
	MCP           config.MCPConfig           `json:"mcp"`
	Memory        config.MemoryConfig        `json:"memory"`
	Epistemic     config.EpistemicConfig     `json:"epistemic"`
	Daemon        config.DaemonConfig        `json:"daemon"`
	Log           config.LogConfig           `json:"log"`
}

// GetTunables returns the current tunable config values for the settings UI.
func (a *App) GetTunables() Tunables {
	return Tunables{
		Ingest:        a.fullConfig.Ingest,
		Consolidation: a.fullConfig.Consolidation,
		Hook:          a.fullConfig.Hook,
		MCP:           a.fullConfig.MCP,
		Memory:        a.fullConfig.Memory,
		Epistemic:     a.fullConfig.Epistemic,
		Daemon:        a.fullConfig.Daemon,
		Log:           a.fullConfig.Log,
	}
}

// SaveTunables saves updated tunable values and persists to disk.
// Merges carefully to avoid clobbering Ollama overrides and subsystem configs
// that the tunables UI doesn't manage.
func (a *App) SaveTunables(t Tunables) error {
	a.fullConfig.Ingest = t.Ingest
	a.fullConfig.Consolidation = t.Consolidation
	a.fullConfig.Hook = t.Hook
	a.fullConfig.MCP = t.MCP
	a.fullConfig.Memory = t.Memory
	a.fullConfig.Epistemic = t.Epistemic
	a.fullConfig.Log = t.Log

	// Daemon: merge only the scalar fields the UI manages.
	// Preserve Ollama overrides and subsystem Enabled states (managed separately).
	a.fullConfig.Daemon.GPUConcurrency = t.Daemon.GPUConcurrency
	a.fullConfig.Daemon.BacklogBatch = t.Daemon.BacklogBatch
	a.fullConfig.Daemon.BacklogPauseS = t.Daemon.BacklogPauseS
	a.fullConfig.Daemon.CorecallThreshold = t.Daemon.CorecallThreshold
	a.fullConfig.Daemon.RecalledTTLH = t.Daemon.RecalledTTLH
	// Condenser tunables
	a.fullConfig.Daemon.Condenser.MinUserChars = t.Daemon.Condenser.MinUserChars
	a.fullConfig.Daemon.Condenser.MinAssistantChars = t.Daemon.Condenser.MinAssistantChars
	a.fullConfig.Daemon.Condenser.MinOtherChars = t.Daemon.Condenser.MinOtherChars
	a.fullConfig.Daemon.Condenser.MaxOutputChars = t.Daemon.Condenser.MaxOutputChars
	// NOT touching: QueueKey (not in UI), Daemon.Ollama, Daemon.Classifier,
	// Daemon.Extractor, Daemon.Verifier, Daemon.Linker — those are managed by
	// SetDaemonSubsystem() and SetDaemonModel().

	a.saveConfig()
	return nil
}
