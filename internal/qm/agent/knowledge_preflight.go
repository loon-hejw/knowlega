package agent

import (
	"context"
	"strings"
)

const projectKnowledgeScopePrefix = "group:web-project-"

type TurnInputPreparer interface {
	PrepareTurnInput(context.Context, *TurnInput) (*TurnResult, error)
}

// ProjectKnowledgePreparer adds stable tool semantics only. It never classifies
// the request, calls a model, or runs Knowledge Core before Pi starts reasoning.
type ProjectKnowledgePreparer struct{}

func NewProjectKnowledgePreparer() *ProjectKnowledgePreparer {
	return &ProjectKnowledgePreparer{}
}

func isProjectKnowledgeScope(scopeLabel string) bool {
	return strings.HasPrefix(strings.TrimSpace(scopeLabel), projectKnowledgeScopePrefix)
}

func (p *ProjectKnowledgePreparer) PrepareTurnInput(_ context.Context, input *TurnInput) (*TurnResult, error) {
	if p == nil || input == nil || !isProjectKnowledgeScope(input.ScopeLabel) || input.Tools == nil {
		return nil, nil
	}
	instruction := `Project Knowledge is available through the single knowledge tool.
Decide autonomously whether the current request needs project facts; greetings, writing, execution, brainstorming, and general conversation do not need a forced knowledge call.
For project factual claims, use search/discover/list only to navigate, then read/follow_links/graph to collect evidence. Search snippets and page lists are never evidence.
For a multi-constraint identity question, treat the candidate as the common grammatical subject of every requirement and preserve subject/object roles. Choose search, action=discover, read, follow_links, or graph as needed; there is no prescribed sequence or per-condition search quota. One current-turn evidence page may support multiple requirements. A dedicated candidate entity page is not required. Use returned canonical paths, titles, or aliases when reading; do not guess filenames. Reject a candidate when evidence contradicts a requirement.
If status or search reports pending, do not conclude that the project has no relevant material: raw sources can be searched and read before derived wiki pages finish compiling, and unresolved requirements must stay explicit.
If Knowledge reports empty, unavailable, or failed, do not replace project evidence with general model knowledge and do not guess a candidate. State that the project facts could not be verified and leave the affected requirements unresolved.
Before presenting a definite evidence-backed project answer, call knowledge action=submit and require its returned status to be complete. Every submit call must include non-empty question, answer, candidate, the full requirements array, and exactly one check per requirement. Identify the candidate consistently with the evidence. If submit returns incomplete, use validation_issues to decide whether to gather evidence, correct checks, revise the candidate, or search again, and resubmit. Repair suggestions are guidance, not a mandatory action sequence. Never present its candidate as the answer while submission remains incomplete. For a negative requirement, explicit negative evidence can support a supported check; selected citations that merely omit an event are not enough for supported. Otherwise search for the positive event that would disprove the condition, carrying candidate and requirement_id, and use not_found_in_corpus only for a relevant zero-result search in a ready workspace. Do not pad the query with unrelated extra conditions. This means absence in the searched corpus, not universal absence. If evidence remains insufficient, explain uncertainty and the missing conditions in ordinary language, without internal validation codes or instructions to the user.
Only call action=writeback when the user explicitly asks to save a useful synthesis. Never save answers automatically.`
	input.Environment = joinEnvironment(input.Environment, instruction)
	return nil, nil
}
