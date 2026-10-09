package config

import "strings"

const (
	DefaultEmbeddingTimeoutSeconds = 120
	DefaultEmbeddingBatchSize      = 16
	MaxEmbeddingBatchSize          = 64
)

// EmbeddingSettings is the persisted OpenAI-compatible embedding endpoint that
// backs the workspace search capability. It is optional: an empty base URL
// disables the semantic channel, and the search tool then serves keyword
// results only. Text embedding models that require task instruction prefixes
// (for example EmbeddingGemma) encode them in QueryPrefix and DocumentPrefix.
type EmbeddingSettings struct {
	BaseURL        string `toml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKey         string `toml:"api_key,omitempty" json:"api_key,omitempty"`
	Model          string `toml:"model,omitempty" json:"model,omitempty"`
	QueryPrefix    string `toml:"query_prefix,omitempty" json:"query_prefix,omitempty"`
	DocumentPrefix string `toml:"document_prefix,omitempty" json:"document_prefix,omitempty"`
	TimeoutSeconds int    `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	BatchSize      int    `toml:"batch_size,omitempty" json:"batch_size,omitempty"`
}

// EmbeddingConfig is the resolved runtime view consumed by the embedding
// client. Limits are filled with defaults; an empty BaseURL means disabled.
type EmbeddingConfig struct {
	BaseURL        string `toml:"base_url" json:"base_url"`
	APIKey         string `toml:"api_key" json:"api_key"`
	Model          string `toml:"model" json:"model"`
	QueryPrefix    string `toml:"query_prefix" json:"query_prefix"`
	DocumentPrefix string `toml:"document_prefix" json:"document_prefix"`
	TimeoutSeconds int    `toml:"timeout_seconds" json:"timeout_seconds"`
	BatchSize      int    `toml:"batch_size" json:"batch_size"`
}

// Enabled reports whether an embedding endpoint is configured. Prefixes are
// intentional no-ops by default so providers without task instructions (for
// example bge-m3) need no extra configuration.
func (c EmbeddingConfig) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != ""
}

func ResolveEmbeddingConfig(settings *EmbeddingSettings) EmbeddingConfig {
	cfg := EmbeddingConfig{
		TimeoutSeconds: DefaultEmbeddingTimeoutSeconds,
		BatchSize:      DefaultEmbeddingBatchSize,
	}
	if settings == nil {
		return cfg
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(settings.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(settings.APIKey)
	cfg.Model = strings.TrimSpace(settings.Model)
	cfg.QueryPrefix = settings.QueryPrefix
	cfg.DocumentPrefix = settings.DocumentPrefix
	if settings.TimeoutSeconds > 0 {
		cfg.TimeoutSeconds = settings.TimeoutSeconds
	}
	if settings.BatchSize > 0 {
		cfg.BatchSize = settings.BatchSize
		if cfg.BatchSize > MaxEmbeddingBatchSize {
			cfg.BatchSize = MaxEmbeddingBatchSize
		}
	}
	return cfg
}

// settingsFromEmbeddingConfig projects a resolved runtime config back to the
// persisted shape. A disabled endpoint stays absent from stored settings.
func settingsFromEmbeddingConfig(cfg EmbeddingConfig) *EmbeddingSettings {
	if !cfg.Enabled() {
		return nil
	}
	return &EmbeddingSettings{
		BaseURL:        cfg.BaseURL,
		APIKey:         cfg.APIKey,
		Model:          cfg.Model,
		QueryPrefix:    cfg.QueryPrefix,
		DocumentPrefix: cfg.DocumentPrefix,
		TimeoutSeconds: cfg.TimeoutSeconds,
		BatchSize:      cfg.BatchSize,
	}
}
