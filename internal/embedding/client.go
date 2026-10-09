// Package embedding is a minimal OpenAI-compatible /embeddings client used by
// the workspace search index. It keeps provider-neutral HTTP concerns out of
// the product tools and supports the task-instruction prefixes that some text
// embedding models (for example EmbeddingGemma) require.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"denova/config"
)

const maxResponseBytes = 64 << 20

// Client sends embedding requests to one endpoint. It is safe for concurrent use.
type Client struct {
	endpoint       string
	apiKey         string
	model          string
	queryPrefix    string
	documentPrefix string
	batchSize      int
	http           *http.Client
}

// New validates the configured endpoint and returns a ready client. A disabled
// configuration reports an error; callers use EnablingConfig/Enabled first.
func New(cfg config.EmbeddingConfig) (*Client, error) {
	if !cfg.Enabled() {
		return nil, errors.New("embedding endpoint is not configured")
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid embedding base URL %q", cfg.BaseURL)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultEmbeddingTimeoutSeconds) * time.Second
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = config.DefaultEmbeddingBatchSize
	}
	return &Client{
		endpoint:       strings.TrimRight(cfg.BaseURL, "/") + "/embeddings",
		apiKey:         cfg.APIKey,
		model:          cfg.Model,
		queryPrefix:    cfg.QueryPrefix,
		documentPrefix: cfg.DocumentPrefix,
		batchSize:      batchSize,
		http:           &http.Client{Timeout: timeout},
	}, nil
}

// Model reports the configured upstream model identifier.
func (c *Client) Model() string { return c.model }

// EmbedQuery embeds one retrieval query with the configured query prefix.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.embed(ctx, []string{c.queryPrefix + text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedDocuments embeds index documents in batches with the document prefix.
// The result order matches the input order.
func (c *Client) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := start + c.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := make([]string, 0, end-start)
		for _, text := range texts[start:end] {
			batch = append(batch, c.documentPrefix+text)
		}
		embedded, err := c.embed(ctx, batch)
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, embedded...)
	}
	return vectors, nil
}

func (c *Client) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	requestBody := map[string]any{
		"input":           inputs,
		"encoding_format": "float",
	}
	if c.model != "" {
		requestBody["model"] = c.model
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embedding endpoint returned status %d: %s", response.StatusCode, truncateForError(body))
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding endpoint returned %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	ordered := make([][]float32, len(parsed.Data))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(ordered) {
			return nil, fmt.Errorf("embedding response index %d out of range", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("embedding at index %d is empty", item.Index)
		}
		ordered[item.Index] = item.Embedding
	}
	return ordered, nil
}

func truncateForError(body []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "..."
	}
	return text
}
