package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestOpenAICompatibleWikiReviewAgentReportsSemanticIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var request struct {
			Messages  []chatMessage `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
		}
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
		if !strings.Contains(user, `"aliases":`) || !strings.Contains(strings.ToLower(user), "rose plan") {
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

func TestOpenAICompatibleWikiReviewAgentBudgetsLargeContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages  []chatMessage `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.MaxTokens != 8192 {
			t.Fatalf("max_tokens=%d", request.MaxTokens)
		}
		if len(request.Messages) < 2 {
			t.Fatalf("expected system and user messages, got %+v", request.Messages)
		}
		user := request.Messages[1].Content
		if got := len([]rune(user)); got > 4500 {
			t.Fatalf("review prompt was not bounded, got %d runes", got)
		}
		if !strings.Contains(user, "wiki/concepts/page-299.md") {
			t.Fatalf("bounded review prompt should preserve later page metadata:\n%s", user)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": `{"issues":[]}`,
				},
			}},
		})
	}))
	defer server.Close()

	pages := make([]WikiReviewPage, 0, 300)
	for i := 0; i < 300; i++ {
		pages = append(pages, WikiReviewPage{
			Path:    filepath.ToSlash(filepath.Join("wiki", "concepts", "page-"+fmtInt(i)+".md")),
			Title:   "Page " + fmtInt(i),
			Type:    "concept",
			Excerpt: strings.Repeat("大段页面内容", 200),
		})
	}
	agent := OpenAICompatibleWikiReviewAgent{
		BaseURL:         server.URL,
		APIKey:          "test-key",
		Model:           "test-model",
		Client:          server.Client(),
		MaxInputChars:   5000,
		MaxOutputTokens: 8192,
	}
	issues, err := agent.ReviewWiki(WikiReviewInput{
		Purpose:  strings.Repeat("purpose ", 200),
		Schema:   strings.Repeat("schema ", 200),
		Index:    strings.Repeat("index ", 2000),
		Overview: strings.Repeat("overview ", 1000),
		Pages:    pages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues=%+v", issues)
	}
}

type emptyWikiReviewAgent struct{}

func (emptyWikiReviewAgent) ReviewWiki(WikiReviewInput) ([]LintIssue, error) {
	return nil, nil
}

func TestReviewWikiCarriesForwardOpenDurableSemanticItems(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "durable-review"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "raw", "sources", "garden.md"), "The current ratio is 2:1; an older guide still says 3:1.\n")
	writeFile(t, filepath.Join(root, "wiki", "reviews.md"), `# Reviews

## [2026-08-17] stale-claim | Older guide contradicts current ratio

- Source: `+"`raw/sources/garden.md`"+`
- Status: open

### Affected Pages

- `+"`wiki/concepts/compost.md`"+`

### Detail

The older guide still publishes the retired 3:1 ratio.
`)

	issues, err := ReviewWiki(WikiReviewOptions{ProjectPath: root, Agent: emptyWikiReviewAgent{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Type != "stale-claim" || issues[0].Path != "wiki/concepts/compost.md" {
		t.Fatalf("carried issues=%+v", issues)
	}
	if !strings.Contains(issues[0].Detail, "retired 3:1") {
		t.Fatalf("carried detail=%q", issues[0].Detail)
	}
}

func TestMergeReviewIssuesKeepsDistinctDurableDetails(t *testing.T) {
	issues := mergeReviewIssues(nil, []LintIssue{
		{Type: "stale-claim", Path: "wiki/concepts/compost.md", Detail: "Guide A still says 3:1."},
		{Type: "stale-claim", Path: "wiki/concepts/compost.md", Detail: "Guide B still says 4:1."},
	})
	if len(issues) != 2 {
		t.Fatalf("distinct durable issues were collapsed: %+v", issues)
	}
}

func fmtInt(v int) string {
	return strconv.Itoa(v)
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
