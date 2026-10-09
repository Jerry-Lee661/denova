package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/alfredxw/denova/agent"

	"denova/config"
)

func writeSearchFixture(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func searchFixtureWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	writeSearchFixture(t, workspace, "chapters/第001章.md", "# 初见\n\n林澈背着长剑走进山门，第一次看见云海。")
	writeSearchFixture(t, workspace, "chapters/第002章.md", "# 屋顶的夜\n\n他独自坐在屋顶上，看着远处的灯火，心底涌起一阵孤独。")
	return workspace
}

func runSearchTool(t *testing.T, definitions []agent.ToolDefinition, input string) agent.ToolResult {
	t.Helper()
	if len(definitions) != 1 {
		t.Fatalf("want exactly one search tool, got %d", len(definitions))
	}
	info, err := definitions[0].Tool.Info(context.Background())
	if err != nil {
		t.Fatalf("tool info: %v", err)
	}
	if info.Name != "search" {
		t.Fatalf("tool name %q", info.Name)
	}
	if capability := definitions[0].Descriptor.Capability; capability != config.AgentToolBookSearch {
		t.Fatalf("capability %q", capability)
	}
	result, err := definitions[0].Tool.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("run search: %v", err)
	}
	if result.Status != agent.ToolResultSuccess {
		t.Fatalf("search failed: %s", result.ModelContent)
	}
	return result
}

func fakeEmbeddingServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, 0, len(body.Input))
		for index, text := range body.Input {
			var vector []float32
			switch {
			case strings.Contains(text, "孤独") || strings.Contains(text, "寂寞"):
				vector = []float32{1, 0, 0}
			default:
				vector = []float32{0, 0, 1}
			}
			data = append(data, map[string]any{"index": index, "embedding": vector})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func TestSearchToolKeywordOnlyPersistsIndexInStore(t *testing.T) {
	workspace := searchFixtureWorkspace(t)
	storeRoot := t.TempDir()
	definitions, err := newSearchTools(workspace, storeRoot, config.EmbeddingConfig{})
	if err != nil {
		t.Fatalf("newSearchTools: %v", err)
	}
	result := runSearchTool(t, definitions, `{"query":"长剑"}`)
	if !strings.Contains(result.ModelContent, "chapters/第001章.md") {
		t.Fatalf("keyword result missing the source path:\n%s", result.ModelContent)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "search-index", "manifest.json")); err != nil {
		t.Fatalf("index should live under the project store: %v", err)
	}
}

func TestSearchToolUsesSemanticChannel(t *testing.T) {
	workspace := searchFixtureWorkspace(t)
	calls := 0
	server := fakeEmbeddingServer(t, &calls)
	defer server.Close()

	definitions, err := newSearchTools(workspace, t.TempDir(), config.EmbeddingConfig{
		BaseURL: server.URL + "/v1", Model: "fake", BatchSize: 8, TimeoutSeconds: 30,
		QueryPrefix: "query: ", DocumentPrefix: "doc: ",
	})
	if err != nil {
		t.Fatalf("newSearchTools: %v", err)
	}
	// 寂寞 appears nowhere in the fixture text; only the semantic channel can
	// rank the lonely-night chapter first.
	result := runSearchTool(t, definitions, `{"query":"寂寞","limit":3}`)
	if !strings.Contains(result.ModelContent, "chapters/第002章.md") {
		t.Fatalf("semantic result missing the paraphrase chapter:\n%s", result.ModelContent)
	}
	if calls == 0 {
		t.Fatal("the embedding endpoint was never called")
	}
}

func TestSearchToolDegradesWhenEndpointIsDown(t *testing.T) {
	workspace := searchFixtureWorkspace(t)
	calls := 0
	server := fakeEmbeddingServer(t, &calls)
	serverURL := server.URL
	server.Close() // endpoint is now unreachable

	definitions, err := newSearchTools(workspace, t.TempDir(), config.EmbeddingConfig{
		BaseURL: serverURL + "/v1", Model: "fake", TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("newSearchTools: %v", err)
	}
	result := runSearchTool(t, definitions, `{"query":"长剑"}`)
	if !strings.Contains(result.ModelContent, "chapters/第001章.md") {
		t.Fatalf("keyword fallback failed when the endpoint is down:\n%s", result.ModelContent)
	}
}

func TestCatalogBookSearchRespectsCapability(t *testing.T) {
	cfg := &config.Config{Workspace: t.TempDir(), ProjectStoreDir: t.TempDir()}
	catalog := NewCatalog(cfg, nil, RuntimeExecutables{})

	disabled, err := catalog.BookSearch(config.ResolvedAgentToolSettings{})
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if len(disabled) != 0 {
		t.Fatalf("disabled capability must register no tool, got %d", len(disabled))
	}
	enabled, err := catalog.BookSearch(config.ResolvedAgentToolSettings{config.AgentToolBookSearch: true})
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if len(enabled) != 1 {
		t.Fatalf("enabled capability must register exactly the search tool, got %d", len(enabled))
	}
}
