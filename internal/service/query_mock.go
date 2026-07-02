package service

import (
	"fmt"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
)

// MockQueryAgent is a deterministic offline action-loop agent for local
// validation. It exercises the LLM Wiki tools without pretending to do semantic
// reasoning.
type MockQueryAgent struct{}

func (MockQueryAgent) PlanQuery(input QueryPlanningInput) (core.QueryPlan, error) {
	return core.QueryPlan{
		Question: input.Question,
		Intent:   "mock_llmwiki_tool_loop",
		ReadFirst: []string{
			"wiki/index.md",
			"wiki/overview.md",
			"wiki/log.md",
		},
		CandidateLimit: 5,
		AnswerMode:     "mock_tool_loop",
		CanWriteBack:   false,
	}, nil
}

func (MockQueryAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
	if mockCodeGraphQuestion(input.Question) {
		if doc := firstGraphEvidenceDoc(input.Docs); doc.Path != "" {
			return core.QueryAction{
				Action: "final",
				Answer: fmt.Sprintf(
					"Mock LLM Wiki answer from graph evidence. The most relevant graph evidence is %s [%s].",
					doc.Title,
					doc.Path,
				),
				Rationale: "graph evidence is available; code relationship questions should not fall back to text search",
			}, nil
		}
		return core.QueryAction{
			Action:    "graph",
			Query:     input.Question,
			Limit:     input.Plan.CandidateLimit,
			Rationale: "code relationship question should use graph evidence",
		}, nil
	}
	switch input.Step {
	case 1:
		return core.QueryAction{
			Action:    "list_pages",
			Query:     input.Question,
			Limit:     8,
			Rationale: "inspect wiki navigation before recall",
		}, nil
	case 2:
		if page := firstNavigatedEvidencePage(input.Navigation); page.Path != "" {
			return core.QueryAction{
				Action:    "read",
				Path:      page.Path,
				Rationale: "read the most relevant navigated wiki page",
			}, nil
		}
		return core.QueryAction{
			Action:    "search",
			Query:     input.Question,
			Limit:     input.Plan.CandidateLimit,
			Rationale: "navigation was insufficient; use recall as a tool",
		}, nil
	default:
		doc := firstAnswerEvidenceDoc(input.Docs)
		if doc.Path == "" {
			return core.QueryAction{
				Action:    "search",
				Query:     input.Question,
				Limit:     input.Plan.CandidateLimit,
				Rationale: "need a readable evidence page before final",
			}, nil
		}
		return core.QueryAction{
			Action: "final",
			Answer: fmt.Sprintf(
				"Mock LLM Wiki answer from read evidence. The most relevant evidence read is %s (%s) [%s].",
				doc.Title,
				doc.Kind,
				doc.Path,
			),
			Rationale: "read evidence is available; search/list outputs are not used directly as the answer",
		}, nil
	}
}

func mockCodeGraphQuestion(q string) bool {
	q = strings.ToLower(q)
	codeTerms := []string{
		"call", "calls", "called by", "function", "method", "class", "route", "handler",
		"repo", "graph", "symbol", "impact", "trace", "validatetoken", "authservice",
	}
	for _, term := range codeTerms {
		if strings.Contains(q, term) {
			return true
		}
	}
	return false
}

func firstGraphEvidenceDoc(docs []QueryReadDocument) QueryReadDocument {
	for _, doc := range docs {
		if doc.Kind == "code-graph" || strings.HasPrefix(doc.Path, "raw/code-graphs/") {
			return doc
		}
	}
	return QueryReadDocument{}
}

func (MockQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	doc := firstAnswerEvidenceDoc(input.Docs)
	if doc.Path == "" {
		return "Mock LLM Wiki answer could not be produced because no readable evidence page was collected.", nil
	}
	return fmt.Sprintf("Mock LLM Wiki synthesis from %s [%s].", doc.Title, doc.Path), nil
}

func firstNavigatedEvidencePage(observations []QueryNavigationObservation) QueryNavigationPage {
	for _, observation := range observations {
		for _, page := range observation.Pages {
			if page.Path != "" && !isAggregateWikiPath(page.Path) {
				return page
			}
		}
	}
	return QueryNavigationPage{}
}

func firstAnswerEvidenceDoc(docs []QueryReadDocument) QueryReadDocument {
	for _, doc := range docs {
		if strings.TrimSpace(doc.Path) != "" && !isAggregateWikiPath(doc.Path) {
			return doc
		}
	}
	return QueryReadDocument{}
}
