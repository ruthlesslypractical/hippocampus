// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// ServiceStatus describes the state of a background service.
type ServiceStatus struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
}

// GetServiceStatus returns the status of all background services.
func (a *App) GetServiceStatus() []ServiceStatus {
	return []ServiceStatus{
		a.checkService("Summarizer (every 3h)", "com.ruthlesslypractical.hippocampus.summarize"),
		a.checkService("Summarizer (daily)", "com.ruthlesslypractical.hippocampus.summarize-daily"),
		a.checkService("Cross-track (daily)", "com.ruthlesslypractical.hippocampus.summarize-crosstrack"),
		a.checkService("Summarizer (weekly)", "com.ruthlesslypractical.hippocampus.summarize-weekly"),
	}
}

// StartService starts an individual background service by label.
func (a *App) StartService(label string) error {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("service not installed: %s", label)
	}
	err := exec.Command("launchctl", "load", plistPath).Run()
	if err == nil {
		a.config.BackgroundServicesEnabled = true
		a.saveConfig()
	}
	return err
}

// StopService stops an individual background service by label.
func (a *App) StopService(label string) error {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	err := exec.Command("launchctl", "unload", plistPath).Run()
	// Check if ALL services are now stopped — if so, persist disabled
	if err == nil {
		allStopped := true
		for _, s := range a.GetServiceStatus() {
			if s.Running {
				allStopped = false
				break
			}
		}
		if allStopped {
			a.config.BackgroundServicesEnabled = false
			a.saveConfig()
		}
	}
	return err
}

// ensureServicesLoaded loads all installed LaunchAgent plists.
func (a *App) ensureServicesLoaded() {
	home, _ := os.UserHomeDir()
	labels := []string{
		"com.ruthlesslypractical.hippocampus.summarize",
		"com.ruthlesslypractical.hippocampus.summarize-daily",
		"com.ruthlesslypractical.hippocampus.summarize-crosstrack",
		"com.ruthlesslypractical.hippocampus.summarize-weekly",
	}
	for _, label := range labels {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		if _, err := os.Stat(plistPath); err == nil {
			exec.Command("launchctl", "load", plistPath).Run()
		}
	}
}

func (a *App) checkService(name, label string) ServiceStatus {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")

	installed := false
	if _, err := os.Stat(plistPath); err == nil {
		installed = true
	}

	running := false
	if installed {
		out, err := exec.Command("launchctl", "list", label).Output()
		if err == nil && len(out) > 0 {
			running = true
		}
	}

	return ServiceStatus{Name: name, Label: label, Installed: installed, Running: running}
}

// InstallServices installs both LaunchAgent plists and loads them.
func (a *App) InstallServices() error {
	home, _ := os.UserHomeDir()
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	os.MkdirAll(launchAgentsDir, 0o755)

	summarizePath := a.bundledBinaryPath("hippocampus-summarize")
	// Fallback to Resources path
	if _, err := os.Stat(summarizePath); err != nil {
		execPath, _ := os.Executable()
		appDir := filepath.Dir(filepath.Dir(execPath))
		summarizePath = filepath.Join(appDir, "Resources", "hippocampus-summarize")
	}

	configPath := a.configFilePath()
	logDir := a.logsDir()
	os.MkdirAll(logDir, 0o755)

	// Install summarizer (3h)
	if err := a.installPlist(
		"com.ruthlesslypractical.hippocampus.summarize",
		summarizePath, configPath, logDir, launchAgentsDir,
	); err != nil {
		return fmt.Errorf("installing summarizer: %w", err)
	}

	// Install summarizer (daily)
	if err := a.installPlist(
		"com.ruthlesslypractical.hippocampus.summarize-daily",
		summarizePath, configPath, logDir, launchAgentsDir,
	); err != nil {
		return fmt.Errorf("installing daily summarizer: %w", err)
	}

	// Install cross-track (daily)
	if err := a.installPlist(
		"com.ruthlesslypractical.hippocampus.summarize-crosstrack",
		summarizePath, configPath, logDir, launchAgentsDir,
	); err != nil {
		return fmt.Errorf("installing cross-track: %w", err)
	}

	// Install summarizer (weekly)
	if err := a.installPlist(
		"com.ruthlesslypractical.hippocampus.summarize-weekly",
		summarizePath, configPath, logDir, launchAgentsDir,
	); err != nil {
		return fmt.Errorf("installing weekly summarizer: %w", err)
	}

	return nil
}

func (a *App) installPlist(label, binaryPath, configPath, logDir, launchAgentsDir string) error {
	// Read template from bundle
	execPath, _ := os.Executable()
	appDir := filepath.Dir(filepath.Dir(execPath))
	templatePath := filepath.Join(appDir, "Resources", "launchd", label+".plist")

	template, err := os.ReadFile(templatePath)
	if err != nil {
		// Fallback: generate inline
		template = a.generatePlistTemplate(label)
	}

	// Replace placeholders
	content := string(template)
	content = strings.Replace(content, "__SUMMARIZE_PATH__", binaryPath, -1)
	content = strings.Replace(content, "__CONFIG_PATH__", configPath, -1)
	content = strings.Replace(content, "__LOG_DIR__", logDir, -1)

	// Write plist
	plistPath := filepath.Join(launchAgentsDir, label+".plist")
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}

	// Load with launchctl
	exec.Command("launchctl", "unload", plistPath).Run() // ignore error (might not be loaded)
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("loading plist: %w", err)
	}

	return nil
}

func (a *App) generatePlistTemplate(label string) []byte {
	// Fallback template if bundle doesn't have the file
	mode := "--daily"

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>__SUMMARIZE_PATH__</string>
        <string>--config</string>
        <string>__CONFIG_PATH__</string>
        <string>%s</string>
    </array>`, label, mode)

	plist += `
    <key>StartInterval</key>
    <integer>10800</integer>`

	plist += `
    <key>StandardOutPath</key>
    <string>__LOG_DIR__/` + strings.TrimPrefix(label, "com.ruthlesslypractical.hippocampus.") + `.log</string>
    <key>StandardErrorPath</key>
    <string>__LOG_DIR__/` + strings.TrimPrefix(label, "com.ruthlesslypractical.hippocampus.") + `.log</string>
    <key>RunAtLoad</key>
    <true/>
    <key>Nice</key>
    <integer>10</integer>
</dict>
</plist>`

	return []byte(plist)
}

// UninstallServices unloads and removes both LaunchAgent plists.
func (a *App) UninstallServices() error {
	home, _ := os.UserHomeDir()
	labels := []string{
		"com.ruthlesslypractical.hippocampus.summarize",
		"com.ruthlesslypractical.hippocampus.summarize-daily",
		"com.ruthlesslypractical.hippocampus.summarize-crosstrack",
		"com.ruthlesslypractical.hippocampus.summarize-weekly",
	}

	for _, label := range labels {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		exec.Command("launchctl", "unload", plistPath).Run()
		os.Remove(plistPath)
	}

	return nil
}

// --- Daemon Management ---

const daemonLabel = "com.ruthlesslypractical.hippocampus.daemon"

// DaemonStatus returns the current state of the background daemon.
type DaemonStatus struct {
	Running       bool   `json:"running"`
	QueueDepth    int64  `json:"queue_depth"`
	Processed     int64  `json:"processed"`
	LastProcessed string `json:"last_processed"` // relative time string like "2m31s ago"
	LastEntryID   string `json:"last_entry_id"`  // for tooltip
}

// GetDaemonStatus returns the daemon's running state and activity info.
func (a *App) GetDaemonStatus() DaemonStatus {
	status := DaemonStatus{}

	// Check if daemon process is running via launchctl
	out, err := exec.Command("launchctl", "list", daemonLabel).Output()
	if err == nil && len(out) > 0 {
		status.Running = true
	}

	// Get queue depth and processing stats from Redis
	if a.redisClient != nil {
		ctx := context.Background()
		qLen, _ := a.redisClient.LLen(ctx, a.fullConfig.Daemon.QueueKey).Result()
		status.QueueDepth = qLen

		processed, _ := a.redisClient.SCard(ctx, "daemon:processed").Result()
		status.Processed = processed

		// Last processed timestamp
		lastTS, err := a.redisClient.Get(ctx, "daemon:last_processed").Result()
		if err == nil && lastTS != "" {
			var ts int64
			fmt.Sscanf(lastTS, "%d", &ts)
			if ts > 0 {
				elapsed := time.Since(time.Unix(ts, 0))
				status.LastProcessed = formatDuration(elapsed)
			}
		}

		lastID, _ := a.redisClient.Get(ctx, "daemon:last_entry_id").Result()
		status.LastEntryID = lastID
	}

	return status
}

// StartDaemon installs and starts the daemon LaunchAgent.
func (a *App) StartDaemon() error {
	home, _ := os.UserHomeDir()
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	os.MkdirAll(launchAgentsDir, 0o755)

	daemonPath := a.bundledBinaryPath("hippocampus-daemon")
	if _, err := os.Stat(daemonPath); err != nil {
		execPath, _ := os.Executable()
		appDir := filepath.Dir(filepath.Dir(execPath))
		daemonPath = filepath.Join(appDir, "Resources", "hippocampus-daemon")
	}

	configPath := a.configFilePath()
	logDir := a.logsDir()
	os.MkdirAll(logDir, 0o755)

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HIPPOCAMPUS_CONFIG</key>
        <string>%s</string>
    </dict>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>LowPriorityBackgroundIO</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/daemon-stdout.log</string>
    <key>StandardErrorPath</key>
    <string>%s/daemon-stderr.log</string>
</dict>
</plist>`, daemonLabel, daemonPath, configPath, logDir, logDir)

	plistPath := filepath.Join(launchAgentsDir, daemonLabel+".plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("writing daemon plist: %w", err)
	}

	exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("loading daemon plist: %w", err)
	}

	return nil
}

// StopDaemon stops and unloads the daemon LaunchAgent.
func (a *App) StopDaemon() error {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", daemonLabel+".plist")
	return exec.Command("launchctl", "unload", plistPath).Run()
}

// GetDaemonSubsystems returns the enabled/disabled state of each subsystem.
type DaemonSubsystems struct {
	Classifier bool `json:"classifier"`
	Extractor  bool `json:"extractor"`
	Verifier   bool `json:"verifier"`
	Linker     bool `json:"linker"`
	Condenser  bool `json:"condenser"`
}

func (a *App) GetDaemonSubsystems() DaemonSubsystems {
	return DaemonSubsystems{
		Classifier: a.fullConfig.Daemon.Classifier.Enabled,
		Extractor:  a.fullConfig.Daemon.Extractor.Enabled,
		Verifier:   a.fullConfig.Daemon.Verifier.Enabled,
		Linker:     a.fullConfig.Daemon.Linker.Enabled,
		Condenser:  a.fullConfig.Daemon.Condenser.Enabled,
	}
}

// SetDaemonSubsystem enables/disables a specific subsystem and persists to config.
func (a *App) SetDaemonSubsystem(subsystem string, enabled bool) error {
	switch subsystem {
	case "classifier":
		a.fullConfig.Daemon.Classifier.Enabled = enabled
	case "extractor":
		a.fullConfig.Daemon.Extractor.Enabled = enabled
	case "verifier":
		a.fullConfig.Daemon.Verifier.Enabled = enabled
	case "linker":
		a.fullConfig.Daemon.Linker.Enabled = enabled
	case "condenser":
		a.fullConfig.Daemon.Condenser.Enabled = enabled
	default:
		return fmt.Errorf("unknown subsystem: %s", subsystem)
	}
	a.saveConfig()
	return nil
}

// GetGPUConcurrency returns the current GPU concurrency setting.
func (a *App) GetGPUConcurrency() int {
	return a.fullConfig.Daemon.GPUConcurrency
}

// SetGPUConcurrency sets the GPU concurrency and persists to config.
func (a *App) SetGPUConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	a.fullConfig.Daemon.GPUConcurrency = n
	a.saveConfig()
}

// SetDaemonModel sets the Ollama model override for a specific daemon subsystem.
// Empty string clears the override (falls through to daemon/global default).
func (a *App) SetDaemonModel(subsystem, model string) error {
	ensureOverride := func(sub *config.SubsystemConfig) {
		if sub.Ollama == nil {
			sub.Ollama = &config.OllamaOverride{}
		}
		sub.Ollama.Model = model
		if model == "" {
			sub.Ollama = nil // clear override entirely
		}
	}
	switch subsystem {
	case "classifier":
		ensureOverride(&a.fullConfig.Daemon.Classifier)
	case "extractor":
		ensureOverride(&a.fullConfig.Daemon.Extractor)
	case "verifier":
		ensureOverride(&a.fullConfig.Daemon.Verifier)
	case "linker":
		ensureOverride(&a.fullConfig.Daemon.Linker)
	case "condenser":
		if a.fullConfig.Daemon.Condenser.Ollama == nil {
			a.fullConfig.Daemon.Condenser.Ollama = &config.OllamaOverride{}
		}
		a.fullConfig.Daemon.Condenser.Ollama.Model = model
		if model == "" {
			a.fullConfig.Daemon.Condenser.Ollama = nil
		}
	default:
		return fmt.Errorf("unknown subsystem: %s", subsystem)
	}
	a.saveConfig()
	return nil
}

// formatDuration formats a duration as a human-readable relative time.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds ago", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh ago", int(d.Hours())/24, int(d.Hours())%24)
}
