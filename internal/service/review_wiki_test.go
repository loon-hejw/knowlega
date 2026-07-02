package service

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

func TestOpenAICompatibleWikiReviewAgentReportsSemanticIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) < 2 {
			t.Fatalf("expected system and user messages, got %+v", request.Messages)
		}
		user := request.Messages[1].Content
		for _, want := range []string{"wiki/concepts/oauth.md", "OAuth Token Validation", "Gateway only"} {
			if !strings.Contains(user, want) {
				t.Fatalf("review context missing %q:\n%s", want, user)
			}
		}
		for _, want := range []string{"Source Manifest:", "Raw Source Excerpts:", "raw/sources/oauth.md", "runbook says Gateway only"} {
			if !strings.Contains(user, want) {
				t.Fatalf("source context missing %q:\n%s", want, user)
			}
		}
		if !strings.Contains(user, `"aliases":`) || !strings.Contains(user, "rose plan") {
			t.Fatalf("page aliases missing from review context:\n%s", user)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role": "assistant",
					"content": `{
						"issues": [
							{
								"type": "contradiction",
								"path": "wiki/concepts/oauth.md",
								"detail": "Gateway-only token validation conflicts with AuthService ownership."
							}
						]
					}`,
				},
			}},
		})
	}))
	defer server.Close()

	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
aliases:
  - "Rose Plan"
sources:
  - "raw/sources/oauth.md"
---

# OAuth Token Validation

Token validation calls AuthService. Legacy notes still say Gateway only.
`)
	writeFile(t, filepath.Join(root, "wiki", "overview.md"), `# Overview

- [[oauth|OAuth Token Validation]] needs review.
`)
	writeFile(t, filepath.Join(root, "raw", "sources", "oauth.md"), `# OAuth Source

ValidateToken calls AuthService. A legacy runbook says Gateway only.
`)
	writeFile(t, filepath.Join(root, ".kbcore", "source-manifest.json"), `{
  "version": 1,
  "sources": {
    "/tmp/oauth.md": {
      "original_path": "/tmp/oauth.md",
      "sha256": "abc123",
      "raw_path": "raw/sources/oauth.md",
      "title": "OAuth Source",
      "files": ["wiki/concepts/oauth.md"],
      "review_count": 1,
      "updated_at": "2026-07-02T00:00:00Z"
    }
  }
}
`)

	issues, err := ReviewWiki(WikiReviewOptions{
		ProjectPath: root,
		Agent: OpenAICompatibleWikiReviewAgent{
			BaseURL: server.URL,
			APIKey:  "test-key",
			Model:   "test-model",
			Client:  server.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues=%+v", issues)
	}
	if issues[0].Type != "contradiction" || issues[0].Path != "wiki/concepts/oauth.md" || !strings.Contains(issues[0].Detail, "AuthService") {
		t.Fatalf("unexpected issue: %+v", issues[0])
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
