// Package ollama provides a shared HTTP client for the Ollama API.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an HTTP client for the Ollama /api/generate endpoint.
type Client struct {
	BaseURL        string
	Model          string
	TimeoutMinutes int
}

// GenerateOptions holds optional parameters for Generate calls.
type GenerateOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

// generateRequest is the JSON body sent to /api/generate.
type generateRequest struct {
	Model   string           `json:"model"`
	Prompt  string           `json:"prompt"`
	Stream  bool             `json:"stream"`
	Options *GenerateOptions `json:"options,omitempty"`
}

// generateResponse is the JSON body returned from /api/generate.
type generateResponse struct {
	Response string `json:"response"`
}

// New creates a new Ollama client.
func New(baseURL, model string, timeoutMinutes int) *Client {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 10
	}
	return &Client{
		BaseURL:        baseURL,
		Model:          model,
		TimeoutMinutes: timeoutMinutes,
	}
}

// Generate sends a prompt to Ollama and returns the response text.
// It uses the client's configured model and timeout.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.GenerateWithOptions(ctx, prompt, c.Model, nil)
}

// GenerateWithModel sends a prompt using a specific model override.
func (c *Client) GenerateWithModel(ctx context.Context, prompt, model string) (string, error) {
	return c.GenerateWithOptions(ctx, prompt, model, nil)
}

// GenerateWithOptions sends a prompt with full control over model and generation options.
func (c *Client) GenerateWithOptions(ctx context.Context, prompt, model string, opts *GenerateOptions) (string, error) {
	reqBody := generateRequest{
		Model:   model,
		Prompt:  prompt,
		Stream:  false,
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

	httpClient := &http.Client{Timeout: time.Duration(c.TimeoutMinutes) * time.Minute}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Ollama returned %d: %s", resp.StatusCode, string(body))
	}

	var olResp generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&olResp); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	return olResp.Response, nil
}

// GenerateRaw sends a prompt and returns the raw response bytes (for callers that
// need to parse the full JSON response themselves).
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
