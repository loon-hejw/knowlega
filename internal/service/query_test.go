package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestUnifiedQueryTurnSkipsRouterAndPlanner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	canWriteBack := false
	agent := &unifiedTurnTestAgent{decisions: []core.QueryTurnDecision{{
		Intent:           QueryIntentDirectChat,
		ResolvedQuestion: "你好",
		ReasoningMode:    "synthesis",
		CanWriteBack:     &canWriteBack,
		Action:           core.QueryAction{Action: "final", Answer: "你好，我在。"},
	}}}
	var events []QueryProgressEvent
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "你好",
		Agent:       agent,
		Progress: func(event QueryProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer != "你好，我在。" || answer.Plan.Intent != QueryIntentDirectChat {
		t.Fatalf("answer=%+v", answer)
	}
	if agent.turnCalls != 1 || agent.planCalls != 0 || agent.routeCalls != 0 || agent.actionCalls != 0 {
		t.Fatalf("unexpected calls: turn=%d plan=%d route=%d legacy_action=%d", agent.turnCalls, agent.planCalls, agent.routeCalls, agent.actionCalls)
	}
	if !queryProgressContains(events, "context_started") || !queryProgressContains(events, "strategy_ready") {
		t.Fatalf("unified progress events missing: %+v", events)
	}
	if queryProgressContains(events, "routing_started") || queryProgressContains(events, "planning_started") {
		t.Fatalf("legacy router/planner events must not be emitted: %+v", events)
	}
	if len(agent.inputs) != 1 || agent.inputs[0].Index == "" || agent.inputs[0].Purpose == "" {
		t.Fatalf("first turn did not receive project guidance: %+v", agent.inputs)
	}
}

func TestUnifiedQueryTurnKeepsNumberedRequirementsAndReadsEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "zhenyuanzi.md"), `---
type: entity
title: 镇元子
---
# 镇元子
镇元子与孙悟空相见，后来结为兄弟。
`)
	canWriteBack := false
	question := "1. 有结义的情节\n2. 见过孙悟空\n3. 跟孙悟空不算敌对关系"
	agent := &unifiedTurnTestAgent{decisions: []core.QueryTurnDecision{
		{
			Intent:           QueryIntentWikiQuery,
			ResolvedQuestion: "谁同时满足三个条件？",
			ReasoningMode:    "constraint_satisfaction",
			Requirements: []core.QueryRequirement{
				{ID: "1", Text: "有结义的情节", Kind: "positive"},
				{ID: "2", Text: "见过孙悟空", Kind: "positive"},
				{ID: "3", Text: "跟孙悟空不算敌对关系", Kind: "positive"},
			},
			RequireAll:   true,
			CanWriteBack: &canWriteBack,
			Action:       core.QueryAction{Action: "read", Path: "wiki/entities/zhenyuanzi.md"},
		},
		{Action: core.QueryAction{
			Action:    "final",
			Candidate: "镇元子",
			Answer:    "镇元子满足这三个条件 [wiki/entities/zhenyuanzi.md]。",
			Checks: []core.QueryEvidenceCheck{
				{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/zhenyuanzi.md"}},
				{RequirementID: "2", Status: "supported", EvidencePaths: []string{"wiki/entities/zhenyuanzi.md"}},
				{RequirementID: "3", Status: "supported", EvidencePaths: []string{"wiki/entities/zhenyuanzi.md"}},
			},
		}},
	}}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    question,
		Agent:       agent,
		Runtime: QueryRuntimeOptions{
			InitialActionBudget: 4,
			MaxActionBudget:     8,
			VerificationPasses:  -1,
			StagnationRounds:    2,
			TotalTimeout:        time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Status != "complete" || answer.Candidate != "镇元子" || len(answer.Plan.Requirements) != 3 {
		t.Fatalf("answer=%+v", answer)
	}
	if agent.planCalls != 0 || agent.routeCalls != 0 || agent.turnCalls != 2 {
		t.Fatalf("unexpected calls: %+v", agent)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/entities/zhenyuanzi.md" {
		t.Fatalf("citations=%+v", answer.Citations)
	}
}

func TestUnifiedMissingEvidenceMustAttemptEvidenceTool(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	canWriteBack := false
	agent := &unifiedTurnTestAgent{decisions: []core.QueryTurnDecision{
		{
			Intent:       QueryIntentMissingEvidence,
			CanWriteBack: &canWriteBack,
			Action:       core.QueryAction{Action: "final", Answer: "知识库没有相关证据。"},
		},
		{Action: core.QueryAction{Action: "search", Query: "不存在的主题"}},
		{Action: core.QueryAction{Action: "final", Answer: "检索后仍未找到相关证据。"}},
	}}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "知识库里有不存在的主题吗？",
		Agent:       agent,
		Runtime: QueryRuntimeOptions{
			InitialActionBudget: 4,
			MaxActionBudget:     8,
			VerificationPasses:  -1,
			StagnationRounds:    2,
			TotalTimeout:        time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer != "检索后仍未找到相关证据。" || agent.turnCalls != 3 {
		t.Fatalf("answer=%+v turns=%d", answer, agent.turnCalls)
	}
	if !hasNoEvidenceFinalRejection(answer.Trace) {
		t.Fatalf("first unsupported final should have been rejected: %+v", answer.Trace)
	}
}

func TestUnifiedConstraintQueryKeepsModelHypothesesAsReasoningState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "唐太宗.md"), `---
type: entity
title: 唐太宗
---
# 唐太宗
唐太宗与玄奘结拜，见过孙悟空，也亲见阎罗王。
`)
	canWriteBack := false
	checks := []core.QueryEvidenceCheck{
		{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}},
		{RequirementID: "2", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}},
		{RequirementID: "7", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}},
	}
	agent := &unifiedTurnTestAgent{decisions: []core.QueryTurnDecision{
		{
			Intent:           QueryIntentWikiQuery,
			ResolvedQuestion: "谁满足全部条件？",
			ReasoningMode:    "constraint_satisfaction",
			Requirements: []core.QueryRequirement{
				{ID: "1", Text: "有结义情节", Kind: "positive"},
				{ID: "2", Text: "见过孙悟空", Kind: "positive"},
				{ID: "7", Text: "见过阎罗王", Kind: "positive"},
			},
			Hypotheses: []core.QueryHypothesis{
				{Candidate: "唐太宗", Rationale: "跨越结义、地府与回朝接见", SuggestedReads: []string{"wiki/entities/唐太宗.md"}},
				{Candidate: "牛魔王", Rationale: "有结义但其余条件待查"},
			},
			RequireAll:   true,
			CanWriteBack: &canWriteBack,
			Action:       core.QueryAction{Action: "read", Path: "wiki/entities/唐太宗.md"},
		},
		{Action: core.QueryAction{
			Action: "final", Candidate: "唐太宗",
			Answer: "唐太宗满足全部条件 [wiki/entities/唐太宗.md]。", Checks: checks,
		}},
	}}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "1.有结义情节\n2.见过孙悟空\n7.见过阎罗王",
		Agent:       agent,
		Runtime: QueryRuntimeOptions{
			InitialActionBudget: 4, MaxActionBudget: 8, VerificationPasses: -1,
			StagnationRounds: 2, TotalTimeout: time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Status != "complete" || answer.Candidate != "唐太宗" {
		t.Fatalf("answer=%+v", answer)
	}
	if len(answer.Plan.Hypotheses) != 2 || answer.Plan.Hypotheses[0].Candidate != "唐太宗" {
		t.Fatalf("model hypotheses were not preserved: %+v", answer.Plan.Hypotheses)
	}
	if len(agent.inputs) < 2 || len(agent.inputs[1].Plan.Hypotheses) != 2 || !queryDocsContain(agent.inputs[1].Docs, "wiki/entities/唐太宗.md") {
		t.Fatalf("continuing turn did not receive hypotheses and read evidence: %+v", agent.inputs)
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

func TestDeepQueryUsesSameAgentStopReviews(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: concept
title: OAuth
---
# OAuth
OAuth uses signed tokens.
`)
	base := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "How does OAuth work?",
			Intent:         "answer_from_persistent_wiki",
			ReasoningMode:  "constraint_satisfaction",
			Requirements:   []core.QueryRequirement{{ID: "1", Text: "uses signed tokens", Kind: "positive"}},
			RequireAll:     true,
			ReadFirst:      []string{"wiki/concepts/oauth.md"},
			CandidateLimit: 5,
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "final", Candidate: "OAuth", Answer: "OAuth uses signed tokens [wiki/concepts/oauth.md].", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/concepts/oauth.md"}}}},
			{Action: "final", Candidate: "OAuth", Answer: "OAuth uses signed tokens [wiki/concepts/oauth.md].", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/concepts/oauth.md"}}}},
			{Action: "final", Candidate: "OAuth", Answer: "OAuth uses signed tokens [wiki/concepts/oauth.md].", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/concepts/oauth.md"}}}},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "How does OAuth work?",
		Agent:       base,
		Runtime: QueryRuntimeOptions{
			InitialActionBudget: 4,
			MaxActionBudget:     8,
			VerificationPasses:  2,
			StagnationRounds:    2,
			TotalTimeout:        time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer != "OAuth uses signed tokens [wiki/concepts/oauth.md]." {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if len(answer.Verification) != 2 || !answer.Verification[0].Accepted || !answer.Verification[1].Accepted {
		t.Fatalf("verification=%+v", answer.Verification)
	}
	if answer.Plan.CanWriteBack {
		t.Fatalf("one-off constraint answer should not become writeback eligible: %+v", answer.Plan)
	}
}

func TestQueryActionLoopRecoversFromMissingRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "king.md"), `---
type: entity
title: King
---
# King
The king returned safely.
`)
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{Intent: "answer_from_persistent_wiki", CandidateLimit: 5},
		actions: []core.QueryAction{
			{Action: "read", Path: "wiki/entities/missing.md"},
			{Action: "read", Path: "wiki/entities/king.md"},
			{Action: "final", Answer: "The king returned safely [wiki/entities/king.md]."},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "What happened?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer.Answer, "returned safely") {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if len(answer.Trace) != 3 || !strings.Contains(answer.Trace[0].Observation, "read failed") {
		t.Fatalf("trace=%+v", answer.Trace)
	}
}

func TestAcceptFinalQueryActionNormalizesRawEvidencePath(t *testing.T) {
	docs := []QueryReadDocument{{
		Path:    "raw/sources/chapter-071/original/chapter-071.txt",
		Title:   "Chapter 71",
		Kind:    "raw-source",
		Content: "source evidence",
	}}
	action := core.QueryAction{
		Action: "final",
		Answer: "The claim is supported by [chapter 71](wiki/raw/sources/chapter-071/original/chapter-071.txt).",
	}
	var trace []core.QueryTraceStep
	answer, _, accepted := acceptFinalQueryAction(1, "what happened?", core.QueryPlan{}, action, nil, docs, &trace)
	if !accepted {
		t.Fatalf("final action was rejected: %+v", trace)
	}
	if strings.Contains(answer, "wiki/raw/") {
		t.Fatalf("answer still contains noncanonical raw path: %q", answer)
	}
	if !strings.Contains(answer, "(raw/sources/chapter-071/original/chapter-071.txt)") {
		t.Fatalf("answer does not contain canonical raw path: %q", answer)
	}
	if len(trace) != 1 || strings.Contains(trace[0].Action.Answer, "wiki/raw/") {
		t.Fatalf("trace did not retain the normalized action: %+v", trace)
	}
}

func TestEnrichQueryPlanExtractsNumberedConstraintRequirements(t *testing.T) {
	question := "1.有结义的情节\n2.见过孙悟空\n3.不曾到过花果山"
	plan := enrichQueryPlan(question, core.QueryPlan{CanWriteBack: true})
	if plan.ReasoningMode != "constraint_satisfaction" || !plan.RequireAll {
		t.Fatalf("plan was not switched to constraint mode: %+v", plan)
	}
	if plan.CanWriteBack {
		t.Fatal("one-off constraint question must not be writeback eligible")
	}
	if len(plan.Requirements) != 3 || plan.Requirements[2].Kind != "" {
		t.Fatalf("requirements=%+v", plan.Requirements)
	}
}

func TestFindNamedEntityCooccurrenceSourcesFindsSharedChapter(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "wiki", "entities", "唐太宗.md"), `---
title: 唐太宗
type: entity
---

# 唐太宗
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "孙悟空.md"), `---
title: 孙悟空
type: entity
---

# 孙悟空
`)
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-100.md"), `---
title: 第一百回
type: source-summary
---

# 第一百回

[[唐太宗]]接见取经众人，玄奘向他引见了[[孙悟空]]。
`)
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-010.md"), `---
title: 第十回
type: source-summary
---

# 第十回

[[唐太宗]]还魂。
`)

	results, docs, err := findNamedEntityCooccurrenceSources(root, "唐太宗 孙悟空 直接见面 章回", 5, 12000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "wiki/sources/chapter-100.md" {
		t.Fatalf("shared source chapter was not found: %+v", results)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Content, "玄奘向他引见了[[孙悟空]]") {
		t.Fatalf("shared source chapter was not read as evidence: %+v", docs)
	}
}

func TestPrioritizeQueryActionDocumentsKeepsCandidateProvenance(t *testing.T) {
	entity := `---
title: 唐太宗
type: entity
sources:
  - raw/sources/chapter-012/original/chapter-012.txt
---

# 唐太宗
`
	docs := []QueryReadDocument{
		{Path: "wiki/entities/唐太宗.md", Title: "唐太宗", Content: entity},
		{Path: "raw/sources/chapter-012/original/chapter-012.txt", Title: "第十二回", Content: "太宗坚持奉饯，三藏不敢不受，复谢恩饮尽。"},
		{Path: "wiki/entities/后来候选.md", Title: "后来候选", Content: "其他证据"},
	}
	ordered := prioritizeQueryActionDocuments(docs, nil, []core.QueryTraceStep{{
		Step: 1,
		Action: core.QueryAction{
			Action:    "final",
			Candidate: "唐太宗",
			Checks:    []core.QueryEvidenceCheck{{RequirementID: "5", Status: "unknown"}},
		},
	}}, nil)
	if len(ordered) != 3 || ordered[len(ordered)-2].Path != "wiki/entities/唐太宗.md" || ordered[len(ordered)-1].Path != "raw/sources/chapter-012/original/chapter-012.txt" {
		t.Fatalf("candidate and provenance were not prioritized together: %+v", ordered)
	}
}

func TestBuildCandidateEvidencePacksCombinesProvenanceAndSharedScenes(t *testing.T) {
	root := t.TempDir()
	rawRel := "raw/sources/chapter-012/original/chapter-012.txt"
	mustWrite(t, filepath.Join(root, "wiki", "entities", "唐太宗.md"), "---\ntitle: 唐太宗\ntype: entity\nsources:\n  - "+rawRel+"\n---\n\n# 唐太宗\n")
	mustWrite(t, filepath.Join(root, "wiki", "entities", "孙悟空.md"), "---\ntitle: 孙悟空\ntype: entity\n---\n\n# 孙悟空\n")
	mustWrite(t, filepath.Join(root, filepath.FromSlash(rawRel)), "太宗坚持奉饯，三藏不敢不受，复谢恩饮尽。")
	mustWrite(t, filepath.Join(root, "wiki", "sources", "chapter-100.md"), "---\ntitle: 第一百回\ntype: source-summary\n---\n\n[[唐太宗]]接见玄奘，并由玄奘引见[[孙悟空]]。\n")

	packs, err := buildCandidateEvidencePacks(root, []core.QueryHypothesis{{Candidate: "唐太宗"}}, []core.QueryRequirement{{ID: "2", Text: "见过孙悟空"}}, 6, 12000)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 || !queryDocsContain(packs[0].Docs, rawRel) || !queryDocsContain(packs[0].Docs, "wiki/sources/chapter-100.md") {
		t.Fatalf("candidate evidence pack is incomplete: %+v", packs)
	}
}

func TestNormalizeCandidateAuditsRanksCoverageAndDropsUnreadPaths(t *testing.T) {
	packs := []QueryCandidateEvidencePack{
		{Candidate: "甲", Docs: []QueryReadDocument{{Path: "wiki/entities/a.md"}}},
		{Candidate: "乙", Docs: []QueryReadDocument{{Path: "wiki/entities/b.md"}}},
	}
	audits := []core.QueryHypothesis{
		{Candidate: "甲", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/a.md"}}}},
		{Candidate: "乙", Checks: []core.QueryEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/b.md"}},
			{RequirementID: "2", Status: "supported", EvidencePaths: []string{"wiki/entities/b.md", "wiki/entities/unread.md"}},
		}},
	}
	normalized := normalizeCandidateAudits(audits, packs, []core.QueryRequirement{{ID: "1"}, {ID: "2"}})
	if len(normalized) != 2 || normalized[0].Candidate != "乙" || normalized[0].Coverage != 2 {
		t.Fatalf("candidate audits were not ranked by evidence coverage: %+v", normalized)
	}
	if got := normalized[0].Checks[1].EvidencePaths; len(got) != 1 || got[0] != "wiki/entities/b.md" {
		t.Fatalf("unread audit evidence path was retained: %+v", got)
	}
}

func TestReadHypothesisDocumentsResolvesCandidateTitlesAndSkipsMissingSuggestions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "entities"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ntitle: 唐太宗\ntype: entity\n---\n\n# 唐太宗\n\n候选证据。\n"
	if err := os.WriteFile(filepath.Join(root, "wiki", "entities", "唐太宗.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	docs, err := readHypothesisDocuments(root, []core.QueryHypothesis{{
		Candidate:      "唐太宗",
		SuggestedReads: []string{"wiki/entities/不存在.md"},
	}}, 4, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "wiki/entities/唐太宗.md" {
		t.Fatalf("hypothesis candidate was not resolved and read: %+v", docs)
	}
}

func TestAcceptFinalQueryActionRequiresEveryConstraintForSameCandidate(t *testing.T) {
	plan := core.QueryPlan{
		ReasoningMode: "constraint_satisfaction",
		RequireAll:    true,
		Requirements: []core.QueryRequirement{
			{ID: "1", Text: "有结义", Kind: "positive"},
			{ID: "2", Text: "见过阎罗王", Kind: "positive"},
			{ID: "3", Text: "不曾到过花果山", Kind: "negative"},
		},
	}
	docs := []QueryReadDocument{
		{Path: "wiki/entities/唐太宗.md", Kind: "wiki-page", Content: "唐太宗证据"},
		{Path: "raw/sources/chapter-010/original/chapter-010.txt", Kind: "raw-source", Content: "阎王证据"},
	}
	action := core.QueryAction{
		Action:    "final",
		Candidate: "唐太宗",
		Answer:    "答案是唐太宗。",
		Checks: []core.QueryEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}},
			{RequirementID: "2", Status: "supported", EvidencePaths: []string{"raw/sources/chapter-010/original/chapter-010.txt"}},
			{RequirementID: "3", Status: "unknown", EvidencePaths: []string{"wiki/entities/唐太宗.md"}},
		},
	}
	var trace []core.QueryTraceStep
	trace = []core.QueryTraceStep{{Step: 1, Action: core.QueryAction{Action: "search", Query: "唐太宗 花果山"}, Observation: "searched corpus"}}
	if _, _, accepted := acceptFinalQueryAction(2, "谁？", plan, action, nil, docs, &trace); accepted {
		t.Fatalf("unknown constraint was accepted: %+v", trace)
	}
	action.Checks[2].Status = "not_found_in_corpus"
	trace = []core.QueryTraceStep{{Step: 1, Action: core.QueryAction{Action: "search", Query: "唐太宗 花果山"}, Observation: "searched corpus"}}
	answer, _, accepted := acceptFinalQueryAction(2, "谁？", plan, action, nil, docs, &trace)
	if !accepted || answer != "答案是唐太宗。" {
		t.Fatalf("closed constraint ledger was rejected: answer=%q trace=%+v", answer, trace)
	}
	action.Checks[0].EvidencePaths = []string{"wiki/Overview.md"}
	trace = []core.QueryTraceStep{{Step: 1, Action: core.QueryAction{Action: "search", Query: "唐太宗 花果山"}, Observation: "searched corpus"}}
	if _, _, accepted := acceptFinalQueryAction(2, "谁？", plan, action, nil, docs, &trace); accepted || !strings.Contains(trace[len(trace)-1].Observation, "navigation-only evidence") {
		t.Fatalf("aggregate evidence was not rejected clearly: %+v", trace)
	}
}

func TestRecallConstraintCandidatesFusesIndependentPositiveRequirementsByEntity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "ruler.md"), `---
type: entity
title: The Ruler
aliases: [Sovereign]
sources:
  - raw/sources/a.txt
  - raw/sources/b.txt
  - raw/sources/c.txt
---
# The Ruler
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "warrior.md"), `---
type: entity
title: The Warrior
sources:
  - raw/sources/a.txt
---
# The Warrior
`)
	store := requirementSearchStore{byQuestion: map[string][]core.QueryResult{
		"formed a sworn bond":      {{Path: "raw/sources/a.txt", Title: "A", Kind: "raw-source", Score: 9, Snippet: "The Ruler and The Warrior formed a sworn bond."}},
		"met the traveler":         {{Path: "raw/sources/b.txt", Title: "B", Kind: "raw-source", Score: 9, Snippet: "The Ruler met the traveler."}},
		"visited the underworld":   {{Path: "raw/sources/c.txt", Title: "C", Kind: "raw-source", Score: 9, Snippet: "The Ruler visited the underworld."}},
		"never visited the island": {{Path: "wiki/entities/warrior.md", Title: "The Warrior", Kind: "entity", Score: 99}},
	}}
	results, err := recallConstraintCandidates(context.Background(), root, "", core.QueryPlan{Requirements: []core.QueryRequirement{
		{ID: "1", Text: "formed a sworn bond", Kind: "positive"},
		{ID: "2", Text: "met the traveler", Kind: "positive"},
		{ID: "3", Text: "visited the underworld", Kind: "positive"},
		{ID: "4", Text: "never visited the island", Kind: "negative"},
	}}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ruler *core.QueryResult
	for index := range results {
		if results[index].Path == "wiki/entities/ruler.md" {
			ruler = &results[index]
			break
		}
	}
	if ruler == nil || !strings.Contains(ruler.Snippet, "recall-coverage=3") {
		t.Fatalf("results=%+v", results)
	}
	if strings.Contains(ruler.Snippet, "ids=1,2,3,4") {
		t.Fatalf("negative requirement incorrectly boosted recall: %+v", ruler)
	}
}

func TestMergeRecallAndModelHypothesesDoesNotDropTailRecallCandidate(t *testing.T) {
	model := []core.QueryHypothesis{
		{Candidate: "model-a"}, {Candidate: "model-b"}, {Candidate: "model-c"}, {Candidate: "model-d"},
	}
	recalled := make([]core.QueryResult, 0, 24)
	for index := 0; index < 23; index++ {
		recalled = append(recalled, core.QueryResult{Path: fmt.Sprintf("wiki/entities/candidate-%02d.md", index), Title: fmt.Sprintf("candidate-%02d", index), Kind: "entity"})
	}
	recalled = append(recalled, core.QueryResult{Path: "wiki/entities/decisive-tail.md", Title: "decisive-tail", Kind: "entity"})
	merged := mergeRecallAndModelHypotheses(recalled, model, 28)
	if len(merged) != 28 || merged[len(merged)-1].Candidate != "decisive-tail" {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestSplitCandidateEvidencePacksUsesBoundedBatches(t *testing.T) {
	packs := []QueryCandidateEvidencePack{
		{Candidate: "A"},
		{Candidate: "B"},
		{Candidate: "C"},
		{Candidate: "D"},
		{Candidate: "E"},
	}
	batches := splitCandidateEvidencePacks(packs, 2)
	if len(batches) != 3 || len(batches[0]) != 2 || len(batches[1]) != 2 || len(batches[2]) != 1 {
		t.Fatalf("unexpected batches: %+v", batches)
	}
	if batches[0][0].Candidate != "A" || batches[1][0].Candidate != "C" || batches[2][0].Candidate != "E" {
		t.Fatalf("candidate order changed: %+v", batches)
	}
}

func TestConstraintLoopFinishIncompletePreservesBestCandidateLedger(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "candidate.md"), "---\ntype: entity\ntitle: Candidate\n---\n# Candidate\nCandidate satisfies the first condition.\n")
	plan := core.QueryPlan{ReasoningMode: "constraint_satisfaction", RequireAll: true, Requirements: []core.QueryRequirement{
		{ID: "1", Text: "first condition", Kind: "positive"},
		{ID: "2", Text: "missing condition", Kind: "positive"},
	}}
	agent := &scriptedActionAgent{actions: []core.QueryAction{
		{Action: "assess_candidate", Candidate: "Candidate", Checks: []core.QueryEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/candidate.md"}},
			{RequirementID: "2", Status: "unknown"},
		}},
		{Action: "finish_incomplete", Candidate: "Candidate", Rationale: "the exact second condition is absent"},
	}}
	result, err := runQueryActionLoop(context.Background(), root, "", "who", "", plan, nil, []QueryReadDocument{{
		Path: "wiki/entities/candidate.md", Title: "Candidate", Kind: "wiki-page", Content: "Candidate satisfies the first condition.",
	}}, QueryPlanningInput{}, agent, nil, nil, nil, QueryRuntimeOptions{MaxSteps: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "incomplete" || result.FinalAction.Candidate != "Candidate" || len(result.FinalAction.Checks) != 2 || !strings.Contains(result.Answer, "最佳候选") {
		t.Fatalf("result=%+v", result)
	}
}

func TestScopeNegativeRequirementAnswerAddsCorpusBoundary(t *testing.T) {
	plan := core.QueryPlan{Requirements: []core.QueryRequirement{{ID: "9", Text: "不曾到过花果山", Kind: "negative"}}}
	action := core.QueryAction{Answer: "答案是唐太宗。", Checks: []core.QueryEvidenceCheck{{RequirementID: "9", Status: "not_found_in_corpus"}}}
	answer := scopeNegativeRequirementAnswer(plan, action)
	if !strings.Contains(answer, "仅限当前知识库语料与本候选专项检索") || !strings.Contains(answer, "并非对语料之外事实的绝对证明") {
		t.Fatalf("negative evidence scope was not appended: %q", answer)
	}
}

func TestQueryTraceContainsCandidateSearchSurvivesLongTrace(t *testing.T) {
	trace := []core.QueryTraceStep{{Action: core.QueryAction{Action: "search", Query: "唐太宗 花果山"}}}
	for i := 0; i < 30; i++ {
		trace = append(trace, core.QueryTraceStep{Action: core.QueryAction{Action: "read", Path: "wiki/entities/other.md"}})
	}
	if !queryTraceContainsCandidateSearch(trace, "唐太宗") {
		t.Fatal("candidate-specific search was lost behind recent trace budget")
	}
}

func TestUnresolvedQueryRequirementsUsesCanonicalChecks(t *testing.T) {
	plan := core.QueryPlan{Requirements: []core.QueryRequirement{
		{ID: "1", Text: "有结义的情节", Kind: "positive"},
		{ID: "7", Text: "见过阎罗王", Kind: "positive"},
		{ID: "9", Text: "不曾到过花果山", Kind: "negative"},
	}}
	checks := []core.QueryEvidenceCheck{
		{RequirementID: "1", Status: "supported"},
		{RequirementID: "7", Status: "not_found_in_corpus"},
		{RequirementID: "9", Status: "not_found_in_corpus"},
	}
	reason := unresolvedQueryRequirements(plan, checks)
	if !strings.Contains(reason, "7. 见过阎罗王") {
		t.Fatalf("positive corpus absence must remain unresolved: %q", reason)
	}
	if strings.Contains(reason, "1. 有结义") || strings.Contains(reason, "9. 不曾到过") {
		t.Fatalf("closed requirements leaked into unresolved reason: %q", reason)
	}
}

func TestBuildCandidateAssessmentDistinguishesPositiveAndNegativeAbsence(t *testing.T) {
	plan := core.QueryPlan{Requirements: []core.QueryRequirement{
		{ID: "7", Text: "见过阎罗王", Kind: "positive"},
		{ID: "9", Text: "不曾到过花果山", Kind: "negative"},
	}}
	docs := []QueryReadDocument{{Path: "wiki/entities/镇元子.md", Title: "镇元子", Kind: "wiki-page"}}
	action := core.QueryAction{Candidate: "镇元子", Checks: []core.QueryEvidenceCheck{
		{RequirementID: "7", Status: "not_found_in_corpus", EvidencePaths: []string{"wiki/entities/镇元子.md"}},
		{RequirementID: "9", Status: "not_found_in_corpus", EvidencePaths: []string{"wiki/entities/镇元子.md"}},
	}}
	trace := []core.QueryTraceStep{{Action: core.QueryAction{Action: "search", Query: "镇元子 花果山"}}}
	assessment := buildCandidateAssessment(plan, action, docs, trace, 2)
	if assessment.Disposition != "partial" || !slices.Contains(assessment.UnresolvedRequirementIDs, "7") || slices.Contains(assessment.UnresolvedRequirementIDs, "9") {
		t.Fatalf("assessment=%+v", assessment)
	}
}

func TestConstraintLoopSwitchesFromPartialCandidateToCompleteCandidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "镇元子.md"), "---\ntype: entity\ntitle: 镇元子\n---\n# 镇元子\n镇元子与孙悟空结为兄弟。\n")
	mustWrite(t, filepath.Join(root, "wiki", "entities", "唐太宗.md"), "---\ntype: entity\ntitle: 唐太宗\n---\n# 唐太宗\n唐太宗与玄奘结拜，并魂游地府见十代冥王。\n")
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Intent: "answer_from_persistent_wiki", ReasoningMode: "constraint_satisfaction", RequireAll: true,
			Requirements: []core.QueryRequirement{{ID: "1", Text: "有结义情节", Kind: "positive"}, {ID: "7", Text: "见过阎罗王", Kind: "positive"}},
			ReadFirst:    []string{"wiki/entities/镇元子.md"}, CandidateLimit: 5,
		},
		actions: []core.QueryAction{
			{Action: "assess_candidate", Candidate: "镇元子", Rationale: "缺少阎罗王证据", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/镇元子.md"}}, {RequirementID: "7", Status: "unknown"}}},
			{Action: "read", Path: "wiki/entities/唐太宗.md"},
			{Action: "final", Candidate: "唐太宗", Answer: "答案是唐太宗 [wiki/entities/唐太宗.md]。", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}}, {RequirementID: "7", Status: "supported", EvidencePaths: []string{"wiki/entities/唐太宗.md"}}}},
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root, Question: "谁符合条件？", Agent: agent,
		Runtime: QueryRuntimeOptions{InitialActionBudget: 3, MaxActionBudget: 3, VerificationPasses: -1, StagnationRounds: 1, TotalTimeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Status != "complete" || answer.Candidate != "唐太宗" || len(answer.EvidenceChecks) != 2 {
		t.Fatalf("answer=%+v", answer)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Path != "wiki/entities/唐太宗.md" {
		t.Fatalf("final citations must come only from the canonical ledger: %+v", answer.Citations)
	}
	if len(answer.CandidateAssessments) != 2 || answer.CandidateAssessments[0].Candidate != "镇元子" || answer.CandidateAssessments[0].Disposition != "partial" {
		t.Fatalf("candidate assessments=%+v", answer.CandidateAssessments)
	}
}

func TestConstraintLoopBudgetExhaustionReturnsBestCandidateExactGaps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "镇元子.md"), "---\ntype: entity\ntitle: 镇元子\n---\n# 镇元子\n镇元子与孙悟空结为兄弟。\n")
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Intent: "answer_from_persistent_wiki", ReasoningMode: "constraint_satisfaction", RequireAll: true,
			Requirements: []core.QueryRequirement{{ID: "1", Text: "有结义情节", Kind: "positive"}, {ID: "7", Text: "见过阎罗王", Kind: "positive"}},
			ReadFirst:    []string{"wiki/entities/镇元子.md"}, CandidateLimit: 5,
		},
		actions: []core.QueryAction{{Action: "assess_candidate", Candidate: "镇元子", Checks: []core.QueryEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/镇元子.md"}}, {RequirementID: "7", Status: "unknown"}}}},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root, Question: "谁符合条件？", Agent: agent,
		Runtime: QueryRuntimeOptions{InitialActionBudget: 1, MaxActionBudget: 1, VerificationPasses: -1, StagnationRounds: 1, TotalTimeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Status != "incomplete" || answer.Candidate != "镇元子" || !strings.Contains(answer.IncompleteReason, "7. 见过阎罗王") || strings.Contains(answer.IncompleteReason, "1. 有结义") {
		t.Fatalf("answer=%+v", answer)
	}
	if !strings.Contains(answer.IncompleteReason, "步骤上限 1") || !queryTraceContainsAction(answer.Trace, "step_limit") {
		t.Fatalf("step limit was not preserved: %+v", answer)
	}
}

func TestQueryLoopUsesOneHardStepLimitWithoutFallbackSynthesis(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &scriptedActionAgent{
		plan: core.QueryPlan{Intent: QueryIntentWikiQuery, AnswerMode: "llm_tool_loop"},
		actions: []core.QueryAction{
			{Action: "list_pages"},
			{Action: "list_pages"},
			{Action: "list_pages"},
		},
	}
	var events []QueryProgressEvent
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "尚未完成的问题",
		Agent:       agent,
		Runtime: QueryRuntimeOptions{
			MaxSteps:           3,
			VerificationPasses: -1,
		},
		Progress: func(event QueryProgressEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.inputs) != 3 {
		t.Fatalf("turns=%d want 3", len(agent.inputs))
	}
	if agent.synthCalled {
		t.Fatal("step limit must not add a synthesis call")
	}
	if answer.Status != "incomplete" || !strings.Contains(answer.IncompleteReason, "步骤上限 3") {
		t.Fatalf("answer=%+v", answer)
	}
	if !queryTraceContainsAction(answer.Trace, "step_limit") || !queryProgressContains(events, "step_limit_reached") {
		t.Fatalf("missing step limit audit evidence: trace=%+v events=%+v", answer.Trace, events)
	}
}

func TestDefaultQueryRuntimeHasNoTotalDeadline(t *testing.T) {
	defaults := DefaultQueryRuntimeOptions()
	if defaults.MaxSteps != 256 || defaults.TotalTimeout != 0 {
		t.Fatalf("defaults=%+v", defaults)
	}
	normalized := normalizeQueryRuntimeOptions(QueryRuntimeOptions{})
	if normalized.MaxSteps != 256 || normalized.TotalTimeout != 0 {
		t.Fatalf("normalized=%+v", normalized)
	}
	legacy := normalizeQueryRuntimeOptions(QueryRuntimeOptions{MaxActionBudget: 17})
	if legacy.MaxSteps != 17 {
		t.Fatalf("legacy max action budget was not mapped: %+v", legacy)
	}
}

func TestLLMWikiQueryDirectChatBypassesWikiEvidenceLoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &countingQueryAgent{
		plan: core.QueryPlan{
			Question:     "你好",
			Intent:       "answer_from_persistent_wiki",
			AnswerMode:   "llm_tool_loop",
			CanWriteBack: true,
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "你好",
		Agent:       agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.planCalled != 0 || agent.actionCalled != 0 || agent.synthCalled != 0 {
		t.Fatalf("direct chat should not call planner/action/synthesis: %+v", agent)
	}
	if answer.Plan.Intent != "direct_chat" || answer.Plan.CanWriteBack {
		t.Fatalf("plan=%+v", answer.Plan)
	}
	if len(answer.Citations) != 0 || len(answer.Results) != 0 || len(answer.Trace) != 0 {
		t.Fatalf("direct chat should not produce evidence artifacts: %+v", answer)
	}
	if !strings.Contains(answer.Answer, "你好") {
		t.Fatalf("answer=%q", answer.Answer)
	}
}

func TestLLMWikiQueryPlannerDirectChatBypassesActionLoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &countingQueryAgent{
		plan: core.QueryPlan{
			Question:     "How are you?",
			Intent:       "direct_chat",
			AnswerMode:   "direct_chat",
			CanWriteBack: true,
		},
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "How are you?",
		Agent:       agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.planCalled != 1 || agent.actionCalled != 0 || agent.synthCalled != 0 {
		t.Fatalf("planner direct chat should only call planner: %+v", agent)
	}
	if answer.Plan.Intent != "direct_chat" || answer.Plan.CanWriteBack {
		t.Fatalf("plan=%+v", answer.Plan)
	}
	if len(answer.Citations) != 0 {
		t.Fatalf("citations=%+v", answer.Citations)
	}
}

func TestLLMWikiQuerySystemFAQBypassesWikiEvidenceLoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &countingQueryAgent{
		plan: core.QueryPlan{
			Question:     "你能做什么",
			Intent:       "answer_from_persistent_wiki",
			AnswerMode:   "llm_tool_loop",
			CanWriteBack: true,
		},
	}
	var events []QueryProgressEvent
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "你能做什么",
		Agent:       agent,
		Progress: func(event QueryProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.planCalled != 0 || agent.actionCalled != 0 || agent.synthCalled != 0 {
		t.Fatalf("system FAQ should not call planner/action/synthesis: %+v", agent)
	}
	if answer.Plan.Intent != "system_faq" || answer.Plan.CanWriteBack {
		t.Fatalf("plan=%+v", answer.Plan)
	}
	if !strings.Contains(answer.Answer, "Knowledge Core") {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if !queryProgressContains(events, "routing_started") || !queryProgressContains(events, "routing_done") {
		t.Fatalf("expected routing events, got %+v", events)
	}
}

func TestLLMWikiQueryGeneralAssistantBypassesWikiPlanner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &routedQueryAgent{
		route: QueryRouteDecision{
			Intent:     "general_assistant",
			Confidence: 0.9,
			Reason:     "general programming explanation",
		},
		generalAnswer: "Go interface 是一组方法约束。",
		plan: core.QueryPlan{
			Question:     "帮我解释一下 Go interface",
			Intent:       "answer_from_persistent_wiki",
			AnswerMode:   "llm_tool_loop",
			CanWriteBack: true,
		},
	}
	var events []QueryProgressEvent
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "帮我解释一下 Go interface",
		Agent:       agent,
		Progress: func(event QueryProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.routeCalled != 1 || agent.generalCalled != 1 {
		t.Fatalf("route/general calls = %d/%d", agent.routeCalled, agent.generalCalled)
	}
	if agent.planCalled != 0 || agent.actionCalled != 0 || agent.synthCalled != 0 {
		t.Fatalf("general assistant should not call wiki planner/action/synthesis: %+v", agent)
	}
	if answer.Plan.Intent != "general_assistant" || answer.Plan.CanWriteBack {
		t.Fatalf("plan=%+v", answer.Plan)
	}
	if len(answer.Citations) != 0 || len(answer.Results) != 0 || len(answer.Trace) != 0 {
		t.Fatalf("general assistant should not produce wiki evidence artifacts: %+v", answer)
	}
	if !strings.Contains(answer.Answer, "Go interface") {
		t.Fatalf("answer=%q", answer.Answer)
	}
	for _, eventType := range []string{"routing_started", "routing_done", "general_answer_started", "general_answer_done"} {
		if !queryProgressContains(events, eventType) {
			t.Fatalf("expected %s event, got %+v", eventType, events)
		}
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

func TestLLMWikiQueryActionFailureStopsWithoutSyntheticAnswer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

Token validation calls AuthService.
`)
	agent := &failingActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/concepts/oauth.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actionErr: errors.New(`parse llm query action: unexpected end of JSON input: {"action":"writeback","title":"`),
	}
	var events []QueryProgressEvent
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "How is token validation handled?",
		Limit:       5,
		Agent:       agent,
		Progress: func(event QueryProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled {
		t.Fatal("action failure must not trigger fallback synthesis")
	}
	if answer.Status != "incomplete" || !strings.Contains(answer.Answer, "未生成答案") {
		t.Fatalf("answer=%+v", answer)
	}
	if len(answer.Citations) == 0 || answer.Citations[0].Path != "wiki/concepts/oauth.md" {
		t.Fatalf("read evidence should be preserved: %+v", answer.Citations)
	}
	if len(answer.Trace) != 1 || answer.Trace[0].Action.Action != "action_failed" || !strings.Contains(answer.Trace[0].Observation, "action_failed") {
		t.Fatalf("expected explicit action failure trace, got %+v", answer.Trace)
	}
	if !queryProgressContains(events, "action_failed") || queryProgressContains(events, "synthesis_started") {
		t.Fatalf("expected action_failed without synthesis, got %+v", events)
	}
}

func TestLLMWikiQueryActionFailureDoesNotRunFallbackRecall(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

Token validation calls AuthService.
`)
	agent := &failingActionAgent{
		plan: core.QueryPlan{
			Question:       "token validation AuthService",
			Intent:         "answer_from_persistent_wiki",
			CandidateLimit: 3,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actionErr: errors.New(`parse llm query action: unexpected end of JSON input: {"action":"writeback","title":"`),
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "token validation AuthService",
		Limit:       3,
		Agent:       agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled {
		t.Fatal("action failure must not trigger synthesis")
	}
	if answer.Status != "incomplete" || len(answer.Results) != 0 {
		t.Fatalf("action failure must not invent a fallback recall: %+v", answer)
	}
}

func TestLLMWikiQueryActionFailureWithoutEvidenceReturnsIncomplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	agent := &failingActionAgent{
		plan: core.QueryPlan{
			Question:       "missing policy",
			Intent:         "answer_from_persistent_wiki",
			CandidateLimit: 3,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actionErr: errors.New(`parse llm query action: unexpected end of JSON input: {"action":"writeback","title":"`),
	}
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "missing policy",
		Limit:       3,
		Agent:       agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled {
		t.Fatal("should not ask LLM to synthesize without readable evidence")
	}
	if answer.Status != "incomplete" || !strings.Contains(answer.Answer, "未生成答案") || !strings.Contains(answer.IncompleteReason, "查询动作") {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if answer.Plan.CanWriteBack {
		t.Fatalf("failed action must not be writeback eligible: %+v", answer.Plan)
	}
	if len(answer.Citations) != 0 {
		t.Fatalf("expected no citations, got %+v", answer.Citations)
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

func TestLLMWikiQueryReadActionResolvesWrongWikiSubdirByBasename(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "chapter-055-concept.md"), `---
type: "concept"
title: "Chapter 055 Concept"
---

# Chapter 055 Concept

Chapter 055 concept evidence.
`)

	agent := &scriptedActionAgent{
		plan: core.QueryPlan{
			Question:       "What happened in chapter 55?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 5,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "read", Path: "wiki/entities/chapter-055-concept.md", Rationale: "read concept page with wrong subdir"},
			{Action: "final", Answer: "Chapter 055 has concept evidence [wiki/concepts/chapter-055-concept.md].", Rationale: "read evidence is sufficient"},
		},
	}
	answer, err := QueryLLMWikiWithAgent(root, "What happened in chapter 55?", 5, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Trace) != 2 || !strings.Contains(answer.Trace[0].Observation, "wiki/concepts/chapter-055-concept.md") {
		t.Fatalf("expected basename-resolved read trace, got %+v", answer.Trace)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/chapter-055-concept.md") {
		t.Fatalf("final action should see basename-resolved doc: %+v", agent.inputs[1].Docs)
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
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "How does routing work?",
		Limit:       5,
		Agent:       agent,
		Runtime:     QueryRuntimeOptions{MaxSteps: 2, VerificationPasses: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled || answer.Status != "incomplete" {
		t.Fatalf("rejected final must stop without fallback synthesis: %+v", answer)
	}
	if len(answer.Trace) < 2 || !strings.Contains(answer.Trace[1].Observation, "final rejected") {
		t.Fatalf("expected final rejection after list_pages only, got %+v", answer.Trace)
	}
	if !queryTraceContainsAction(answer.Trace, "step_limit") {
		t.Fatalf("expected step limit trace, got %+v", answer.Trace)
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
	if searchStore.called != 1 || searchStore.projectID != "project-1" || searchStore.limit < 3 {
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
	if searchStore.vectorCalled != 1 || searchStore.ftsCalled != 1 {
		t.Fatalf("expected vector and FTS fusion, got vector=%d fts=%d", searchStore.vectorCalled, searchStore.ftsCalled)
	}
	if len(answer.Results) != 1 || answer.Results[0].Path != "wiki/concepts/vector-oauth.md" {
		t.Fatalf("expected vector result, got %+v", answer.Results)
	}
	if !queryDocsContain(agent.inputs[1].Docs, "wiki/concepts/vector-oauth.md") {
		t.Fatalf("final action should see auto-read vector candidate page: %+v", agent.inputs[1].Docs)
	}
}

func TestSearchFusionFallsBackWhenVectorBackendFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "fts-oauth.md"), `---
type: concept
title: FTS OAuth
---
# FTS OAuth
Token validation uses AuthService.
`)
	store := &fakeVectorSearchEvidenceStore{
		vectorErr: errors.New("vector unavailable"),
		ftsResults: []core.QueryResult{{
			Path: "wiki/concepts/fts-oauth.md", Title: "FTS OAuth", Kind: "concept", Score: 500,
		}},
	}
	results, err := SearchWikiCandidatesWithStore(context.Background(), root, "project-1", store, &fakeEmbeddingProvider{embedding: []float32{0.1}}, core.QueryPlan{
		Question: "token validation",
		Searches: []core.QuerySearch{{Text: "token validation", Weight: 6}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if store.vectorCalled != 1 || store.ftsCalled != 1 || len(results) == 0 || results[0].Path != "wiki/concepts/fts-oauth.md" {
		t.Fatalf("fusion did not degrade to FTS/file search: store=%+v results=%+v", store, results)
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
	if agent.synthCalled {
		t.Fatal("should not synthesize without readable evidence after rejecting unread search-result citation")
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
	if writeErr == nil || !strings.Contains(writeErr.Error(), "not eligible") {
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
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: root,
		Question:    "Can we answer from snippets?",
		Limit:       5,
		Agent:       agent,
		Runtime:     QueryRuntimeOptions{MaxSteps: 2, VerificationPasses: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.synthCalled {
		t.Fatal("should not synthesize without readable evidence after repeated unsupported final actions")
	}
	if answer.Status != "incomplete" || !strings.Contains(answer.Answer, "步骤上限") {
		t.Fatalf("answer=%q", answer.Answer)
	}
	if len(answer.Trace) < 3 || !strings.Contains(answer.Trace[0].Observation, "final rejected") || !strings.Contains(answer.Trace[1].Observation, "final rejected") || !queryTraceContainsAction(answer.Trace, "step_limit") {
		t.Fatalf("expected final rejection trace, got %+v", answer.Trace)
	}
	if answer.Plan.CanWriteBack {
		t.Fatalf("missing evidence answer must not be writeback eligible: %+v", answer.Plan)
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
		{
			name: "deep_answer_without_completed_verification",
			answer: &core.QueryAnswer{
				Question: "verified?",
				Plan: core.QueryPlan{
					Intent:              "answer_from_persistent_wiki",
					AnswerMode:          "llm_synthesis",
					CanWriteBack:        true,
					RequireVerification: true,
					VerificationPasses:  2,
				},
				Answer: "unchecked answer",
				Citations: []core.QueryCitation{{
					Path:  "wiki/concepts/oauth.md",
					Title: "OAuth",
					Kind:  "concept",
				}},
				Verification: []core.QueryVerification{{Pass: 1, Kind: "coverage", Accepted: true}},
			},
			want: "completed independent verification",
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

type verifyingActionAgent struct {
	*scriptedActionAgent
	results []core.QueryVerification
	calls   int
}

type failingActionAgent struct {
	plan        core.QueryPlan
	actionErr   error
	inputs      []QueryActionInput
	synthCalled bool
	synthInput  QuerySynthesisInput
}

type countingQueryAgent struct {
	plan         core.QueryPlan
	planCalled   int
	actionCalled int
	synthCalled  int
}

type routedQueryAgent struct {
	plan          core.QueryPlan
	route         QueryRouteDecision
	generalAnswer string
	routeCalled   int
	generalCalled int
	planCalled    int
	actionCalled  int
	synthCalled   int
}

type unifiedTurnTestAgent struct {
	decisions   []core.QueryTurnDecision
	inputs      []QueryActionInput
	turnCalls   int
	planCalls   int
	routeCalls  int
	actionCalls int
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

type requirementSearchStore struct {
	byQuestion map[string][]core.QueryResult
}

func (s requirementSearchStore) SearchWikiEvidence(_ context.Context, _ string, plan core.QueryPlan, _ int) ([]core.QueryResult, error) {
	return s.byQuestion[plan.Question], nil
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
	vectorErr     error
	ftsErr        error
	vectorCalled  int
	ftsCalled     int
	embedding     []float32
}

func (s *fakeVectorSearchEvidenceStore) SearchWikiEvidence(_ context.Context, _ string, _ core.QueryPlan, _ int) ([]core.QueryResult, error) {
	s.ftsCalled++
	return s.ftsResults, s.ftsErr
}

func (s *fakeVectorSearchEvidenceStore) SearchWikiEvidenceVector(_ context.Context, _ string, _ core.QueryPlan, embedding []float32, _ int) ([]core.QueryResult, error) {
	s.vectorCalled++
	s.embedding = embedding
	return s.vectorResults, s.vectorErr
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

func (f *verifyingActionAgent) VerifyQueryAnswerContext(_ context.Context, _ QueryVerificationInput) (core.QueryVerification, error) {
	if f.calls >= len(f.results) {
		return core.QueryVerification{Accepted: false, Summary: "no scripted verification"}, nil
	}
	result := f.results[f.calls]
	f.calls++
	return result, nil
}

func (f *failingActionAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (f *failingActionAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
	f.inputs = append(f.inputs, input)
	if f.actionErr != nil {
		return core.QueryAction{}, f.actionErr
	}
	return core.QueryAction{}, errors.New("action failed")
}

func (f *failingActionAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	f.synthCalled = true
	f.synthInput = input
	if len(input.Docs) == 0 {
		return "degraded synthesis without docs", nil
	}
	return "degraded synthesis from " + input.Docs[len(input.Docs)-1].Path, nil
}

func (f *countingQueryAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	f.planCalled++
	return f.plan, nil
}

func (f *countingQueryAgent) NextQueryAction(QueryActionInput) (core.QueryAction, error) {
	f.actionCalled++
	return core.QueryAction{Action: "final", Answer: "direct final"}, nil
}

func (f *countingQueryAgent) SynthesizeQuery(QuerySynthesisInput) (string, error) {
	f.synthCalled++
	return "direct synthesized", nil
}

func (f *routedQueryAgent) RouteQuery(QueryRoutingInput) (QueryRouteDecision, error) {
	f.routeCalled++
	return f.route, nil
}

func (f *routedQueryAgent) AnswerGeneralQuery(QueryGeneralAnswerInput) (string, error) {
	f.generalCalled++
	return f.generalAnswer, nil
}

func (f *routedQueryAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	f.planCalled++
	return f.plan, nil
}

func (f *routedQueryAgent) NextQueryAction(QueryActionInput) (core.QueryAction, error) {
	f.actionCalled++
	return core.QueryAction{Action: "final", Answer: "direct final"}, nil
}

func (f *routedQueryAgent) SynthesizeQuery(QuerySynthesisInput) (string, error) {
	f.synthCalled++
	return "direct synthesized", nil
}

func (f *unifiedTurnTestAgent) RouteQuery(QueryRoutingInput) (QueryRouteDecision, error) {
	f.routeCalls++
	return QueryRouteDecision{Intent: QueryIntentWikiQuery}, nil
}

func (f *unifiedTurnTestAgent) PlanQuery(QueryPlanningInput) (core.QueryPlan, error) {
	f.planCalls++
	return core.QueryPlan{Intent: QueryIntentWikiQuery}, nil
}

func (f *unifiedTurnTestAgent) NextQueryTurn(input QueryActionInput) (core.QueryTurnDecision, error) {
	f.turnCalls++
	f.inputs = append(f.inputs, input)
	if f.turnCalls > len(f.decisions) {
		return core.QueryTurnDecision{}, errors.New("no more unified decisions")
	}
	return f.decisions[f.turnCalls-1], nil
}

func (f *unifiedTurnTestAgent) NextQueryAction(QueryActionInput) (core.QueryAction, error) {
	f.actionCalls++
	return core.QueryAction{}, errors.New("legacy action method must not be called")
}

func (f *unifiedTurnTestAgent) SynthesizeQuery(QuerySynthesisInput) (string, error) {
	return "unexpected synthesis", nil
}

func queryProgressContains(events []QueryProgressEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func queryTraceContainsAction(trace []core.QueryTraceStep, action string) bool {
	for _, step := range trace {
		if step.Action.Action == action {
			return true
		}
	}
	return false
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
