package searchindex

import (
	"context"
	"os"
	"strings"
	"testing"

	"denova/config"
	"denova/internal/embedding"
)

// TestLiveEmbeddingEndpoint exercises the full chain (embedding client, index
// build, hybrid ranking) against a real OpenAI-compatible embedding service.
// It is skipped unless DENOVA_EMBEDDING_BASE_URL points at one, for example:
//
//	DENOVA_EMBEDDING_BASE_URL=http://127.0.0.1:8289/v1 go test ./internal/book/searchindex/ -run LiveEmbedding -v
func TestLiveEmbeddingEndpoint(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("DENOVA_EMBEDDING_BASE_URL"))
	if baseURL == "" {
		t.Skip("DENOVA_EMBEDDING_BASE_URL is not set; skipping the live embedding test")
	}
	client, err := embedding.New(config.EmbeddingConfig{
		BaseURL:        baseURL,
		Model:          strings.TrimSpace(os.Getenv("DENOVA_EMBEDDING_MODEL")),
		QueryPrefix:    "task: search result | query: ",
		DocumentPrefix: "title: none | text: ",
		TimeoutSeconds: 120,
		BatchSize:      8,
	})
	if err != nil {
		t.Fatalf("embedding.New: %v", err)
	}

	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "chapters/第001章.md",
		"# 初入宗门\n\n少年林澈背着长剑走进山门，第一次看见云海，师兄弟们正在比试剑法。")
	writeWorkspaceFile(t, workspace, "chapters/第002章.md",
		"# 屋顶的夜\n\n他独自坐在屋顶上看着远处的灯火，心里空落落的，说不出的滋味。")
	writeWorkspaceFile(t, workspace, "setting/outline.md",
		"# 大纲\n\n第一卷：林澈入门，结识同门，并在年末大比中崭露头角。")

	options := Options{Workspace: workspace, IndexDir: t.TempDir(), Embedder: client}
	ctx := context.Background()

	keyword, err := Search(ctx, options, "林澈", 5)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if len(keyword) == 0 {
		t.Fatal("keyword search returned no results")
	}
	t.Logf("keyword query 林澈 -> %d results, top: %s (%s)", len(keyword), keyword[0].Path, keyword[0].Title)

	semantic, err := Search(ctx, options, "孤独", 5)
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if len(semantic) == 0 {
		t.Fatal("semantic search returned no results")
	}
	for index, result := range semantic {
		t.Logf("semantic query 孤独 -> #%d %s score=%.4f snippet=%q",
			index+1, result.Path, result.Score, truncateRunes(result.Text, 40))
	}
	if semantic[0].Path != "chapters/第002章.md" {
		t.Fatalf("semantic channel should rank the lonely-night chapter first, got %s", semantic[0].Path)
	}
}

func truncateRunes(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "..."
}
