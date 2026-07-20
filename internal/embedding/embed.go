package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// Embedder generates vector embeddings from text via Ollama.
type Embedder struct {
	baseURL string
	model   string
	dims    int
	client  *http.Client
}

// NewEmbedder creates an Embedder from config.
// Returns nil if embedding is not configured (no model specified).
func NewEmbedder(cfg config.OllamaConfig) *Embedder {
	if cfg.EmbeddingModel == "" {
		return nil
	}
	return &Embedder{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.EmbeddingModel,
		dims:    cfg.EmbeddingDimensions,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Dimensions returns the configured embedding dimensions.
func (e *Embedder) Dimensions() int {
	return e.dims
}

// Embed generates a vector embedding for the given text.
// Returns nil if the embedder is nil or the call fails.
func (e *Embedder) Embed(text string) ([]float32, error) {
	if e == nil {
		return nil, nil
	}

	// Truncate input to avoid overwhelming the embedding model
	// Most embedding models cap at ~8192 tokens (~32K chars)
	if len(text) > 32000 {
		text = text[:32000]
	}

	reqBody := embedRequest{
		Model: e.model,
		Input: text,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling embed request: %w", err)
	}

	url := e.baseURL + "/api/embed"
	resp, err := e.client.Post(url, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("calling Ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Ollama embed returned %d: %s", resp.StatusCode, string(body))
	}

	var embedResp embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decoding embed response: %w", err)
	}

	if len(embedResp.Embeddings) == 0 || len(embedResp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}

	// Convert float64 to float32
	embedding := make([]float32, len(embedResp.Embeddings[0]))
	for i, v := range embedResp.Embeddings[0] {
		embedding[i] = float32(v)
	}

	return embedding, nil
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}
