// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package ollama provides a shared HTTP client for the Ollama API.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client is an HTTP client for the Ollama /api/generate endpoint.
type Client struct {
	BaseURL        string
	Model          string
	TimeoutMinutes int
	// WedgeTimeout is how long to wait for a new token before declaring the model wedged.
	// If zero, defaults to 90 seconds.
	WedgeTimeout time.Duration
}

// GenerateOptions holds optional parameters for Generate calls.
type GenerateOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

// generateRequest is the JSON body sent to /api/generate.
type generateRequest struct {
	Model     string           `json:"model"`
	Prompt    string           `json:"prompt"`
	Stream    bool             `json:"stream"`
	Options   *GenerateOptions `json:"options,omitempty"`
	KeepAlive interface{}      `json:"keep_alive,omitempty"`
}

// generateResponse is the JSON body returned from /api/generate (non-streaming).
type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// streamChunk is a single streamed response object from Ollama.
type streamChunk struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// RunningModel represents a model currently loaded in memory.
type RunningModel struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Size      int64  `json:"size"`
	SizeVRAM  int64  `json:"size_vram"`
	ExpiresAt string `json:"expires_at"`
}

// psResponse is the response from GET /api/ps.
type psResponse struct {
	Models []RunningModel `json:"models"`
}

// ErrWedged is returned when the model stops producing tokens.
var ErrWedged = fmt.Errorf("ollama: model wedged (no tokens received within timeout)")

// New creates a new Ollama client.
func New(baseURL, model string, timeoutMinutes int) *Client {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 10
	}
	return &Client{
		BaseURL:        baseURL,
		Model:          model,
		TimeoutMinutes: timeoutMinutes,
		WedgeTimeout:   90 * time.Second,
	}
}

// Generate sends a prompt to Ollama and returns the response text.
// It uses streaming mode with wedge detection.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.GenerateWithOptions(ctx, prompt, c.Model, nil)
}

// GenerateWithModel sends a prompt using a specific model override.
func (c *Client) GenerateWithModel(ctx context.Context, prompt, model string) (string, error) {
	return c.GenerateWithOptions(ctx, prompt, model, nil)
}

// GenerateWithOptions sends a prompt with full control over model and generation options.
// Uses streaming mode with wedge detection: if no token arrives within WedgeTimeout,
// returns ErrWedged.
func (c *Client) GenerateWithOptions(ctx context.Context, prompt, model string, opts *GenerateOptions) (string, error) {
	reqBody := generateRequest{
		Model:   model,
		Prompt:  prompt,
		Stream:  true, // Always stream for wedge detection
		Options: opts,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/api/generate"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// No overall timeout — wedge detection (per-token timeout) and context cancellation
	// handle liveness. An HTTP-level timeout kills long-but-healthy generations on large windows.
	httpClient := &http.Client{}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Ollama returned %d: %s", resp.StatusCode, string(body))
	}

	// Read streaming NDJSON response with wedge detection
	return c.readStream(ctx, resp.Body)
}

// readStream reads an NDJSON stream from Ollama, accumulating the response.
// If no new data arrives within WedgeTimeout, it returns ErrWedged.
func (c *Client) readStream(ctx context.Context, body io.Reader) (string, error) {
	wedgeTimeout := c.WedgeTimeout
	if wedgeTimeout <= 0 {
		wedgeTimeout = 90 * time.Second // fallback only if misconfigured
	}

	var result strings.Builder
	scanner := bufio.NewScanner(body)
	// Default bufio.MaxScanTokenSize is 64KB — Ollama can produce NDJSON lines far
	// larger than that on dense prompts (e.g. large 3h summary windows). 1MB is generous.
	const maxTokenSize = 1024 * 1024 // 1MB
	scanner.Buffer(make([]byte, 0, maxTokenSize), maxTokenSize)

	// Channel-based reading for timeout detection
	type scanResult struct {
		line string
		err  error
		done bool // scanner returned false
	}

	lineCh := make(chan scanResult, 1)

	// Read lines in a goroutine
	go func() {
		for scanner.Scan() {
			lineCh <- scanResult{line: scanner.Text()}
		}
		if err := scanner.Err(); err != nil {
			lineCh <- scanResult{err: err, done: true}
		} else {
			lineCh <- scanResult{done: true}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return result.String(), ctx.Err()

		case <-time.After(wedgeTimeout):
			return result.String(), ErrWedged

		case sr := <-lineCh:
			if sr.err != nil {
				return result.String(), fmt.Errorf("reading stream: %w", sr.err)
			}
			if sr.done {
				// Stream ended without a done:true chunk — unusual but not fatal
				return result.String(), nil
			}

			line := strings.TrimSpace(sr.line)
			if line == "" {
				continue
			}

			var chunk streamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// Skip malformed lines
				slog.Debug("skipping malformed stream chunk", "line", line, "err", err)
				continue
			}

			result.WriteString(chunk.Response)

			if chunk.Done {
				return result.String(), nil
			}
		}
	}
}

// ForceUnload sends an empty prompt with keep_alive=0 to force the model out of memory.
func (c *Client) ForceUnload(ctx context.Context) error {
	return c.ForceUnloadModel(ctx, c.Model)
}

// ForceUnloadModel forces a specific model out of memory.
func (c *Client) ForceUnloadModel(ctx context.Context, model string) error {
	reqBody := generateRequest{
		Model:     model,
		Prompt:    "",
		Stream:    false,
		KeepAlive: 0,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling unload request: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/api/generate"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("unload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unload returned %d: %s", resp.StatusCode, string(body))
	}

	slog.Info("model unloaded", "model", model)
	return nil
}

// ListRunning returns the list of models currently loaded in memory.
func (c *Client) ListRunning(ctx context.Context) ([]RunningModel, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/api/ps"
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("listing running models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ps returned %d", resp.StatusCode)
	}

	var psResp psResponse
	if err := json.NewDecoder(resp.Body).Decode(&psResp); err != nil {
		return nil, fmt.Errorf("decoding ps response: %w", err)
	}

	return psResp.Models, nil
}

// IsModelLoaded checks if the configured model is currently loaded in memory.
func (c *Client) IsModelLoaded(ctx context.Context) (bool, error) {
	models, err := c.ListRunning(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range models {
		if m.Name == c.Model || m.Model == c.Model {
			return true, nil
		}
	}
	return false, nil
}

// IsHealthy checks if Ollama is reachable and responding.
func (c *Client) IsHealthy(ctx context.Context) bool {
	_, err := c.ListRunning(ctx)
	return err == nil
}

// GenerateRaw sends a prompt and returns the raw response bytes (for callers that
// need to parse the full JSON response themselves). Uses non-streaming mode.
func (c *Client) GenerateRaw(ctx context.Context, body []byte) ([]byte, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/api/generate"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: time.Duration(c.TimeoutMinutes) * time.Minute}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
