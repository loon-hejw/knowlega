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
For a multi-constraint identity question, treat the candidate as the common grammatical subject of every requirement and preserve subject/object roles. Prefer action=discover with the full numbered requirements to rank shared-subject entity candidates before cycling through paraphrased searches, then verify every positive requirement with a candidate-specific search carrying candidate and the same requirement_id. After each positive search, read one returned path and cite that path for the matching check; an unrelated current-turn read cannot support the condition. Reject a candidate as soon as a requirement contradicts it.
If status or search reports pending, do not conclude that the project has no relevant material: raw sources can be searched and read before derived wiki pages finish compiling, and unresolved requirements must stay explicit.
If Knowledge reports empty, unavailable, or failed, do not replace project evidence with general model knowledge and do not guess a candidate. State that the project facts could not be verified and leave the affected requirements unresolved.
Before presenting a definite evidence-backed project answer, call knowledge action=submit and require its returned status to be complete. Every submit call must include non-empty question, answer, candidate, the full requirements array, and exactly one check per requirement. Use the exact title or alias of the candidate page as candidate; put explanatory alternate names in answer. If submit returns incomplete, follow its validation_issues and any repair action literally, gather or correct the missing evidence, and submit again; never describe an incomplete submission as verified, infer a backend failure while validation_issues remain, or present its candidate as the answer. For a negative requirement, search for the positive event that would disprove it, for example {"action":"search","query":"到过 花果山","candidate":"<exact candidate title or alias>","requirement_id":"<same requirement id>"}; candidate is strict metadata and must not be repeated in query, and unrelated extra conditions must not be added to manufacture a zero result. A negative requirement resolves only as not_found_in_corpus after a current-turn zero-result candidate-specific search in a ready workspace; selected citations that merely omit the event are not enough for supported. If a broad search returns a mention-only hit, read it and refine the query using only event terms already present in the requirement. If a valid event search shows that the candidate did the forbidden thing, reject that candidate.
Only call action=writeback when the user explicitly asks to save a useful synthesis. Never save answers automatically.`
	input.Environment = joinEnvironment(input.Environment, instruction)
	return nil, nil
}
