package tools

import (
	"os"
	"strings"
	"testing"

	"denova/config"
)

// TestLiveSearchTool runs the real search tool (the same entry point the
// Agent calls) against a live OpenAI-compatible embedding service. It is
// skipped unless DENOVA_EMBEDDING_BASE_URL points at one, for example:
//
//	DENOVA_EMBEDDING_BASE_URL=http://127.0.0.1:8289/v1 go test ./internal/agents/tools/ -run LiveSearchTool -v
func TestLiveSearchTool(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("DENOVA_EMBEDDING_BASE_URL"))
	if baseURL == "" {
		t.Skip("DENOVA_EMBEDDING_BASE_URL is not set; skipping the live search tool test")
	}
	workspace := searchFixtureWorkspace(t)
	definitions, err := newSearchTools(workspace, t.TempDir(), config.EmbeddingConfig{
		BaseURL:        baseURL,
		Model:          strings.TrimSpace(os.Getenv("DENOVA_EMBEDDING_MODEL")),
		QueryPrefix:    "task: search result | query: ",
		DocumentPrefix: "title: none | text: ",
		TimeoutSeconds: 120,
		BatchSize:      8,
	})
	if err != nil {
		t.Fatalf("newSearchTools: %v", err)
	}
	result := runSearchTool(t, definitions, `{"query":"寂寞"}`)
	t.Logf("live search output:\n%s", result.ModelContent)
	if !strings.Contains(result.ModelContent, "chapters/第002章.md") {
		t.Fatalf("live semantic search did not rank the paraphrase chapter first:\n%s", result.ModelContent)
	}
}
