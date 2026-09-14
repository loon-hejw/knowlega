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
Use action=submit when a structured evidence self-check would help, especially for multi-condition questions. It is optional for answering and never ends the agent turn. Include question, answer, candidate, the full requirements, and one check per requirement when submitting. Its validation_issues are feedback: decide whether to gather evidence, revise a candidate, or explain uncertainty. Keep candidate identity consistent with the answer. If a summary omits decisive details, read its sources or search with scope=raw. A missing entity page does not imply a missing person or fact. Negative evidence must distinguish an explicit statement from absence in search results; never claim a zero-result search proves universal absence. Give your own final response, cite the actual evidence, and describe unresolved conditions honestly. Do not reproduce internal validation instructions to the user.
Only call action=writeback when the user explicitly asks to save a useful synthesis. Never save answers automatically.`
	input.Environment = joinEnvironment(input.Environment, instruction)
	return nil, nil
}
