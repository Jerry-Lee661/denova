package config

import "testing"

func TestResolveEmbeddingConfigDefaultsDisabledAndClamps(t *testing.T) {
	disabled := ResolveEmbeddingConfig(nil)
	if disabled.Enabled() {
		t.Fatal("nil settings must stay disabled")
	}
	if disabled.TimeoutSeconds != DefaultEmbeddingTimeoutSeconds || disabled.BatchSize != DefaultEmbeddingBatchSize {
		t.Fatalf("defaults: %+v", disabled)
	}

	resolved := ResolveEmbeddingConfig(&EmbeddingSettings{
		BaseURL:        " http://127.0.0.1:8289/v1/ ",
		Model:          " embeddinggemma-2 ",
		QueryPrefix:    "task: search result | query: ",
		DocumentPrefix: "title: none | text: ",
		TimeoutSeconds: 60,
		BatchSize:      MaxEmbeddingBatchSize + 10,
	})
	if !resolved.Enabled() {
		t.Fatal("base URL must enable the endpoint")
	}
	if resolved.BaseURL != "http://127.0.0.1:8289/v1" {
		t.Fatalf("base URL not normalized: %q", resolved.BaseURL)
	}
	if resolved.Model != "embeddinggemma-2" || resolved.TimeoutSeconds != 60 {
		t.Fatalf("resolved fields: %+v", resolved)
	}
	if resolved.BatchSize != MaxEmbeddingBatchSize {
		t.Fatalf("batch size not clamped: %d", resolved.BatchSize)
	}
	if resolved.QueryPrefix != "task: search result | query: " || resolved.DocumentPrefix != "title: none | text: " {
		t.Fatalf("prefixes must pass through verbatim: %+v", resolved)
	}
}

func TestEmbeddingSettingsMergeReplacesWholeSection(t *testing.T) {
	parent := Settings{Embedding: &EmbeddingSettings{BaseURL: "http://host-a/v1", Model: "model-a"}}
	child := Settings{Embedding: &EmbeddingSettings{BaseURL: "http://host-b/v1"}}
	merged := Merge(parent, child)
	if merged.Embedding == nil || merged.Embedding.BaseURL != "http://host-b/v1" {
		t.Fatalf("child section must win: %+v", merged.Embedding)
	}
	if merged.Embedding.Model != "" {
		t.Fatalf("section replacement should not keep parent fields: %+v", merged.Embedding)
	}
	kept := Merge(parent, Settings{})
	if kept.Embedding == nil || kept.Embedding.Model != "model-a" {
		t.Fatalf("missing child section must inherit: %+v", kept.Embedding)
	}
}

func TestSettingsFromEmbeddingConfigDropsDisabledSection(t *testing.T) {
	if section := settingsFromEmbeddingConfig(EmbeddingConfig{}); section != nil {
		t.Fatalf("disabled config must not persist a section: %+v", section)
	}
	section := settingsFromEmbeddingConfig(EmbeddingConfig{BaseURL: "http://host/v1", BatchSize: 16, TimeoutSeconds: 120})
	if section == nil || section.BaseURL != "http://host/v1" {
		t.Fatalf("enabled config must round-trip: %+v", section)
	}
}
