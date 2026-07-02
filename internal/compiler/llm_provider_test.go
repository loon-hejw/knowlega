package compiler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestOpenAICompatibleProviderWorksWithValidateFlow(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		call++
		content := "# Analysis\n\n- Create an OAuth concept page."
		if call == 2 {
			var request chatCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if len(request.Messages) < 1 ||
				!strings.Contains(request.Messages[0].Content, "Always include exactly one source-summary page") ||
				!strings.Contains(request.Messages[0].Content, "add that wording to the target page aliases") {
				t.Fatalf("generate prompt missing alias-link rule: %+v", request.Messages)
			}
			content = `---FILE: wiki/concepts/oauth-token-validation.md
---
type: "concept"
title: "OAuth Token Validation"
sources:
  - "raw/sources/test-source.md"
confidence: "EXTRACTED"
aliases: "token auth"
---

# OAuth Token Validation

Token validation calls the [[authservice]].

---REVIEW: suggestion | Expand AuthService
SEARCH: AuthService token validation`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": content,
				},
			}},
		})
	}))
	defer server.Close()

	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "test-source.md")
	if err := os.WriteFile(source, []byte("# OAuth\n\nToken validation calls the auth service."), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root,
		SourcePath:  source,
		Provider: OpenAICompatibleProvider{
			BaseURL: server.URL,
			APIKey:  "test-key",
			Model:   "test-model",
			Client:  server.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("expected analyze and generate calls, got %d", call)
	}
	if len(result.Files) != 1 {
		t.Fatalf("files=%v", result.Files)
	}
	page, err := os.ReadFile(filepath.Join(root, "wiki", "concepts", "oauth-token-validation.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "aliases: \"token auth\"") {
		t.Fatalf("generated page lost aliases frontmatter:\n%s", page)
	}
	if result.ReviewCount != 1 {
		t.Fatalf("review count=%d", result.ReviewCount)
	}
}
