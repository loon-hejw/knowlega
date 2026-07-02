package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleQueryAgentParsesPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role": "assistant",
					"content": `{
						"intent":"answer_from_persistent_wiki",
						"read_first":["wiki/index.md"],
						"searches":[{"text":"西梁女国 女国","weight":8,"rationale":"alias expansion"}],
						"candidate_limit":4,
						"answer_mode":"llm_synthesis",
						"can_write_back":true
					}`,
				},
			}},
		})
	}))
	defer server.Close()

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
	}
	plan, err := agent.PlanQuery(QueryPlanningInput{
		Question: "女儿国 唐僧 八戒",
		Index:    "- [[wiki/sources/chapter-054]] 西梁女国",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Searches) != 1 {
		t.Fatalf("searches=%+v", plan.Searches)
	}
	if plan.Searches[0].Text != "西梁女国 女国" {
		t.Fatalf("unexpected search text: %+v", plan.Searches[0])
	}
	if plan.CandidateLimit != 4 {
		t.Fatalf("candidate_limit=%d", plan.CandidateLimit)
	}
}

func TestOpenAICompatibleQueryAgentDoesNotForceSearches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role": "assistant",
					"content": `{
						"intent":"answer_from_persistent_wiki",
						"read_first":["wiki/index.md","wiki/concepts/routing.md"],
						"candidate_limit":4,
						"answer_mode":"llm_tool_loop",
						"can_write_back":true
					}`,
				},
			}},
		})
	}))
	defer server.Close()

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
	}
	plan, err := agent.PlanQuery(QueryPlanningInput{Question: "routing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Searches) != 0 {
		t.Fatalf("planner should not force direct search, got %+v", plan.Searches)
	}
}

func TestOpenAICompatibleQueryAgentParsesNextAction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) < 2 || !strings.Contains(request.Messages[1].Content, "Navigation observations:") {
			t.Fatalf("navigation section missing from request: %+v", request.Messages)
		}
		if !strings.Contains(request.Messages[1].Content, "wiki/concepts/oauth.md") {
			t.Fatalf("navigation page missing from request: %s", request.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": `{"action":"read","path":"wiki/concepts/oauth.md","rationale":"inspect the concept page"}`,
				},
			}},
		})
	}))
	defer server.Close()

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
	}
	action, err := agent.NextQueryAction(QueryActionInput{
		Question: "How is token validation handled?",
		Navigation: []QueryNavigationObservation{{
			Action: "list_pages",
			Query:  "oauth",
			Pages: []QueryNavigationPage{{
				Path:  "wiki/concepts/oauth.md",
				Title: "OAuth Token Validation",
				Type:  "concept",
				Score: 42,
			}},
		}},
		Step: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action.Action != "read" || action.Path != "wiki/concepts/oauth.md" {
		t.Fatalf("action=%+v", action)
	}
}
