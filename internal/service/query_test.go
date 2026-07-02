package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestQueryDownranksAggregatePages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "chapter-007-concept.md"), `---
type: "concept"
title: "第七回 八卦炉 五行山"
---

# 第七回 八卦炉 五行山

第七回讲八卦炉和五行山。
`)
	indexPath := filepath.Join(root, "wiki", "index.md")
	mustWrite(t, indexPath, strings.Repeat("第七回 八卦炉 五行山 大圣\n", 20))

	results, err := QueryWiki(root, "第七回 八卦炉 五行山 大圣", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Path == "wiki/index.md" {
		t.Fatalf("aggregate index ranked first: %+v", results[0])
	}
}

func TestLLMWikiQueryUsesPlannerForAliasExpansion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "raw", "sources", "chapter-054.txt"), `《》目录 第五十四回　法性西来逢女国　心猿定计脱烟花
唐僧师徒早到西梁国界，前面城池是西梁女国。八戒在旁贪看。`)
	mustWrite(t, filepath.Join(root, "raw", "sources", "chapter-999.txt"), strings.Repeat("唐僧 八戒 ", 5))

	answer, err := QueryLLMWikiWithAgent(root, "女儿国 唐僧 八戒", 5, fakeQueryAgent{
		plan: core.QueryPlan{
			Question: "女儿国 唐僧 八戒",
			Intent:   "answer_from_persistent_wiki",
			ReadFirst: []string{
				"wiki/index.md",
				"wiki/overview.md",
			},
			Searches: []core.QuerySearch{
				{Text: "女儿国 唐僧 八戒", Weight: 6, Rationale: "user wording"},
				{Text: "西梁女国 女国 西梁国", Weight: 8, Rationale: "planner-expanded aliases from wiki context"},
			},
			CandidateLimit: 5,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := answer.Results
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if !strings.Contains(results[0].Path, "chapter-054.txt") {
		t.Fatalf("expected alias match to rank chapter 054 first, got %+v", results)
	}
	foundRaw := false
	for _, result := range results {
		if result.Kind == "raw-source" && strings.Contains(result.Path, "chapter-054.txt") {
			foundRaw = true
			if !strings.Contains(result.Snippet, "女国") {
				t.Fatalf("snippet lost CJK content: %q", result.Snippet)
			}
		}
	}
	if !foundRaw {
		t.Fatalf("expected raw source result, got %+v", results)
	}
	for _, search := range answer.Plan.Searches {
		if strings.Contains(search.Rationale, "hard-coded") {
			t.Fatalf("planner should not report hard-coded corpus aliases: %+v", answer.Plan.Searches)
		}
	}
}

func TestSearchUsesAliasFrontmatterWithoutHardCodedCorpusRules(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "chapter-054-concept.md"), `---
type: "concept"
title: "第五十四回 法性西来逢女国"
aliases:
  - "女儿国"
  - "西梁女国"
sources:
  - "raw/sources/chapter-054.txt"
---

# 第五十四回 法性西来逢女国

唐僧师徒到西梁国，八戒在旁贪看。
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "high-frequency.md"), `---
type: "concept"
title: "唐僧八戒高频页"
---

# 唐僧八戒高频页

`+strings.Repeat("唐僧 八戒 ", 10))

	results, err := QueryWiki(root, "女儿国 唐僧 八戒", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Path != "wiki/concepts/chapter-054-concept.md" {
		t.Fatalf("expected alias metadata page first, got %+v", results)
	}
}

func TestFallbackQueryIsOfflineReadOnlyAndUsesReadPages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

The complete page says token validation calls AuthService and refreshes the session.
`)

	answer, err := QueryLLMWikiWithAgent(root, "token validation AuthService", 3, FallbackQueryAgent{})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Plan.Intent != "offline_fallback_query" || answer.Plan.AnswerMode != "offline_reading_fallback" {
		t.Fatalf("fallback plan should be explicit offline mode: %+v", answer.Plan)
	}
	if answer.Plan.CanWriteBack {
		t.Fatalf("fallback answer must not be eligible for writeback: %+v", answer.Plan)
	}
	if !strings.Contains(answer.Answer, "complete page says token validation calls AuthService") {
		t.Fatalf("fallback should summarize read page content, got:\n%s", answer.Answer)
	}
	if strings.Contains(answer.Answer, "Token validation") && !strings.Contains(answer.Answer, "refreshes the session") {
		t.Fatalf("fallback appears to be using only search snippet, got:\n%s", answer.Answer)
	}
	if len(answer.Notes) == 0 || !strings.Contains(answer.Notes[0], "offline scaffold") {
		t.Fatalf("fallback note missing: %+v", answer.Notes)
	}
}

func TestQueryLogsToStoreWhenConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth

Token validation calls AuthService.
`)
	logStore := &fakeQueryLogStore{}

	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath:   root,
		ProjectID:     "project-1",
		Question:      "token validation",
		Limit:         3,
		QueryLogStore: logStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if logStore.called != 1 {
		t.Fatalf("expected query log call, got %d", logStore.called)
	}
	if logStore.projectID != "project-1" || logStore.query != "token validation" {
		t.Fatalf("unexpected query log identity: %+v", logStore)
	}
	if logStore.mode != answer.Plan.AnswerMode || logStore.resultCount != len(answer.Results) {
		t.Fatalf("unexpected query log payload: mode=%q result_count=%d answer=%+v", logStore.mode, logStore.resultCount, answer)
	}
	if !strings.HasPrefix(logStore.id, "query-") {
		t.Fatalf("unexpected query log id: %s", logStore.id)
	}
}

func TestLLMWikiQueryCanReadPlannedPagesWithoutSearch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "routing.md"), `---
type: "concept"
title: "Routing Notes"
---

# Routing Notes

The canonical answer is in a directly planned page.
`)

	agent := capturingQueryAgent{
		plan: core.QueryPlan{
			Question:       "routing answer",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/concepts/routing.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "routing answer", 5, &agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Results) != 0 {
		t.Fatalf("expected no keyword recall results, got %+v", answer.Results)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/concepts/routing.md" {
		t.Fatalf("expected planned page citation, got %+v", answer.Citations)
	}
	if len(agent.docs) != 1 {
		t.Fatalf("expected one planned document, got %+v", agent.docs)
	}
	if agent.docs[0].Path != "wiki/concepts/routing.md" || agent.docs[0].Kind != "wiki-navigation" {
		t.Fatalf("unexpected planned doc: %+v", agent.docs[0])
	}
	if !strings.Contains(agent.docs[0].Content, "canonical answer") {
		t.Fatalf("planned doc content not passed to synthesizer: %+v", agent.docs[0])
	}
}

func TestLLMWikiQueryActionLoopSearchesReadsThenFinalizes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

Token validation calls the auth service.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "search", Query: "token validation auth service", Limit: 3, Rationale: "find candidate pages"},
			{Action: "read", Path: "wiki/concepts/oauth.md", Rationale: "read the relevant concept page"},
			{Action: "final", Answer: "Token validation calls the auth service [wiki/concepts/oauth.md].", Rationale: "evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "How is token validation handled?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled {
		t.Fatal("final action answer should not call synthesizer")
	}
	if answer.Answer != "Token validation calls the auth service [wiki/concepts/oauth.md]." {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if len(answer.Trace) != 3 {
		t.Fatalf("trace=%+v", answer.Trace)
	}
	if answer.Trace[0].Action.Action != "search" || !strings.Contains(answer.Trace[0].Observation, "wiki/concepts/oauth.md") {
		t.Fatalf("unexpected search trace: %+v", answer.Trace[0])
	}
	if len(answer.Results) == 0 || answer.Results[0].Path != "wiki/concepts/oauth.md" {
		t.Fatalf("expected search result for oauth page, got %+v", answer.Results)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/concepts/oauth.md" {
		t.Fatalf("expected oauth citation, got %+v", answer.Citations)
	}
	if len(agent.inputs) != 3 {
		t.Fatalf("agent inputs=%d", len(agent.inputs))
	}
	if len(agent.inputs[1].Results) == 0 {
		t.Fatalf("second action should see search results: %+v", agent.inputs[1])
	}
	if !queryDocsContain(agent.inputs[2].Docs, "wiki/concepts/oauth.md") {
		t.Fatalf("final action should see read document: %+v", agent.inputs[2].Docs)
	}
}

func TestLLMWikiQueryActionLoopCanRewriteSearchTerms(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "xiyouji"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-054.md"), `---
type: "source-summary"
title: "第五十四回 法性西来逢女国"
aliases:
  - "女儿国"
---

# 第五十四回 法性西来逢女国

唐僧师徒到达西梁女国，国中王旨求亲。
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "女儿国发生了什么？",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "search", Query: "西梁女国 求亲", Limit: 3, Rationale: "rewrite the user wording into wiki terms"},
			{Action: "final", Answer: "女儿国情节对应西梁女国求亲 [wiki/sources/chapter-054.md].", Rationale: "auto-read search evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "女儿国发生了什么？", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Results) == 0 || answer.Results[0].Path != "wiki/sources/chapter-054.md" {
		t.Fatalf("expected rewritten search to recall chapter 054, got %+v", answer.Results)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/sources/chapter-054.md") {
		t.Fatalf("final action should see auto-read rewritten search result: %+v", agent.inputs[1].Docs)
	}
	if answer.Plan.Intent == "offline_fallback_query" || answer.Plan.AnswerMode == "offline_reading_fallback" {
		t.Fatalf("expected LLM tool-loop plan, got %+v", answer.Plan)
	}
}

func TestMockQueryAgentRunsToolLoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "xiyouji"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-028.md"), `---
type: "source-summary"
title: "第二十八回 花果山群妖聚义"
---

# 第二十八回 花果山群妖聚义

孙悟空回到花果山，重整水帘洞。
`)

	answer, err := QueryLLMWikiWithAgent(root, "花果山", 5, MockQueryAgent{})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Plan.Intent == "offline_fallback_query" || answer.Plan.AnswerMode == "offline_reading_fallback" {
		t.Fatalf("mock query agent should not use offline fallback plan: %+v", answer.Plan)
	}
	if answer.Plan.CanWriteBack {
		t.Fatalf("mock query agent must not be writeback eligible: %+v", answer.Plan)
	}
	if len(answer.Trace) < 3 {
		t.Fatalf("expected action-loop trace, got %+v", answer.Trace)
	}
	if answer.Trace[0].Action.Action != "list_pages" || answer.Trace[1].Action.Action != "read" || answer.Trace[2].Action.Action != "final" {
		t.Fatalf("unexpected mock trace: %+v", answer.Trace)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/sources/chapter-028.md" {
		t.Fatalf("expected chapter citation, got %+v", answer.Citations)
	}
}

func TestMockQueryAgentFallsBackToSearchWhenNavigationMisses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "xiyouji"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-001.md"), `---
type: "source-summary"
title: "第一回 灵根育孕源流出"
---

# 第一回 灵根育孕源流出

石猴发现水帘洞，并带群猴入洞安身。
`)

	answer, err := QueryLLMWikiWithAgent(root, "水帘洞", 5, MockQueryAgent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) < 3 {
		t.Fatalf("expected action-loop trace, got %+v", answer.Trace)
	}
	if answer.Trace[0].Action.Action != "list_pages" || !strings.Contains(answer.Trace[0].Observation, "0 wiki pages") {
		t.Fatalf("expected navigation miss first, got %+v", answer.Trace)
	}
	if answer.Trace[1].Action.Action != "search" || !strings.Contains(answer.Trace[1].Observation, "auto-read 1 candidate document") {
		t.Fatalf("expected search fallback with auto-read, got %+v", answer.Trace)
	}
	if answer.Trace[2].Action.Action != "final" {
		t.Fatalf("expected final after search auto-read, got %+v", answer.Trace)
	}
	if len(answer.Results) == 0 || answer.Results[0].Path != "wiki/sources/chapter-001.md" {
		t.Fatalf("expected search result for chapter body evidence, got %+v", answer.Results)
	}
}

func TestMockQueryAgentUsesGraphForCodeQuestions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "code"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "raw", "code-graphs", "repo", "graphify", "graph.json"), `{
  "nodes": [
    {"id":"auth.validate","kind":"function","label":"ValidateToken","source_file":"internal/auth/token.go"},
    {"id":"auth.service","kind":"class","label":"AuthService","source_file":"internal/auth/service.go"}
  ],
  "edges": [
    {"source":"auth.validate","target":"auth.service","relation":"calls","confidence":"EXTRACTED","weight":1}
  ]
}`)

	answer, err := QueryLLMWikiWithAgent(root, "ValidateToken AuthService calls", 5, MockQueryAgent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) < 2 {
		t.Fatalf("expected graph action-loop trace, got %+v", answer.Trace)
	}
	if answer.Trace[0].Action.Action != "graph" || !strings.Contains(answer.Trace[0].Observation, "raw/code-graphs/repo/graphify/graph.json") {
		t.Fatalf("expected graph evidence first, got %+v", answer.Trace)
	}
	if answer.Trace[1].Action.Action != "final" {
		t.Fatalf("expected final after graph evidence, got %+v", answer.Trace)
	}
	if len(answer.Citations) == 0 || !strings.HasPrefix(answer.Citations[0].Path, "raw/code-graphs/repo/graphify/graph.json#") {
		t.Fatalf("expected graph citation, got %+v", answer.Citations)
	}
}

func TestLLMWikiQueryActionLoopCanSuggestWritebackTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

Token validation calls the auth service.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "read", Path: "wiki/concepts/oauth.md", Rationale: "read durable wiki evidence"},
			{Action: "writeback", Title: "OAuth Token Validation Synthesis", Answer: "Token validation calls the auth service [wiki/concepts/oauth.md].", Rationale: "reusable synthesis"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "How is token validation handled?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if answer.SuggestedWritebackTitle != "OAuth Token Validation Synthesis" {
		t.Fatalf("suggested title=%q", answer.SuggestedWritebackTitle)
	}
	if len(answer.Trace) != 2 || answer.Trace[1].Observation != "writeback suggested" {
		t.Fatalf("expected writeback trace, got %+v", answer.Trace)
	}
	writeback, err := WriteQueryAnswer(QueryWritebackOptions{
		ProjectPath: root,
		Title:       answer.SuggestedWritebackTitle,
		Answer:      answer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeback.Path != "wiki/syntheses/oauth-token-validation-synthesis.md" {
		t.Fatalf("path=%s", writeback.Path)
	}
}

func TestLLMWikiQueryActionLoopCanListPagesBeforeRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "routing.md"), `---
type: "concept"
title: "Routing Notes"
---

# Routing Notes

The router reads the index before choosing pages.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How does routing work?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "list_pages", Query: "routing", Limit: 10, Rationale: "inspect wiki navigation"},
			{Action: "read", Path: "wiki/concepts/routing.md", Rationale: "read listed page"},
			{Action: "final", Answer: "Routing reads the index before choosing pages [wiki/concepts/routing.md].", Rationale: "read evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "How does routing work?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) != 3 || !strings.Contains(answer.Trace[0].Observation, "list_pages returned 1 wiki page") {
		t.Fatalf("expected list_pages trace, got %+v", answer.Trace)
	}
	if !strings.Contains(answer.Trace[0].Observation, "wiki/concepts/routing.md") {
		t.Fatalf("list_pages should return routing page, got %+v", answer.Trace[0])
	}
	if len(agent.inputs[1].Navigation) != 1 || len(agent.inputs[1].Navigation[0].Pages) != 1 {
		t.Fatalf("second action should receive structured navigation pages: %+v", agent.inputs[1].Navigation)
	}
	if agent.inputs[1].Navigation[0].Pages[0].Path != "wiki/concepts/routing.md" {
		t.Fatalf("navigation page mismatch: %+v", agent.inputs[1].Navigation[0].Pages)
	}
	if !queryDocsContain(agent.inputs[2].Docs, "wiki/concepts/routing.md") {
		t.Fatalf("final action should see read listed page: %+v", agent.inputs[2].Docs)
	}
}

func TestLLMWikiQueryReadActionResolvesWikiLinkAliases(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "auth-service.md"), `---
type: "concept"
title: "Auth Service"
aliases:
  - "Token Authority"
---

# Auth Service

Auth Service validates tokens.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "Who validates tokens?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "read", Path: "[[Token Authority]]", Rationale: "read the linked alias"},
			{Action: "final", Answer: "Auth Service validates tokens [wiki/concepts/auth-service.md].", Rationale: "read evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "Who validates tokens?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) != 2 || !strings.Contains(answer.Trace[0].Observation, "wiki/concepts/auth-service.md") {
		t.Fatalf("expected alias read trace, got %+v", answer.Trace)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/auth-service.md") {
		t.Fatalf("final action should see alias-resolved doc: %+v", agent.inputs[1].Docs)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/concepts/auth-service.md" {
		t.Fatalf("expected alias-resolved citation, got %+v", answer.Citations)
	}
}

func TestLLMWikiQueryActionLoopFollowsWikiLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "routing.md"), `---
type: "concept"
title: "Routing Notes"
---

# Routing Notes

See [[Auth Service]] and [Token Validation](token-validation.md).
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "auth-service.md"), `---
type: "concept"
title: "Auth Service"
---

# Auth Service

Auth Service verifies credentials.
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "token-validation.md"), `---
type: "concept"
title: "Token Validation"
---

# Token Validation

Token validation calls Auth Service.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "What does routing depend on?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/concepts/routing.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "follow_links", Path: "wiki/concepts/routing.md", Limit: 5, Rationale: "inspect linked pages"},
			{Action: "final", Answer: "Routing links to Auth Service and Token Validation [wiki/concepts/auth-service.md] [wiki/concepts/token-validation.md].", Rationale: "linked evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "What does routing depend on?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) != 2 || !strings.Contains(answer.Trace[0].Observation, "read 2 linked document") {
		t.Fatalf("expected follow_links trace, got %+v", answer.Trace)
	}
	if len(agent.inputs[1].Navigation) != 1 || len(agent.inputs[1].Navigation[0].Pages) != 2 {
		t.Fatalf("final action should receive structured linked page navigation: %+v", agent.inputs[1].Navigation)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/auth-service.md") || !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/token-validation.md") {
		t.Fatalf("final action should see linked docs: %+v", agent.inputs[1].Docs)
	}
	if len(answer.Citations) != 3 {
		t.Fatalf("expected source plus linked citations, got %+v", answer.Citations)
	}
}

func TestLLMWikiQueryRejectsFinalAfterListPagesOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "routing.md"), `# Routing Notes

The router reads the index.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How does routing work?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "list_pages", Query: "routing", Limit: 10, Rationale: "navigation only"},
			{Action: "final", Answer: "Routing works from the listed page [wiki/concepts/routing.md].", Rationale: "incorrectly finalizes from listing"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "How does routing work?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if !agent.synthCalled {
		t.Fatal("expected fallback synthesis after final from list_pages only is rejected")
	}
	if len(answer.Trace) < 2 || !strings.Contains(answer.Trace[1].Observation, "final rejected") {
		t.Fatalf("expected final rejection after list_pages only, got %+v", answer.Trace)
	}
}

func TestLLMWikiQuerySearchActionPrefersSearchStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "pg-oauth.md"), `---
type: "concept"
title: "PG OAuth Token Validation"
---

# PG OAuth Token Validation

Token validation calls the auth service.
`)
	searchStore := &fakeSearchEvidenceStore{
		results: []core.QueryResult{{
			Path:    "wiki/concepts/pg-oauth.md",
			Title:   "PG OAuth Token Validation",
			Snippet: "Token validation calls the auth service.",
			Score:   900,
			Kind:    "concept",
		}},
	}
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "search", Query: "token validation auth service", Limit: 3, Rationale: "candidate recall"},
			{Action: "final", Answer: "Token validation calls the auth service [wiki/concepts/pg-oauth.md].", Rationale: "auto-read wiki page evidence is enough"},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Question:    "How is token validation handled?",
		Limit:       5,
		Agent:       agent,
		SearchStore: searchStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searchStore.called != 1 || searchStore.projectID != "project-1" || searchStore.limit != 3 {
		t.Fatalf("search store call mismatch: %+v", searchStore)
	}
	if len(answer.Results) != 1 || answer.Results[0].Path != "wiki/concepts/pg-oauth.md" {
		t.Fatalf("expected PG search result, got %+v", answer.Results)
	}
	if !strings.Contains(answer.Trace[0].Observation, "wiki/concepts/pg-oauth.md") {
		t.Fatalf("trace=%+v", answer.Trace)
	}
	if !strings.Contains(answer.Trace[0].Observation, "auto-read 1 candidate document") {
		t.Fatalf("search should auto-read candidate page, trace=%+v", answer.Trace)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/pg-oauth.md") {
		t.Fatalf("final action should see auto-read PG candidate page: %+v", agent.inputs[1].Docs)
	}
}

func TestLLMWikiQuerySearchActionPrefersVectorSearchWhenEmbeddingAvailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "vector-oauth.md"), `---
type: "concept"
title: "Vector OAuth Token Validation"
---

# Vector OAuth Token Validation

Token validation uses semantic vector evidence.
`)
	searchStore := &fakeVectorSearchEvidenceStore{
		vectorResults: []core.QueryResult{{
			Path:    "wiki/concepts/vector-oauth.md",
			Title:   "Vector OAuth Token Validation",
			Snippet: "Semantic vector match for token validation.",
			Score:   997,
			Kind:    "concept",
		}},
	}
	embeddingProvider := &fakeEmbeddingProvider{embedding: []float32{0.1, 0.2, 0.3}}
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "search", Query: "token validation auth service", Limit: 3, Rationale: "semantic candidate recall"},
			{Action: "final", Answer: "Token validation uses semantic evidence [wiki/concepts/vector-oauth.md].", Rationale: "vector result is enough for this test"},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Question:          "How is token validation handled?",
		Limit:             5,
		Agent:             agent,
		SearchStore:       searchStore,
		EmbeddingProvider: embeddingProvider,
	})
	if err != nil {
		t.Fatal(err)
	}
	if embeddingProvider.called != 1 || !strings.Contains(embeddingProvider.text, "token validation auth service") {
		t.Fatalf("embedding provider not used correctly: %+v", embeddingProvider)
	}
	if searchStore.vectorCalled != 1 || searchStore.ftsCalled != 0 {
		t.Fatalf("expected vector search only, got vector=%d fts=%d", searchStore.vectorCalled, searchStore.ftsCalled)
	}
	if len(answer.Results) != 1 || answer.Results[0].Path != "wiki/concepts/vector-oauth.md" {
		t.Fatalf("expected vector result, got %+v", answer.Results)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/vector-oauth.md") {
		t.Fatalf("final action should see auto-read vector candidate page: %+v", agent.inputs[1].Docs)
	}
}

func TestLLMWikiQueryRejectsFinalThatCitesUnreadSearchResult(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	searchStore := &fakeSearchEvidenceStore{
		results: []core.QueryResult{{
			Path:    "wiki/concepts/missing-oauth.md",
			Title:   "Missing OAuth Token Validation",
			Snippet: "Token validation calls the auth service.",
			Score:   900,
			Kind:    "concept",
		}},
	}
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "search", Query: "token validation auth service", Limit: 3, Rationale: "candidate recall"},
			{Action: "final", Answer: "Token validation calls the auth service [wiki/concepts/missing-oauth.md].", Rationale: "incorrectly cites unread search result"},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Question:    "How is token validation handled?",
		Limit:       5,
		Agent:       agent,
		SearchStore: searchStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !agent.synthCalled {
		t.Fatal("expected fallback synthesis after rejecting unread search-result citation")
	}
	if !strings.Contains(answer.Trace[0].Observation, "skipped 1 unreadable candidate") {
		t.Fatalf("expected unreadable candidate trace, got %+v", answer.Trace)
	}
	if len(answer.Trace) < 2 || !strings.Contains(answer.Trace[1].Observation, "cited search candidate") {
		t.Fatalf("expected unread search-result citation rejection, got %+v", answer.Trace)
	}
	if len(answer.Citations) != 0 {
		t.Fatalf("unread search result must not become citation evidence: %+v", answer.Citations)
	}
	_, writeErr := WriteQueryAnswer(QueryWritebackOptions{
		ProjectPath: root,
		Title:       "Unread Search Result",
		Answer:      answer,
	})
	if writeErr == nil || !strings.Contains(writeErr.Error(), "non-navigation citation") {
		t.Fatalf("expected writeback rejection for unread search result, got %v", writeErr)
	}
}

func TestLLMWikiQueryRejectsFinalWithoutReadEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "Can we answer from snippets?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "final", Answer: "Snippet-only answer [wiki/concepts/missing.md].", Rationale: "incorrectly finalizes without evidence"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "Can we answer from snippets?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if !agent.synthCalled {
		t.Fatal("expected fallback synthesis after rejecting unsupported final action")
	}
	if answer.Answer != "synthesized fallback" {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if len(answer.Trace) == 0 || !strings.Contains(answer.Trace[0].Observation, "final rejected") {
		t.Fatalf("expected final rejection trace, got %+v", answer.Trace)
	}
}

func TestLLMWikiQueryActionLoopUsesCodeGraphEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "raw", "code-graphs", "repo", "graphify", "graph.json"), `{
  "nodes": [
    {"id":"auth.validate","kind":"function","label":"ValidateToken","source_file":"internal/auth/token.go"},
    {"id":"auth.service","kind":"class","label":"AuthService","source_file":"internal/auth/service.go"}
  ],
  "edges": [
    {"source":"auth.validate","target":"auth.service","relation":"calls","confidence":"EXTRACTED","weight":1}
  ]
}`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "What validates tokens?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "graph", Query: "ValidateToken AuthService calls", Limit: 5, Rationale: "code relationship question"},
			{Action: "final", Answer: "ValidateToken calls AuthService [raw/code-graphs/repo/graphify/graph.json#edge/auth.validate/auth.service].", Rationale: "graph evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "What validates tokens?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) != 2 {
		t.Fatalf("trace=%+v", answer.Trace)
	}
	if answer.Trace[0].Action.Action != "graph" || !strings.Contains(answer.Trace[0].Observation, "raw/code-graphs/repo/graphify/graph.json") {
		t.Fatalf("unexpected graph trace: %+v", answer.Trace[0])
	}
	if !queryDocsContainPrefix(agent.inputs[1].Docs, "raw/code-graphs/repo/graphify/graph.json#") {
		t.Fatalf("final action should see graph evidence docs: %+v", agent.inputs[1].Docs)
	}
	if len(answer.Citations) == 0 || !strings.HasPrefix(answer.Citations[0].Path, "raw/code-graphs/repo/graphify/graph.json#") {
		t.Fatalf("expected graph citation, got %+v", answer.Citations)
	}
	if !strings.Contains(answer.Answer, "ValidateToken calls AuthService") {
		t.Fatalf("answer=%q", answer.Answer)
	}
}

func TestLLMWikiQueryGraphActionPrefersGraphStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	graphStore := &fakeGraphEvidenceStore{
		evidence: []core.GraphEvidence{{
			Path:    "raw/code-graphs/repo/graphify/graph.json#edge/auth.validate/auth.service",
			Title:   "ValidateToken -> AuthService CALLS",
			Content: "Graph edge evidence from PG\nRelation: CALLS\nConfidence: EXTRACTED",
			Score:   120,
		}},
	}
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "What validates tokens?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "graph", Query: "ValidateToken AuthService calls", Limit: 5, Rationale: "code relationship question"},
			{Action: "final", Answer: "ValidateToken calls AuthService [raw/code-graphs/repo/graphify/graph.json#edge/auth.validate/auth.service].", Rationale: "PG graph evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Question:    "What validates tokens?",
		Limit:       5,
		Agent:       agent,
		GraphStore:  graphStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if graphStore.called != 1 || graphStore.projectID != "project-1" || graphStore.query != "ValidateToken AuthService calls" {
		t.Fatalf("graph store call mismatch: %+v", graphStore)
	}
	if !queryDocsContain(agent.inputs[1].Docs, graphStore.evidence[0].Path) {
		t.Fatalf("final action should see PG graph evidence: %+v", agent.inputs[1].Docs)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != graphStore.evidence[0].Path {
		t.Fatalf("expected PG graph citation, got %+v", answer.Citations)
	}
	if !strings.Contains(answer.Trace[0].Observation, "graph returned 1 evidence") {
		t.Fatalf("trace=%+v", answer.Trace)
	}
}

func TestWriteQueryAnswerCreatesSynthesisPage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "oauth.md"), `---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls the auth service.
`)

	answer, err := QueryLLMWikiWithAgent(root, "token auth", 3, fakeQueryAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			Searches:       []core.QuerySearch{{Text: "token auth", Weight: 6, Rationale: "user wording"}},
			CandidateLimit: 3,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	answer.Trace = []core.QueryTraceStep{{
		Step: 1,
		Action: core.QueryAction{
			Action: "read",
			Path:   "wiki/sources/oauth.md",
		},
		Observation: "read wiki/sources/oauth.md",
	}}
	writeback, err := WriteQueryAnswer(QueryWritebackOptions{
		ProjectPath: root,
		Title:       "OAuth Token Auth Synthesis",
		Answer:      answer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeback.Path != "wiki/syntheses/oauth-token-auth-synthesis.md" {
		t.Fatalf("path=%s", writeback.Path)
	}
	page, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(writeback.Path)))
	if err != nil {
		t.Fatal(err)
	}
	content := string(page)
	for _, want := range []string{
		`type: "synthesis"`,
		`question: "token auth"`,
		"## Query Plan",
		"## Query Trace",
		`"action": "read"`,
		"wiki/sources/oauth.md",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("synthesis page missing %q:\n%s", want, content)
		}
	}
	index, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "wiki/syntheses/oauth-token-auth-synthesis.md") {
		t.Fatalf("index missing synthesis entry:\n%s", index)
	}
	logData, err := os.ReadFile(filepath.Join(root, "wiki", "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "query | OAuth Token Auth Synthesis") {
		t.Fatalf("log missing query writeback:\n%s", logData)
	}
	overview, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overview), "Recent Syntheses") || !strings.Contains(string(overview), writeback.Path) {
		t.Fatalf("overview missing synthesis entry:\n%s", overview)
	}
}

func TestWriteQueryAnswerRejectsIneligibleAnswers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		answer *core.QueryAnswer
		want   string
	}{
		{
			name: "can_write_back_false",
			answer: &core.QueryAnswer{
				Question: "offline",
				Plan: core.QueryPlan{
					Intent:       "offline_fallback_query",
					AnswerMode:   "offline_reading_fallback",
					CanWriteBack: false,
				},
				Answer: "offline answer",
				Citations: []core.QueryCitation{{
					Path:  "wiki/concepts/oauth.md",
					Title: "OAuth",
					Kind:  "concept",
				}},
			},
			want: "not eligible",
		},
		{
			name: "offline_mode_even_if_flagged",
			answer: &core.QueryAnswer{
				Question: "offline",
				Plan: core.QueryPlan{
					Intent:       "offline_fallback_query",
					AnswerMode:   "offline_reading_fallback",
					CanWriteBack: true,
				},
				Answer: "offline answer",
				Citations: []core.QueryCitation{{
					Path:  "wiki/concepts/oauth.md",
					Title: "OAuth",
					Kind:  "concept",
				}},
			},
			want: "offline query answers",
		},
		{
			name: "navigation_only_citation",
			answer: &core.QueryAnswer{
				Question: "navigation",
				Plan: core.QueryPlan{
					Intent:       "answer_from_persistent_wiki",
					AnswerMode:   "llm_synthesis",
					CanWriteBack: true,
				},
				Answer: "navigation-only answer",
				Citations: []core.QueryCitation{{
					Path:  "wiki/index.md",
					Title: "Index",
					Kind:  "wiki-navigation",
				}},
			},
			want: "non-navigation citation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WriteQueryAnswer(QueryWritebackOptions{
				ProjectPath: root,
				Title:       "Rejected",
				Answer:      tc.answer,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func mustWrite(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeQueryAgent struct {
	plan core.QueryPlan
}

func (f fakeQueryAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (fakeQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	if len(input.Results) == 0 {
		return "no results", nil
	}
	return "synthesized from " + input.Results[0].Path, nil
}

type capturingQueryAgent struct {
	plan core.QueryPlan
	docs []QueryReadDocument
}

func (f *capturingQueryAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (f *capturingQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	f.docs = input.Docs
	return "synthesized from planned docs", nil
}

type scriptedActionAgent struct {
	plan        core.QueryPlan
	actions     []core.QueryAction
	inputs      []QueryActionInput
	synthCalled bool
}

type fakeGraphEvidenceStore struct {
	evidence  []core.GraphEvidence
	called    int
	projectID string
	query     string
	limit     int
}

type fakeSearchEvidenceStore struct {
	results   []core.QueryResult
	called    int
	projectID string
	plan      core.QueryPlan
	limit     int
}

type fakeQueryLogStore struct {
	called      int
	id          string
	projectID   string
	query       string
	mode        string
	resultCount int
}

type fakeVectorSearchEvidenceStore struct {
	vectorResults []core.QueryResult
	ftsResults    []core.QueryResult
	vectorCalled  int
	ftsCalled     int
	embedding     []float32
}

func (s *fakeVectorSearchEvidenceStore) SearchWikiEvidence(_ context.Context, _ string, _ core.QueryPlan, _ int) ([]core.QueryResult, error) {
	s.ftsCalled++
	return s.ftsResults, nil
}

func (s *fakeVectorSearchEvidenceStore) SearchWikiEvidenceVector(_ context.Context, _ string, _ core.QueryPlan, embedding []float32, _ int) ([]core.QueryResult, error) {
	s.vectorCalled++
	s.embedding = embedding
	return s.vectorResults, nil
}

type fakeEmbeddingProvider struct {
	embedding []float32
	called    int
	text      string
	texts     []string
}

func (p *fakeEmbeddingProvider) EmbedText(_ context.Context, text string) ([]float32, error) {
	p.called++
	p.text = text
	p.texts = append(p.texts, text)
	return p.embedding, nil
}

func (s *fakeSearchEvidenceStore) SearchWikiEvidence(_ context.Context, projectID string, plan core.QueryPlan, limit int) ([]core.QueryResult, error) {
	s.called++
	s.projectID = projectID
	s.plan = plan
	s.limit = limit
	return s.results, nil
}

func (s *fakeQueryLogStore) InsertQueryLog(_ context.Context, id, projectID, query, mode string, resultCount int) error {
	s.called++
	s.id = id
	s.projectID = projectID
	s.query = query
	s.mode = mode
	s.resultCount = resultCount
	return nil
}

func (s *fakeGraphEvidenceStore) SearchGraphEvidence(_ context.Context, projectID, q string, limit int) ([]core.GraphEvidence, error) {
	s.called++
	s.projectID = projectID
	s.query = q
	s.limit = limit
	return s.evidence, nil
}

func (f *scriptedActionAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (f *scriptedActionAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
	f.inputs = append(f.inputs, input)
	if input.Step <= 0 || input.Step > len(f.actions) {
		return core.QueryAction{Action: "final", Answer: "no more scripted actions"}, nil
	}
	return f.actions[input.Step-1], nil
}

func (f *scriptedActionAgent) SynthesizeQuery(QuerySynthesisInput) (string, error) {
	f.synthCalled = true
	return "synthesized fallback", nil
}

func queryDocsContain(docs []QueryReadDocument, path string) bool {
	for _, doc := range docs {
		if doc.Path == path {
			return true
		}
	}
	return false
}

func queryDocsContainPrefix(docs []QueryReadDocument, prefix string) bool {
	for _, doc := range docs {
		if strings.HasPrefix(doc.Path, prefix) {
			return true
		}
	}
	return false
}
