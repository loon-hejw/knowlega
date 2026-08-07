package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
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

func TestOpenAICompatibleQueryAgentParsesDirectChatPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role": "assistant",
					"content": `{
						"intent":"direct_chat",
						"candidate_limit":0,
						"answer_mode":"direct_chat",
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
	plan, err := agent.PlanQuery(QueryPlanningInput{Question: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Intent != "direct_chat" || plan.AnswerMode != "direct_chat" {
		t.Fatalf("plan=%+v", plan)
	}
	if len(plan.ReadFirst) != 0 || len(plan.Searches) != 0 || plan.CanWriteBack {
		t.Fatalf("direct chat plan should not read/search/writeback: %+v", plan)
	}
}

func TestOpenAICompatibleQueryAgentFallsBackWhenPlannerReturnsInvalidJSON(t *testing.T) {
	for _, content := range []string{"", "I will search the wiki first."} {
		t.Run("content="+content, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			agent := OpenAICompatibleQueryAgent{
				BaseURL: server.URL,
				APIKey:  "test-key",
				Model:   "test-model",
				Client:  server.Client(),
			}
			plan, err := agent.PlanQuery(QueryPlanningInput{Question: "预算审批流程是什么"})
			if err != nil {
				t.Fatal(err)
			}
			if plan.AnswerMode != "llm_synthesis_with_fallback_plan" {
				t.Fatalf("answer_mode=%q", plan.AnswerMode)
			}
			if plan.CanWriteBack {
				t.Fatalf("fallback plan should not be writeback eligible")
			}
			if len(plan.Searches) != 1 || plan.Searches[0].Text != "预算审批流程是什么" {
				t.Fatalf("fallback searches=%+v", plan.Searches)
			}
			if len(plan.ReadFirst) != 2 {
				t.Fatalf("read_first=%+v", plan.ReadFirst)
			}
		})
	}
}

func TestOpenAICompatibleQueryAgentRetriesPlannerInvalidJSON(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		content := "I will search the wiki first."
		if call == 2 {
			content = `{
				"intent":"answer_from_persistent_wiki",
				"read_first":["wiki/index.md"],
				"searches":[{"text":"预算审批","weight":8,"rationale":"retry recovered planner JSON"}],
				"candidate_limit":4,
				"answer_mode":"llm_synthesis",
				"can_write_back":true
			}`
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

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
		RetryOptions: llmretry.Options{
			Retries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	plan, err := agent.PlanQuery(QueryPlanningInput{Question: "预算审批流程是什么"})
	if err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("expected one retry, got %d calls", call)
	}
	if plan.AnswerMode != "llm_synthesis" || len(plan.Searches) != 1 || plan.Searches[0].Text != "预算审批" {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestOpenAICompatibleQueryAgentRetriesHTTPTransientFailure(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			http.Error(w, `{"error":{"message":"temporary overload"}}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": "recovered answer",
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
		RetryOptions: llmretry.Options{
			Retries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	answer, err := agent.SynthesizeQuery(QuerySynthesisInput{Question: "token auth"})
	if err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("expected one retry, got %d calls", call)
	}
	if answer != "recovered answer" {
		t.Fatalf("answer=%q", answer)
	}
}

func TestOpenAICompatibleQueryAgentDoesNotRetryAuthFailure(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
		RetryOptions: llmretry.Options{
			Retries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	if _, err := agent.SynthesizeQuery(QuerySynthesisInput{Question: "token auth"}); err == nil {
		t.Fatal("expected auth failure")
	}
	if call != 1 {
		t.Fatalf("expected no retry for auth failure, got %d calls", call)
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

func TestDecodeQueryTurnDecisionEnvelope(t *testing.T) {
	canWriteBack := false
	decision, err := decodeQueryTurnDecision(`{
  "intent":"wiki_query",
  "resolved_question":"谁满足全部条件？",
  "reasoning_mode":"constraint_satisfaction",
  "requirements":[{"id":"1","text":"见过孙悟空","kind":"positive"}],
  "hypotheses":[{"candidate":"唐太宗","rationale":"回朝接见","suggested_reads":["wiki/entities/唐太宗.md"]}],
  "require_all_requirements":true,
  "can_write_back":false,
  "action":{"action":"search","query":"见过孙悟空","limit":10}
}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Intent != QueryIntentWikiQuery || decision.Action.Action != "search" || len(decision.Requirements) != 1 || !decision.RequireAll {
		t.Fatalf("decision=%+v", decision)
	}
	if len(decision.Hypotheses) != 1 || decision.Hypotheses[0].Candidate != "唐太宗" {
		t.Fatalf("hypotheses=%+v", decision.Hypotheses)
	}
	if decision.CanWriteBack == nil || *decision.CanWriteBack != canWriteBack {
		t.Fatalf("can_write_back=%v", decision.CanWriteBack)
	}
}

func TestDecodeQueryTurnDecisionAcceptsIdenticalDuplicateObjects(t *testing.T) {
	content := `{"action":"search","query":"candidate gap","limit":5}{"action":"search","query":"candidate gap","limit":5}`
	decision, normalized, err := decodeQueryTurnDecisionWithNormalization(content)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized || decision.Action.Action != "search" || decision.Action.Query != "candidate gap" {
		t.Fatalf("decision=%+v normalized=%t", decision, normalized)
	}
}

func TestDecodeQueryTurnDecisionRejectsConflictingDuplicateObjects(t *testing.T) {
	_, _, err := decodeQueryTurnDecisionWithNormalization(`{"action":"search","query":"one"}{"action":"read","path":"wiki/entities/two.md"}`)
	if err == nil || !strings.Contains(err.Error(), "conflicting JSON objects") {
		t.Fatalf("err=%v", err)
	}
}

func TestDecodeQueryTurnDecisionAllowsCommentaryAndMarkdownFenceAroundSingleObject(t *testing.T) {
	decision, normalized, err := decodeQueryTurnDecisionWithNormalization("reasoning before output\n```json\n{\"action\":\"discover_candidates\"}\n```\ntrailing note")
	if err != nil {
		t.Fatal(err)
	}
	if normalized || decision.Action.Action != "discover_candidates" {
		t.Fatalf("decision=%+v normalized=%t", decision, normalized)
	}
}

func TestValidateQueryCandidateAuditResponseRequiresEveryCandidateAndRequirement(t *testing.T) {
	packs := []QueryCandidateEvidencePack{{Candidate: "Alpha"}, {Candidate: "Beta"}}
	requirements := []core.QueryRequirement{{ID: "1"}, {ID: "2"}}
	checks := []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported"}, {RequirementID: "2", Status: "unknown"}}
	if err := validateQueryCandidateAuditResponse([]core.QueryHypothesis{{Candidate: "Alpha", Checks: checks}}, packs, requirements); err == nil || !strings.Contains(err.Error(), "missing: Beta") {
		t.Fatalf("err=%v", err)
	}
	if err := validateQueryCandidateAuditResponse([]core.QueryHypothesis{{Candidate: "Alpha", Checks: checks}, {Candidate: "Beta", Checks: checks}}, packs, requirements); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConstraintFirstTurnRequiresCandidateDiscovery(t *testing.T) {
	input := QueryActionInput{Step: 1}
	decision := core.QueryTurnDecision{
		ReasoningMode: "constraint_satisfaction",
		RequireAll:    true,
		Requirements: []core.QueryRequirement{
			{ID: "1", Text: "condition one", Kind: "positive"},
			{ID: "2", Text: "condition two", Kind: "negative"},
		},
		Action: core.QueryAction{Action: "search", Query: "premature candidate"},
	}
	if err := validateQueryTurnDecision(input, decision); err == nil {
		t.Fatal("premature first-turn candidate search was accepted")
	}
	decision.Action = core.QueryAction{Action: "discover_candidates"}
	if err := validateQueryTurnDecision(input, decision); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAICompatibleQueryAgentRetriesActionInvalidJSON(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		content := `{"action":"writeback","title":"`
		if call == 2 {
			content = `{"action":"writeback","title":"Token Auth","answer":"Token validation calls AuthService [wiki/concepts/oauth.md].","rationale":"save reusable answer"}`
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

	agent := OpenAICompatibleQueryAgent{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Client:  server.Client(),
		RetryOptions: llmretry.Options{
			Retries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	action, err := agent.NextQueryAction(QueryActionInput{
		Question: "How is token validation handled?",
		Docs: []QueryReadDocument{{
			Path:    "wiki/concepts/oauth.md",
			Title:   "OAuth",
			Kind:    "wiki-page",
			Content: "Token validation calls AuthService.",
		}},
		Step: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("expected one retry, got %d calls", call)
	}
	if action.Action != "writeback" || action.Title != "Token Auth" {
		t.Fatalf("action=%+v", action)
	}
}

func TestOpenAICompatibleQueryAgentReturnsActionParseErrorAfterRetries(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": `{"action":"writeback","title":"`,
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
		RetryOptions: llmretry.Options{
			Retries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		},
	}
	_, err := agent.NextQueryAction(QueryActionInput{Question: "token auth", Step: 1})
	if err == nil {
		t.Fatal("expected action parse error")
	}
	if call != 2 {
		t.Fatalf("expected one retry before failure, got %d calls", call)
	}
	if !strings.Contains(err.Error(), "parse llm query action") {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenAICompatibleQueryAgentBudgetsLargeSynthesisPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.MaxTokens != 123 {
			t.Fatalf("max_tokens=%d", request.MaxTokens)
		}
		if len(request.Messages) < 2 {
			t.Fatalf("expected system and user messages, got %+v", request.Messages)
		}
		if got := len([]rune(request.Messages[1].Content)); got > 3900 {
			t.Fatalf("user prompt was not bounded, got %d runes", got)
		}
		if !strings.Contains(request.Messages[1].Content, "prompt context truncated") {
			t.Fatalf("expected truncation marker in prompt:\n%s", request.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": "bounded answer",
				},
			}},
		})
	}))
	defer server.Close()

	agent := OpenAICompatibleQueryAgent{
		BaseURL:         server.URL,
		APIKey:          "test-key",
		Model:           "test-model",
		Client:          server.Client(),
		MaxInputChars:   4000,
		MaxOutputTokens: 123,
	}
	answer, err := agent.SynthesizeQuery(QuerySynthesisInput{
		Question: "large prompt",
		Docs: []QueryReadDocument{{
			Path:    "wiki/concepts/large.md",
			Title:   "Large",
			Kind:    "wiki-page",
			Content: strings.Repeat("长期上下文", 2000),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "bounded answer" {
		t.Fatalf("answer=%q", answer)
	}
}

func TestBudgetRecentReadDocumentsKeepsNewestEvidence(t *testing.T) {
	docs := []QueryReadDocument{
		{Path: "wiki/old.md", Content: strings.Repeat("旧", 6000)},
		{Path: "wiki/middle.md", Content: strings.Repeat("中", 6000)},
		{Path: "wiki/new.md", Content: strings.Repeat("新", 6000)},
	}
	bounded := budgetRecentReadDocuments(docs, 7000)
	if len(bounded) != 2 || bounded[len(bounded)-1].Path != "wiki/new.md" {
		t.Fatalf("newest evidence was not retained: %+v", bounded)
	}
	for _, doc := range bounded {
		if doc.Path == "wiki/old.md" {
			t.Fatalf("oldest evidence displaced newer evidence: %+v", bounded)
		}
	}
}

func TestBudgetQueryActionInputPinsCandidateAndTopRecallEvidence(t *testing.T) {
	docs := []QueryReadDocument{
		{Path: "wiki/sources/chapter-100.md", Content: strings.Repeat("终", 6000)},
		{Path: "wiki/entities/唐太宗.md", Title: "唐太宗", Content: strings.Repeat("唐", 6000)},
		{Path: "wiki/unrelated-old.md", Content: strings.Repeat("旧", 6000)},
		{Path: "wiki/unrelated-new.md", Content: strings.Repeat("新", 6000)},
	}
	input := budgetQueryActionInput(QueryActionInput{
		Docs:    docs,
		Results: []core.QueryResult{{Path: "wiki/sources/chapter-100.md"}},
		Trace: []core.QueryTraceStep{{Action: core.QueryAction{
			Action: "final", Candidate: "唐太宗",
		}}},
	}, 7000)
	paths := map[string]bool{}
	for _, doc := range input.Docs {
		paths[doc.Path] = true
	}
	if !paths["wiki/entities/唐太宗.md"] || !paths["wiki/sources/chapter-100.md"] {
		t.Fatalf("candidate evidence was displaced by recency: %+v", input.Docs)
	}
}
