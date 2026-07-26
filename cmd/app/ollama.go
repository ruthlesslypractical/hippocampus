// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// OllamaModel represents an installed model.
type OllamaModel struct {
	Name       string `json:"name"`
	Size       string `json:"size"`       // human-readable
	SizeBytes  int64  `json:"size_bytes"` // raw bytes
	ModifiedAt string `json:"modified_at"`
}

// PullStatus tracks download progress for a model pull.
type PullStatus struct {
	Model     string  `json:"model"`
	Status    string  `json:"status"`    // "downloading", "verifying", "success", "error"
	Progress  float64 `json:"progress"`  // 0-100
	Total     int64   `json:"total"`     // total bytes
	Completed int64   `json:"completed"` // bytes downloaded
	Error     string  `json:"error,omitempty"`
}

var (
	pullMu      sync.Mutex
	pullActives = make(map[string]*PullStatus)
)

// StartOllama starts or connects to Ollama.
func (a *App) StartOllama() error {
	if a.config.OllamaMode == "local" {
		// Check if already running
		if a.checkOllama() {
			return nil
		}
		// Try system ollama first (likely has models already), bundled as fallback
		ollamaPath := "ollama"
		if _, err := exec.LookPath("ollama"); err != nil {
			// No system install — use bundled
			ollamaPath = a.bundledBinaryPath("ollama")
			if _, err := os.Stat(ollamaPath); err != nil {
				return fmt.Errorf("Ollama not found (install from ollama.com or rebuild the app bundle)")
			}
		}
		cmd := exec.Command(ollamaPath, "serve")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting Ollama: %w", err)
		}
		a.ollamaCmd = cmd
		// Wait for it to be ready
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			if a.checkOllama() {
				return nil
			}
		}
		return fmt.Errorf("Ollama started but not responding")
	}
	return nil
}

// StopOllama stops the Ollama process if we spawned it.
func (a *App) StopOllama() error {
	if a.ollamaCmd != nil && a.ollamaCmd.Process != nil {
		a.ollamaCmd.Process.Kill()
		a.ollamaCmd = nil
	}
	return nil
}

// ListOllamaModels returns all models available on the connected Ollama instance.
func (a *App) ListOllamaModels() ([]OllamaModel, error) {
	resp, err := http.Get(a.ollamaBaseURL() + "/api/tags")
	if err != nil {
		return nil, fmt.Errorf("connecting to Ollama: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing model list: %w", err)
	}

	var models []OllamaModel
	for _, m := range result.Models {
		models = append(models, OllamaModel{
			Name:       m.Name,
			Size:       formatBytes(m.Size),
			SizeBytes:  m.Size,
			ModifiedAt: m.ModifiedAt,
		})
	}
	return models, nil
}

// PullOllamaModel starts pulling a model in the background. Progress is tracked via GetPullStatus.
func (a *App) PullOllamaModel(model string) error {
	pullMu.Lock()
	if s, ok := pullActives[model]; ok && s.Status == "downloading" {
		pullMu.Unlock()
		return fmt.Errorf("already pulling %s", model)
	}
	pullActives[model] = &PullStatus{Model: model, Status: "starting", Progress: 0}
	pullMu.Unlock()

	go a.doPull(model)
	return nil
}

func (a *App) doPull(model string) {
	body, _ := json.Marshal(map[string]interface{}{
		"name":   model,
		"stream": true,
	})

	resp, err := http.Post(a.ollamaBaseURL()+"/api/pull", "application/json", strings.NewReader(string(body)))
	if err != nil {
		pullMu.Lock()
		pullActives[model] = &PullStatus{Model: model, Status: "error", Error: err.Error()}
		pullMu.Unlock()
		return
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := decoder.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			pullMu.Lock()
			pullActives[model] = &PullStatus{Model: model, Status: "error", Error: err.Error()}
			pullMu.Unlock()
			return
		}

		if line.Error != "" {
			pullMu.Lock()
			pullActives[model] = &PullStatus{Model: model, Status: "error", Error: line.Error}
			pullMu.Unlock()
			return
		}

		pullMu.Lock()
		if line.Total > 0 {
			pullActives[model] = &PullStatus{
				Model:     model,
				Status:    "downloading",
				Progress:  float64(line.Completed) / float64(line.Total) * 100,
				Total:     line.Total,
				Completed: line.Completed,
			}
		} else if line.Status == "success" {
			pullActives[model] = &PullStatus{Model: model, Status: "success", Progress: 100}
		} else {
			prev := pullActives[model]
			prevProg := float64(0)
			if prev != nil {
				prevProg = prev.Progress
			}
			pullActives[model] = &PullStatus{Model: model, Status: line.Status, Progress: prevProg}
		}
		pullMu.Unlock()
	}

	pullMu.Lock()
	if s, ok := pullActives[model]; ok && s.Status != "error" {
		pullActives[model] = &PullStatus{Model: model, Status: "success", Progress: 100}
	}
	pullMu.Unlock()
}

// GetPullStatus returns all active/recent pull statuses.
func (a *App) GetPullStatus() []PullStatus {
	pullMu.Lock()
	defer pullMu.Unlock()
	var results []PullStatus
	for _, s := range pullActives {
		results = append(results, *s)
	}
	return results
}

// ClearPullStatus removes a completed/errored pull from tracking.
func (a *App) ClearPullStatus(model string) {
	pullMu.Lock()
	defer pullMu.Unlock()
	delete(pullActives, model)
}

// DeleteOllamaModel removes a model from the Ollama instance.
func (a *App) DeleteOllamaModel(model string) error {
	body, _ := json.Marshal(map[string]string{"name": model})
	req, err := http.NewRequest("DELETE", a.ollamaBaseURL()+"/api/delete", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleting model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("delete failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// SetSummarizerModel sets the model used for summarization/consolidation.
func (a *App) SetSummarizerModel(model string) {
	a.fullConfig.Ollama.Model = model
	a.saveConfig()
}

// SetEmbeddingModel sets the model used for vector embeddings.
func (a *App) SetEmbeddingModel(model string) {
	a.fullConfig.Ollama.EmbeddingModel = model
	a.saveConfig()
}

// GetModelConfig returns the current model selections.
func (a *App) GetModelConfig() map[string]interface{} {
	return map[string]interface{}{
		"summarizer":  a.fullConfig.Ollama.Model,
		"embedding":   a.fullConfig.Ollama.EmbeddingModel,
		"wst_model":   a.fullConfig.WorkingSet.Model,
		"wst_enabled": a.fullConfig.WorkingSet.Enabled,
		"ofc_model":   a.fullConfig.OFC.Model,
		"ofc_enabled": a.fullConfig.OFC.Enabled,
	}
}

// SetWSTEnabled enables or disables the working set tracker.
func (a *App) SetWSTEnabled(enabled bool) {
	a.fullConfig.WorkingSet.Enabled = enabled
	a.saveConfig()
}

// SetWSTModel sets the model used for the working set tracker sidecar.
func (a *App) SetWSTModel(model string) {
	a.fullConfig.WorkingSet.Model = model
	a.saveConfig()
}

func (a *App) ollamaBaseURL() string {
	url := a.config.OllamaURL
	if url == "" {
		url = "http://localhost:11434"
	}
	return strings.TrimRight(url, "/")
}

func (a *App) checkOllama() bool {
	url := a.ollamaBaseURL()
	host := strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://")
	if !strings.Contains(host, ":") {
		host += ":11434"
	}
	conn, err := net.DialTimeout("tcp", host, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", b/(1<<10))
	}
}
