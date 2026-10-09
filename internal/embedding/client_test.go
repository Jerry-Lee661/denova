package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"denova/config"
)

func testConfig(serverURL string) config.EmbeddingConfig {
	return config.EmbeddingConfig{
		BaseURL:        serverURL,
		Model:          "test-model",
		QueryPrefix:    "query: ",
		DocumentPrefix: "doc: ",
		TimeoutSeconds: 30,
		BatchSize:      2,
	}
}

type recordedRequest struct {
	Inputs []string
	Model  string
}

func embeddingServer(t *testing.T, requests *[]recordedRequest, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*requests = append(*requests, recordedRequest{Inputs: body.Input, Model: body.Model})
		// Return shuffled indexes to prove caller-side reordering.
		data := make([]map[string]any, 0, len(body.Input))
		for index := len(body.Input) - 1; index >= 0; index-- {
			vector := make([]float32, dim)
			vector[0] = float32(index + 1)
			data = append(data, map[string]any{"index": index, "embedding": vector})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": body.Model, "object": "list"})
	}))
}

func TestEmbedQueryAppliesPrefixAndReorders(t *testing.T) {
	var requests []recordedRequest
	server := embeddingServer(t, &requests, 4)
	defer server.Close()

	client, err := New(testConfig(server.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vector, err := client.EmbedQuery(context.Background(), "主角是谁")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(vector) != 4 || vector[0] != 1 {
		t.Fatalf("unexpected vector %v", vector)
	}
	if len(requests) != 1 || len(requests[0].Inputs) != 1 || requests[0].Inputs[0] != "query: 主角是谁" {
		t.Fatalf("query prefix not applied: %+v", requests)
	}
	if requests[0].Model != "test-model" {
		t.Fatalf("model not sent: %+v", requests[0])
	}
}

func TestEmbedDocumentsBatchesInOrder(t *testing.T) {
	var requests []recordedRequest
	server := embeddingServer(t, &requests, 3)
	defer server.Close()

	client, err := New(testConfig(server.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vectors, err := client.EmbedDocuments(context.Background(), []string{"一", "二", "三", "四", "五"})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if len(vectors) != 5 {
		t.Fatalf("want 5 vectors, got %d", len(vectors))
	}
	for index, vector := range vectors {
		// Server fills vector[0] with (local index + 1), so a correct client
		// returns monotonic values despite the shuffled response.
		if vector[0] != float32(index%2+1) {
			t.Fatalf("vector %d out of order: %v", index, vector[0])
		}
	}
	if len(requests) != 3 {
		t.Fatalf("want 3 batched requests, got %d", len(requests))
	}
	if requests[0].Inputs[0] != "doc: 一" || requests[2].Inputs[0] != "doc: 五" {
		t.Fatalf("document prefix not applied: %+v", requests)
	}
}

func TestEmbedRejectsErrorStatusAndCountMismatch(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	client, err := New(testConfig(failing.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.EmbedQuery(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status error, got %v", err)
	}

	mismatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	}))
	defer mismatch.Close()
	client, err = New(testConfig(mismatch.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.EmbedQuery(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "0 vectors") {
		t.Fatalf("want count mismatch error, got %v", err)
	}
}

func TestEmbedDocumentsFallsBackPerItemWhenBatchIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, text := range body.Input {
			if strings.Contains(text, "too-long") {
				http.Error(w, `{"error":"input too large"}`, http.StatusInternalServerError)
				return
			}
		}
		data := make([]map[string]any, 0, len(body.Input))
		for index := range body.Input {
			data = append(data, map[string]any{"index": index, "embedding": []float32{1, 0}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client, err := New(testConfig(server.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vectors, err := client.EmbedDocuments(context.Background(), []string{"a", "too-long text", "b", "c"})
	if err != nil {
		t.Fatalf("a rejected input must not fail the batch: %v", err)
	}
	if len(vectors) != 4 {
		t.Fatalf("want 4 slots, got %d", len(vectors))
	}
	if len(vectors[1]) != 0 {
		t.Fatalf("rejected input should keep a nil vector, got %v", vectors[1])
	}
	for _, index := range []int{0, 2, 3} {
		if len(vectors[index]) != 2 {
			t.Fatalf("input %d lost its vector: %v", index, vectors[index])
		}
	}
}

func TestEmbedDocumentsAllItemsRejectedReturnsError(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	client, err := New(testConfig(failing.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.EmbedDocuments(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("when every input fails the call must report an error")
	}
}

func TestNewValidation(t *testing.T) {
	var calls atomic.Int32
	if _, err := New(config.EmbeddingConfig{}); err == nil {
		t.Fatal("disabled config must not build a client")
	}
	if _, err := New(config.EmbeddingConfig{BaseURL: "not a url"}); err == nil {
		t.Fatal("invalid base URL must be rejected")
	}
	if _, err := New(config.EmbeddingConfig{BaseURL: "ftp://host/v1"}); err == nil {
		t.Fatal("non-http scheme must be rejected")
	}
	_ = calls.Load()
}
