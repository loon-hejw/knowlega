package compiler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/llmretry"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestNewProviderPropagatesAnthropicProtocol(t *testing.T) {
	cfg := config.Defaults().LLM
	cfg.Protocol = "anthropic"
	cfg.BaseURL = "https://anthropic.example/v1"
	cfg.APIKey = "test-key"
	cfg.Model = "claude-test"
	cfg.UserAgent = "claude-cli/2.1.205 (external, cli)"
	cfg.AnthropicVersion = "2024-01-01"

	provider, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	actual, ok := provider.(OpenAICompatibleProvider)
	if !ok {
		t.Fatalf("provider type=%T", provider)
	}
	if actual.Protocol != "anthropic" || actual.AnthropicVersion != "2024-01-01" || actual.UserAgent != cfg.UserAgent || !actual.DisableThinking {
		t.Fatalf("provider protocol config=%+v", actual)
	}
}

func TestOpenAICompatibleProviderWorksWithValidateFlow(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		call++
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		content := "# Analysis\n\n- Create an OAuth concept page."
		if call == 1 {
			analysisPrompt := request.Messages[len(request.Messages)-1].Content
			for _, want := range []string{"at most 12 total ---FILE blocks", "at most 3 new non-summary pages"} {
				if !strings.Contains(analysisPrompt, want) {
					t.Fatalf("analysis prompt missing %q: %s", want, analysisPrompt)
				}
			}
		}
		if call == 2 {
			if len(request.Messages) < 1 ||
				!strings.Contains(request.Messages[0].Content, "Always include exactly one source-summary page") ||
				!strings.Contains(request.Messages[0].Content, "add that wording to the target page aliases") {
				t.Fatalf("generate prompt missing alias-link rule: %+v", request.Messages)
			}
			userPrompt := request.Messages[len(request.Messages)-1].Content
			for _, want := range []string{"at most 4 total ---FILE blocks", "at most 3 new non-summary pages", "primary language of the current source"} {
				if !strings.Contains(userPrompt, want) {
					t.Fatalf("generate prompt missing %q: %s", want, userPrompt)
				}
			}
			if enabled, ok := request.ChatTemplateKwargs["enable_thinking"].(bool); !ok || enabled {
				t.Fatalf("thinking must be disabled for wiki compilation: %+v", request.ChatTemplateKwargs)
			}
			sourceRel := valueAfterLinePrefix(request.Messages[len(request.Messages)-1].Content, "Source path: ")
			content = fmt.Sprintf(`---FILE: wiki/sources/test-source.md
---
type: "source-summary"
title: "Test Source"
sources:
  - "%s"
confidence: "EXTRACTED"
---

# Test Source

See [[oauth-token-validation]].

---FILE: wiki/concepts/oauth-token-validation.md
---
type: "concept"
title: "OAuth Token Validation"
sources:
  - "%s"
confidence: "EXTRACTED"
aliases: "token auth"
---

# OAuth Token Validation

Token validation calls the auth service.

---REVIEW: suggestion | Expand AuthService
SEARCH: AuthService token validation`, sourceRel, sourceRel)
		}
		if call == 3 {
			if request.MaxTokens != 2048 {
				t.Fatalf("page gate max_tokens=%d", request.MaxTokens)
			}
			content = `{"accepted":["wiki/concepts/oauth-token-validation.md"],"rejected":[]}`
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
			BaseURL:         server.URL,
			APIKey:          "test-key",
			Model:           "test-model",
			Client:          server.Client(),
			DisableThinking: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if call != 3 {
		t.Fatalf("expected analyze, generate, and page-gate calls, got %d", call)
	}
	if len(result.Files) != 2 {
		t.Fatalf("files=%v", result.Files)
	}
	page, err := os.ReadFile(filepath.Join(root, "wiki", "concepts", "oauth-token-validation.md"))
	if err != nil {
		t.Fatal(err)
	}
	frontmatter, err := parseGeneratedFrontmatter(string(page))
	if err != nil {
		t.Fatal(err)
	}
	if aliases := frontmatterStrings(frontmatter["aliases"]); len(aliases) != 1 || aliases[0] != "token auth" {
		t.Fatalf("generated page lost aliases frontmatter:\n%s", page)
	}
	if result.ReviewCount != 1 {
		t.Fatalf("review count=%d", result.ReviewCount)
	}
}

func valueAfterLinePrefix(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func TestOpenAICompatibleProviderRetriesTransientFailures(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch call {
		case 1:
			http.Error(w, `{"error":{"message":"temporary overload"}}`, http.StatusServiceUnavailable)
			return
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]string{
						"role":    "assistant",
						"content": "",
					},
				}},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": "recovered compiler output",
				},
			}},
		})
	}))
	defer server.Close()

	provider := OpenAICompatibleProvider{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
		RetryOptions: llmretry.Options{
			Retries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	output, err := provider.chat("system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if call != 3 {
		t.Fatalf("expected two retries, got %d calls", call)
	}
	if output != "recovered compiler output" {
		t.Fatalf("output=%q", output)
	}
}
