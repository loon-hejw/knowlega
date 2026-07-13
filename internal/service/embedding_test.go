package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleEmbeddingProvider(t *testing.T) {
	var gotModel string
	var gotInput string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotModel = req.Model
		gotInput = req.Input
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"embedding": []float32{0.1, 0.2, 0.3},
			}},
		})
	}))
	defer server.Close()

	provider := OpenAICompatibleEmbeddingProvider{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "text-embedding-test",
		Client:  server.Client(),
	}
	embedding, err := provider.EmbedText(context.Background(), " token validation ")
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "text-embedding-test" || gotInput != "token validation" {
		t.Fatalf("request model/input=%s/%s", gotModel, gotInput)
	}
	if len(embedding) != 3 || embedding[1] != 0.2 {
		t.Fatalf("embedding=%+v", embedding)
	}
}

func TestOpenAICompatibleEmbeddingProviderRetriesV1EmbeddingsOnRoot404(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/embeddings" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"embedding": []float32{0.8, 0.9},
			}},
		})
	}))
	defer server.Close()

	provider := OpenAICompatibleEmbeddingProvider{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "text-embedding-test",
		Client:  server.Client(),
	}
	embedding, err := provider.EmbedText(context.Background(), "token validation")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/embeddings" || paths[1] != "/v1/embeddings" {
		t.Fatalf("paths=%v", paths)
	}
	if len(embedding) != 2 || embedding[0] != 0.8 {
		t.Fatalf("embedding=%+v", embedding)
	}
}

func TestOpenAICompatibleEmbeddingProviderBudgetsLongInput(t *testing.T) {
	var gotInput string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotInput = req.Input
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"embedding": []float32{0.1},
			}},
		})
	}))
	defer server.Close()

	provider := OpenAICompatibleEmbeddingProvider{
		BaseURL:       server.URL,
		APIKey:        "test-key",
		Model:         "text-embedding-test",
		Client:        server.Client(),
		MaxInputChars: 200,
	}
	_, err := provider.EmbedText(context.Background(), strings.Repeat("长文本", 200))
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(gotInput)); got > 200 {
		t.Fatalf("embedding input was not bounded, got %d runes", got)
	}
	if !strings.Contains(gotInput, "prompt context truncated") {
		t.Fatalf("expected truncation marker in input: %q", gotInput)
	}
}
