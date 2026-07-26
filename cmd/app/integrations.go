// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// IntegrationTarget describes a supported MCP client for one-click integration.
type IntegrationTarget struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ConfigPath  string `json:"config_path"`
	Installed   bool   `json:"installed"` // true if the config file or parent dir exists
}

// GetIntegrationTargets returns the list of supported MCP clients with their status.
func (a *App) GetIntegrationTargets() []IntegrationTarget {
	home, _ := os.UserHomeDir()

	targets := []IntegrationTarget{
		{
			ID:          "claude-desktop",
			Name:        "Claude Desktop",
			Description: "Anthropic's desktop app",
			ConfigPath:  filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"),
		},
		{
			ID:          "claude-code",
			Name:        "Claude Code",
			Description: "Claude CLI coding agent",
			ConfigPath:  filepath.Join(home, ".claude", "settings.json"),
		},
		{
			ID:          "cursor",
			Name:        "Cursor",
			Description: "AI-native code editor",
			ConfigPath:  filepath.Join(home, ".cursor", "mcp.json"),
		},
		{
			ID:          "kiro",
			Name:        "Kiro CLI",
			Description: "AWS Kiro coding agent",
			ConfigPath:  filepath.Join(home, ".kiro", "settings", "mcp.json"),
		},
		{
			ID:          "windsurf",
			Name:        "Windsurf",
			Description: "Codeium's AI editor",
			ConfigPath:  filepath.Join(home, ".windsurf", "mcp.json"),
		},
		{
			ID:          "gemini",
			Name:        "Gemini CLI",
			Description: "Google's AI coding agent",
			ConfigPath:  filepath.Join(home, ".gemini", "settings.json"),
		},
	}

	// Check which clients are installed (config file or parent dir exists)
	for i := range targets {
		dir := filepath.Dir(targets[i].ConfigPath)
		if _, err := os.Stat(dir); err == nil {
			targets[i].Installed = true
		}
	}

	return targets
}

// IntegrateClient writes Hippocampus MCP config into the specified client's config file.
// It merges with existing config (doesn't clobber other MCP servers).
func (a *App) IntegrateClient(clientID string) error {
	targets := a.GetIntegrationTargets()
	var target *IntegrationTarget
	for i := range targets {
		if targets[i].ID == clientID {
			target = &targets[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("unknown client: %s", clientID)
	}

	mcpPath := a.mcpPath()
	configPath := a.configFilePath()

	// Build our MCP server entry
	hippocampusEntry := map[string]interface{}{
		"command": mcpPath,
		"args":    []string{"--config", configPath},
	}

	// Read existing config (or start fresh)
	var fullConfig map[string]interface{}

	data, err := os.ReadFile(target.ConfigPath)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &fullConfig); err != nil {
			// File exists but is invalid JSON — back it up and start fresh
			backupPath := target.ConfigPath + ".bak." + time.Now().Format("20060102-150405")
			os.WriteFile(backupPath, data, 0o644)
			fullConfig = make(map[string]interface{})
		}
	} else {
		fullConfig = make(map[string]interface{})
	}

	// Ensure mcpServers key exists
	mcpServers, ok := fullConfig["mcpServers"].(map[string]interface{})
	if !ok {
		mcpServers = make(map[string]interface{})
	}

	// Add/update our entry
	mcpServers["hippocampus"] = hippocampusEntry
	fullConfig["mcpServers"] = mcpServers

	// Write back
	out, err := json.MarshalIndent(fullConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Ensure directory exists
	os.MkdirAll(filepath.Dir(target.ConfigPath), 0o755)

	if err := os.WriteFile(target.ConfigPath, out, 0o644); err != nil {
		return fmt.Errorf("writing config to %s: %w", target.ConfigPath, err)
	}

	// For Kiro: also create an agent config with hooks
	if clientID == "kiro" {
		a.installKiroAgent()
	}

	return nil
}

// installKiroAgent creates a hippocampus agent file for Kiro CLI with hooks configured.
func (a *App) installKiroAgent() {
	home, _ := os.UserHomeDir()
	agentDir := filepath.Join(home, ".kiro", "agents")
	agentPath := filepath.Join(agentDir, "hippocampus.json")

	// Don't clobber if it already exists
	if _, err := os.Stat(agentPath); err == nil {
		return
	}

	hookPath := a.hookPath()
	mcpPath := a.mcpPath()
	configPath := a.configFilePath()

	timeoutMs := a.fullConfig.Hook.HookTimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 10000
	}

	agent := map[string]interface{}{
		"name":  "hippocampus",
		"tools": []string{"*"},
		"hooks": map[string]interface{}{
			"userPromptSubmit": []map[string]interface{}{
				{"command": hookPath, "timeout_ms": timeoutMs},
			},
			"stop": []map[string]interface{}{
				{"command": hookPath, "timeout_ms": timeoutMs},
			},
		},
		"mcpServers": map[string]interface{}{
			"hippocampus": map[string]interface{}{
				"command": mcpPath,
				"args":    []string{"--config", configPath},
			},
		},
		"allowedTools": []string{
			"memory_store", "memory_search", "memory_by_tags", "memory_get",
			"memory_delete", "memory_add_tags", "memory_remove_tags",
			"memory_list_tags", "memory_rename_tag", "memory_time_range",
			"memory_link", "memory_unlink", "memory_links",
			"memory_ingest_url", "memory_store_chunked", "memory_get_section",
		},
		"systemPrompt": fmt.Sprintf("You have persistent memory via MCP tools (memory_*). Use them proactively. Config: %s", configPath),
	}

	os.MkdirAll(agentDir, 0o755)
	data, _ := json.MarshalIndent(agent, "", "  ")
	os.WriteFile(agentPath, data, 0o644)
}

// IsClientIntegrated checks if Hippocampus is already configured in a client.
func (a *App) IsClientIntegrated(clientID string) bool {
	targets := a.GetIntegrationTargets()
	var configPath string
	for _, t := range targets {
		if t.ID == clientID {
			configPath = t.ConfigPath
			break
		}
	}
	if configPath == "" {
		return false
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}

	mcpServers, ok := cfg["mcpServers"].(map[string]interface{})
	if !ok {
		return false
	}

	_, exists := mcpServers["hippocampus"]
	return exists
}

// CopyAgentConfig generates the agent JSON config snippet.
func (a *App) CopyAgentConfig() string {
	hookPath := a.hookPath()
	mcpPath := a.mcpPath()
	configPath := a.configFilePath()

	timeoutMs := a.fullConfig.Hook.HookTimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 10000
	}

	snippet := fmt.Sprintf(`{
  "hooks": {
    "userPromptSubmit": [{"command": "%s", "timeout_ms": %d}],
    "stop": [{"command": "%s", "timeout_ms": %d}]
  },
  "mcpServers": {
    "hippocampus": {
      "command": "%s",
      "args": ["--config", "%s"]
    }
  }
}`, hookPath, timeoutMs, hookPath, timeoutMs, mcpPath, configPath)

	return snippet
}
