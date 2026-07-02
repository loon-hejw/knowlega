package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
