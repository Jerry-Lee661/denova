package searchindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEmbedder struct {
	model     string
	documents int
	batches   int
}

// conceptVector maps text onto a tiny deterministic space so tests can prove
// the semantic channel matches paraphrases that share no literal tokens.
func conceptVector(text string) []float32 {
	switch {
	case strings.Contains(text, "孤独") || strings.Contains(text, "寂寞"):
		return []float32{1, 0, 0}
	case strings.Contains(text, "战斗") || strings.Contains(text, "交锋"):
		return []float32{0, 1, 0}
	default:
		return []float32{0, 0, 1}
	}
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return conceptVector(text), nil
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.batches++
	f.documents += len(texts)
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		vectors[index] = conceptVector(text)
	}
	return vectors, nil
}

func writeWorkspaceFile(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func testWorkspace(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "chapters/第001章.md",
		"# 初入宗门\n\n少年林澈背着长剑走进山门，第一次看见云海。")
	writeWorkspaceFile(t, workspace, "chapters/第002章.md",
		"# 屋顶的夜\n\n他独自坐在屋顶上，看着远处的灯火，心底涌起一阵孤独。")
	writeWorkspaceFile(t, workspace, "setting/outline.md",
		"# 大纲\n\n第一卷：林澈入门，结识同门。")
	writeWorkspaceFile(t, workspace, "setting/lore/items.json",
		`{"version":2,"items":[{"id":"1","name":"林澈","content":"主角，孤独的剑客"}]}`)
	writeWorkspaceFile(t, workspace, ".hidden/secret.md", "隐藏目录不应被索引。")
	indexDir := filepath.Join(t.TempDir(), "search-index")
	return workspace, indexDir
}

func TestKeywordSearchRanksMatchingFileFirst(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	results, err := Search(context.Background(), Options{Workspace: workspace, IndexDir: indexDir}, "林澈", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results for 林澈")
	}
	if results[0].Path != "chapters/第001章.md" && results[0].Path != "setting/outline.md" {
		t.Fatalf("unexpected top result: %+v", results[0])
	}
	for _, result := range results {
		if result.Path == "setting/lore/items.json" {
			t.Fatalf("lore store must be excluded, got %+v", result)
		}
		if strings.HasPrefix(result.Path, ".hidden/") {
			t.Fatalf("hidden directories must be excluded, got %+v", result)
		}
	}
}

func TestSemanticChannelMatchesParaphrase(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	embedder := &fakeEmbedder{model: "fake-semantic"}
	results, err := Search(context.Background(), Options{Workspace: workspace, IndexDir: indexDir, Embedder: embedder}, "寂寞", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results for 寂寞")
	}
	// 寂寞 appears nowhere in the workspace text; only the semantic channel can
	// rank the 孤独-topic chapter first.
	if results[0].Path != "chapters/第002章.md" {
		t.Fatalf("semantic channel did not rank the paraphrase chunk first: %+v", results)
	}
}

func TestEmbeddingCacheAvoidsReembeddingUnchangedChunks(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	embedder := &fakeEmbedder{model: "fake-semantic"}
	options := Options{Workspace: workspace, IndexDir: indexDir, Embedder: embedder}
	if _, err := Search(context.Background(), options, "林澈", 5); err != nil {
		t.Fatalf("first search: %v", err)
	}
	firstDocuments := embedder.documents
	if firstDocuments == 0 {
		t.Fatal("first build embedded nothing")
	}
	// A new process would reload from disk; simulate by clearing the memory
	// cache so the persisted embedding cache must serve the vectors.
	memoryCache.Delete(indexDir)
	writeWorkspaceFile(t, workspace, "chapters/第003章.md", "# 新的章节\n\n又是一段全新的内容，需要新的向量。")
	if _, err := Search(context.Background(), options, "林澈", 5); err != nil {
		t.Fatalf("second search: %v", err)
	}
	added := embedder.documents - firstDocuments
	if added <= 0 {
		t.Fatal("the new chapter should be embedded")
	}
	if added > 4 {
		t.Fatalf("cache should reuse unchanged chunks, %d documents were re-embedded", added)
	}
}

// rejectingEmbedder refuses marked texts so tests can prove that one rejected
// input keeps keyword coverage instead of failing the whole index.
type rejectingEmbedder struct{ model string }

func (r *rejectingEmbedder) Model() string { return r.model }

func (r *rejectingEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return conceptVector(text), nil
}

func (r *rejectingEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		if strings.Contains(text, "巨长段落") {
			continue
		}
		vectors[index] = conceptVector(text)
	}
	return vectors, nil
}

func TestRejectedChunkKeepsKeywordCoverageAndIndexReloads(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "chapters/第001章.md", "# 一\n\n林澈背着一柄长剑走进山门。")
	writeWorkspaceFile(t, workspace, "chapters/第002章.md", "# 二\n\n巨长段落里藏着独特词汇云海灯。")
	indexDir := filepath.Join(t.TempDir(), "search-index")
	options := Options{Workspace: workspace, IndexDir: indexDir, Embedder: &rejectingEmbedder{model: "rejecting"}}

	results, err := Search(context.Background(), options, "独特词汇", 5)
	if err != nil {
		t.Fatalf("search with a rejected input: %v", err)
	}
	found := false
	for _, result := range results {
		if result.Path == "chapters/第002章.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("keyword channel lost the rejected chunk: %+v", results)
	}
	// The persisted index must stay aligned and reusable after a reload.
	memoryCache.Delete(indexDir)
	reloaded, err := Search(context.Background(), options, "长剑", 5)
	if err != nil {
		t.Fatalf("reloaded search: %v", err)
	}
	if len(reloaded) == 0 || reloaded[0].Path != "chapters/第001章.md" {
		t.Fatalf("reloaded index lost content: %+v", reloaded)
	}
}

func TestKeywordOnlyWorksWithoutEmbedder(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	results, err := Search(context.Background(), Options{Workspace: workspace, IndexDir: indexDir}, "长剑", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].Path != "chapters/第001章.md" {
		t.Fatalf("keyword-only search failed: %+v", results)
	}
	// The vector file must not exist for a keyword-only index.
	if _, err := os.Stat(filepath.Join(indexDir, vectorsFileName)); !os.IsNotExist(err) {
		t.Fatalf("keyword-only index should not persist vectors (stat err: %v)", err)
	}
}

func TestIndexRebuildsWhenFilesChangeAndSurvivesDeletion(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	options := Options{Workspace: workspace, IndexDir: indexDir}
	if results, err := Search(context.Background(), options, "旧词", 5); err != nil || len(results) != 0 {
		t.Fatalf("baseline search: %v %v", results, err)
	}
	writeWorkspaceFile(t, workspace, "chapters/第001章.md", "# 初入宗门\n\n旧词在这里出现了。")
	results, err := Search(context.Background(), options, "旧词", 5)
	if err != nil || len(results) == 0 {
		t.Fatalf("rebuilt search failed: %v %v", results, err)
	}
	// Deleting the index directory must not break the next search.
	if err := os.RemoveAll(indexDir); err != nil {
		t.Fatalf("remove index: %v", err)
	}
	memoryCache.Delete(indexDir)
	if results, err := Search(context.Background(), options, "旧词", 5); err != nil || len(results) == 0 {
		t.Fatalf("search after index deletion failed: %v %v", results, err)
	}
}

func TestSearchValidation(t *testing.T) {
	workspace, indexDir := testWorkspace(t)
	if _, err := Search(context.Background(), Options{Workspace: workspace, IndexDir: indexDir}, "  ", 5); err == nil {
		t.Fatal("empty query must be rejected")
	}
	if _, err := Search(context.Background(), Options{IndexDir: indexDir}, "x", 5); err == nil {
		t.Fatal("missing workspace must be rejected")
	}
	if _, err := Search(context.Background(), Options{Workspace: workspace}, "x", 5); err == nil {
		t.Fatal("missing index dir must be rejected")
	}
}

func TestDefaultIndexDir(t *testing.T) {
	if got := DefaultIndexDir("D:/book", ""); got != filepath.Join("D:/book", "."+DefaultIndexDirName) {
		t.Fatalf("workspace fallback: %q", got)
	}
	if got := DefaultIndexDir("D:/book", "D:/store"); got != filepath.Join("D:/store", DefaultIndexDirName) {
		t.Fatalf("store location: %q", got)
	}
}
