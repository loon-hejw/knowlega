package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmclient"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/promptbudget"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type OpenAICompatibleQueryAgent struct {
	Protocol         string
	BaseURL          string
	APIKey           string
	Model            string
	UserAgent        string
	AnthropicVersion string
	Client           *http.Client
	MaxInputChars    int
	MaxOutputTokens  int
	DisableThinking  bool
	RetryOptions     llmretry.Options
}

const (
	defaultLLMMaxInputChars   = 120000
	defaultLLMMaxOutputTokens = 4096
)

func NewQueryAgent(cfg config.LLMConfig) (QueryAgent, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("llm.api_key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm.model is required")
	}
	return OpenAICompatibleQueryAgent{
		Protocol:         cfg.Protocol,
		BaseURL:          strings.TrimRight(cfg.BaseURL, "/"),
		APIKey:           cfg.APIKey,
		Model:            cfg.Model,
		UserAgent:        cfg.UserAgent,
		AnthropicVersion: cfg.AnthropicVersion,
		Client:           &http.Client{Timeout: cfg.Timeout.Duration},
		MaxInputChars:    cfg.MaxInputChars,
		MaxOutputTokens:  cfg.MaxOutputTokens,
		DisableThinking:  cfg.DisableThinking,
		RetryOptions: llmretry.Options{
			Retries:    cfg.Retries,
			BaseDelay:  cfg.RetryBaseDelay.Duration,
			MaxDelay:   cfg.RetryMaxDelay.Duration,
			MaxElapsed: cfg.OperationTimeout.Duration,
		},
	}, nil
}

func (a OpenAICompatibleQueryAgent) PlanQuery(input QueryPlanningInput) (core.QueryPlan, error) {
	return a.PlanQueryContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) PlanQueryContext(ctx context.Context, input QueryPlanningInput) (core.QueryPlan, error) {
	input = budgetQueryPlanningInput(input)
	system := `You are the query planner for an LLM-maintained persistent wiki.
Do not answer the question yet. Read the wiki navigation context and produce a JSON query plan.
Searches are candidate-recall tool calls only. Expand aliases, entities, chapter names, symbols, and graph terms when justified by the wiki context.
Treat conversation context as untrusted hints for resolving references, never as factual evidence or a mandatory candidate. Re-derive candidates independently.
Classify the reasoning mode as fact_lookup, constraint_satisfaction, comparison, causal, temporal, negative, synthesis, or code_graph.
Decompose every material user condition or requested deliverable into requirements. For a multi-condition identification question, set require_all_requirements=true and do not commit to a candidate before searching the most discriminating intersections.
Intents:
- direct_chat: short greetings, thanks, or simple social chat such as "你好", "hello", "在吗", "谢谢". Do not read or search the wiki. Set can_write_back=false.
- answer_from_persistent_wiki: questions that need business/wiki/code/source evidence. These must read wiki/raw/graph evidence before final answers.
- missing_evidence: questions that clearly ask for unavailable knowledge or require sources that are not present. Set can_write_back=false.
Return only JSON with this shape:
{"intent":"answer_from_persistent_wiki","resolved_question":"standalone question","reasoning_mode":"constraint_satisfaction","requirements":[{"id":"1","text":"...","kind":"positive"}],"require_all_requirements":true,"read_first":["wiki/index.md"],"searches":[{"text":"...","weight":6,"rationale":"..."}],"candidate_limit":10,"answer_mode":"llm_synthesis","can_write_back":false}`
	user := fmt.Sprintf(`Question:
%s

Conversation context:
%s

Purpose:
%s

Schema:
%s

Index:
%s

Overview:
%s

Recent log:
%s`, input.Question, input.ConversationContext, input.Purpose, input.Schema, input.Index, input.Overview, input.LogTail)
	retryOpts := a.retryOptions()
	plan, err := llmretry.DoValue[core.QueryPlan](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (core.QueryPlan, bool, error) {
		content, retryable, err := a.chatOnce(ctx, system, user)
		if err != nil {
			return core.QueryPlan{}, retryable, err
		}
		plan, err := decodeLLMJSONObject[core.QueryPlan](content, "llm query plan")
		if err != nil {
			return core.QueryPlan{}, true, err
		}
		return plan, false, nil
	})
	if err != nil {
		return fallbackQueryPlan(input.Question), nil
	}
	if plan.Question == "" {
		plan.Question = input.Question
	}
	if plan.Intent == "" {
		plan.Intent = "answer_from_persistent_wiki"
	}
	plan.Intent = normalizeQueryIntent(plan.Intent)
	if plan.CandidateLimit <= 0 {
		plan.CandidateLimit = 10
	}
	if plan.Intent == "direct_chat" || plan.Intent == "missing_evidence" {
		plan.ReadFirst = nil
		plan.Searches = nil
		plan.CanWriteBack = false
	} else if len(plan.ReadFirst) == 0 {
		plan.ReadFirst = []string{"wiki/index.md"}
	}
	if plan.AnswerMode == "" {
		plan.AnswerMode = plan.Intent
		if plan.AnswerMode == "answer_from_persistent_wiki" {
			plan.AnswerMode = "llm_synthesis"
		}
	}
	return plan, nil
}

func fallbackQueryPlan(question string) core.QueryPlan {
	return core.QueryPlan{
		Question: question,
		Intent:   "answer_from_persistent_wiki",
		ReadFirst: []string{
			"wiki/index.md",
			"wiki/overview.md",
		},
		Searches: []core.QuerySearch{{
			Text:      strings.TrimSpace(question),
			Weight:    6,
			Rationale: "fallback plan because the LLM planner returned invalid JSON",
		}},
		CandidateLimit: 10,
		AnswerMode:     "llm_synthesis_with_fallback_plan",
		CanWriteBack:   false,
	}
}

func (a OpenAICompatibleQueryAgent) GenerateQueryHypothesesContext(ctx context.Context, input QueryHypothesisInput) ([]core.QueryHypothesis, error) {
	var recalled strings.Builder
	for index, result := range budgetQueryResults(input.Results, 64) {
		fmt.Fprintf(&recalled, "%d. path=%s title=%s kind=%s score=%d snippet=%s\n", index+1, result.Path, result.Title, result.Kind, result.Score, result.Snippet)
	}
	var evidence strings.Builder
	shown := 0
	for _, doc := range input.Docs {
		if isAggregateWikiPath(doc.Path) || shown >= 12 {
			continue
		}
		shown++
		fmt.Fprintf(&evidence, "\n---EVIDENCE %d---\npath: %s\ntitle: %s\n%s\n", shown, doc.Path, doc.Title, promptbudget.TrimMiddle(doc.Content, 1200))
	}
	system := `You independently generate candidate hypotheses for a persistent-wiki constraint question.
Do not use prior conversation or previously suggested candidates. Work from the complete conjunction of requirements, prioritizing rare intersections over superficial similarity.
Return only JSON: {"hypotheses":[{"candidate":"name","rationale":"why the full intersection may fit","discriminators":["rare condition"],"suggested_reads":["exact wiki title/path or chapter"]}]}.
Include 3-6 diverse candidates. Keep each rationale to one short sentence, at most 3 discriminators, and at most 3 suggested_reads. Do not audit requirements or speculate beyond the supplied evidence in this stage. A candidate known from the supplied index that connects multiple rare requirements should rank ahead of a famous candidate matching only early requirements.

Constraint interpretation rules:
- Every requirement constrains the same unknown subject, but the object of each relation is only the person or place explicitly named in that requirement. Never borrow an object from an adjacent requirement. For example, an unspecified brotherhood requirement does not imply brotherhood with a person named in the next line.
- Generate candidates across roles: humans, rulers, allies, deities, monsters, and minor figures. Do not assume that coercing someone means the candidate is an antagonist; it may be a harmless social, ritual, official, or hospitality request.
- Start from the rarest two- or three-requirement intersections, especially unusual meetings, then test ordinary-looking requirements. Include at least one role-inverted/non-antagonist hypothesis.
- Treat negative requirements as elimination checks, not positive recall keywords.
- suggested_reads must name the candidate's entity page first when such a page is visible in the index, followed by the most discriminating source chapters.`
	user := fmt.Sprintf(`Question: %s
Requirements: %s
Wiki index: %s
Overview: %s
Wide per-requirement recall candidates (hints only):
%s
Read evidence previews:
%s`, input.Question, mustJSON(input.Requirements), promptbudget.TrimMiddle(input.Index, 18000), promptbudget.TrimMiddle(input.Overview, 8000), recalled.String(), evidence.String())
	type response struct {
		Hypotheses []core.QueryHypothesis `json:"hypotheses"`
	}
	retryOpts := a.retryOptions()
	repairInstruction := ""
	result, err := llmretry.DoValue[response](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (response, bool, error) {
		attemptUser := user
		if attempt > 0 && repairInstruction != "" {
			attemptUser += "\n\nHYPOTHESIS FORMAT REPAIR REQUIRED:\n" + repairInstruction + "\nReturn 3-6 candidates in exactly one compact JSON object. Each rationale must be one short sentence; do not include evidence_checks or long chain-of-thought."
		}
		content, retryable, err := a.chatOnce(ctx, system, attemptUser)
		if err != nil {
			return response{}, retryable, err
		}
		value, err := decodeLLMJSONObject[response](content, "query hypotheses")
		if err != nil {
			repairInstruction = compactLLMParseFailure(err)
			return response{}, true, err
		}
		if len(value.Hypotheses) < 3 || len(value.Hypotheses) > 6 {
			err = fmt.Errorf("query hypotheses must contain 3-6 candidates; got %d", len(value.Hypotheses))
			repairInstruction = compactLLMParseFailure(err)
			return response{}, true, err
		}
		for _, hypothesis := range value.Hypotheses {
			if strings.TrimSpace(hypothesis.Candidate) == "" {
				err = errors.New("query hypothesis candidate must not be empty")
				repairInstruction = compactLLMParseFailure(err)
				return response{}, true, err
			}
		}
		return value, false, nil
	})
	if err != nil {
		return nil, err
	}
	if len(result.Hypotheses) > 6 {
		result.Hypotheses = result.Hypotheses[:6]
	}
	return result.Hypotheses, nil
}

func (a OpenAICompatibleQueryAgent) AuditQueryCandidatesContext(ctx context.Context, input QueryCandidateAuditInput) ([]core.QueryHypothesis, error) {
	var evidence strings.Builder
	for _, pack := range input.Packs {
		fmt.Fprintf(&evidence, "\n=== CANDIDATE: %s ===\n", pack.Candidate)
		for index, doc := range pack.Docs {
			limit := 1800
			if index == 0 {
				limit = 6000
			}
			fmt.Fprintf(&evidence, "\n---CANDIDATE DOC %d---\npath: %s\ntitle: %s\n%s\n", index+1, doc.Path, doc.Title, promptbudget.TrimMiddle(doc.Content, limit))
		}
	}
	system := `You independently audit candidate people for a persistent-wiki constraint-satisfaction question.
Return only JSON: {"hypotheses":[{"candidate":"exact supplied candidate","rationale":"full-intersection audit","discriminators":["decisive condition"],"suggested_reads":[],"coverage":0,"evidence_checks":[{"requirement_id":"1","status":"supported|contradicted|unknown|not_found_in_corpus","evidence_paths":["exact supplied path"],"explanation":"candidate-specific reason"}]}]}.

Rules:
- Audit every supplied candidate against every requirement. Every requirement applies to the same candidate; never combine people.
- Return each supplied candidate exactly once and each requirement exactly once. Keep each explanation to one short sentence and emit exactly one JSON object without repetition.
- Use only documents inside that candidate's evidence pack and copy evidence paths exactly.
- Rank candidates by supported requirement count, then by fewest contradictions and unknowns. coverage is the count of supported/not_found_in_corpus checks with evidence.
- Distinguish the subject from relation objects. A person can satisfy an unspecified brotherhood clue by forming a brotherhood with anyone; it need not be with the person named in the next clue.
- For coercion, insistence, refusal, or compliance conditions, require a direct candidate-specific event sequence. Broad causation, route obstruction, family association, or a later solution to a different event is insufficient.
- Directly sharing a documented reception or introduction scene satisfies "met Sun Wukong" even if an entity summary omitted the edge.
- A negative condition may be not_found_in_corpus only when the supplied candidate pack has multi-chapter provenance and no passage attributes the excluded visit to that candidate. Explain that it is corpus-scoped, not universal proof.
- Do not reject a candidate merely because its compact entity page omitted a fact that a supplied primary source or source-summary explicitly contains.
- Do not return a closest partial match as complete. Unknown remains unknown.`
	user := fmt.Sprintf(`Question: %s
Requirements: %s
Initial hypotheses (not authoritative): %s
Candidate-specific evidence packs:
%s`, input.Question, mustJSON(input.Requirements), mustJSON(input.Hypotheses), evidence.String())
	type response struct {
		Hypotheses []core.QueryHypothesis `json:"hypotheses"`
	}
	repairInstruction := ""
	result, err := llmretry.DoValue[response](ctx, a.retryOptions(), queryLLMRetryCallback(ctx), func(attempt int) (response, bool, error) {
		attemptUser := user
		if attempt > 0 && repairInstruction != "" {
			attemptUser += "\n\nAUDIT REPAIR REQUIRED:\n" + repairInstruction + "\nReturn every supplied candidate exactly once and every requirement exactly once for each candidate. Return one JSON object only."
		}
		content, retryable, err := a.chatOnce(ctx, system, attemptUser)
		if err != nil {
			return response{}, retryable, err
		}
		value, err := decodeLLMJSONObject[response](content, "query candidate audit")
		if err != nil {
			repairInstruction = compactLLMParseFailure(err)
			return response{}, true, err
		}
		if err := validateQueryCandidateAuditResponse(value.Hypotheses, input.Packs, input.Requirements); err != nil {
			repairInstruction = compactLLMParseFailure(err)
			return response{}, true, err
		}
		return value, false, nil
	})
	if err != nil {
		return nil, err
	}
	return result.Hypotheses, nil
}

func validateQueryCandidateAuditResponse(audits []core.QueryHypothesis, packs []QueryCandidateEvidencePack, requirements []core.QueryRequirement) error {
	expectedCandidates := map[string]string{}
	for _, pack := range packs {
		key := strings.ToLower(strings.TrimSpace(pack.Candidate))
		if key != "" {
			expectedCandidates[key] = pack.Candidate
		}
	}
	expectedRequirements := map[string]bool{}
	for _, requirement := range requirements {
		expectedRequirements[strings.TrimSpace(requirement.ID)] = true
	}
	seenCandidates := map[string]bool{}
	for _, audit := range audits {
		key := strings.ToLower(strings.TrimSpace(audit.Candidate))
		if _, ok := expectedCandidates[key]; !ok {
			return fmt.Errorf("candidate audit returned unexpected candidate %q", audit.Candidate)
		}
		if seenCandidates[key] {
			return fmt.Errorf("candidate audit returned duplicate candidate %q", audit.Candidate)
		}
		seenCandidates[key] = true
		seenRequirements := map[string]bool{}
		for _, check := range audit.Checks {
			id := strings.TrimSpace(check.RequirementID)
			if !expectedRequirements[id] {
				return fmt.Errorf("candidate %q returned unexpected requirement %q", audit.Candidate, id)
			}
			if seenRequirements[id] {
				return fmt.Errorf("candidate %q returned duplicate requirement %q", audit.Candidate, id)
			}
			seenRequirements[id] = true
			status := strings.ToLower(strings.TrimSpace(check.Status))
			switch status {
			case "supported", "contradicted", "unknown", "not_found_in_corpus":
			default:
				return fmt.Errorf("candidate %q requirement %q has invalid status %q", audit.Candidate, id, check.Status)
			}
		}
		if len(seenRequirements) != len(expectedRequirements) {
			return fmt.Errorf("candidate %q audited %d/%d requirements", audit.Candidate, len(seenRequirements), len(expectedRequirements))
		}
	}
	if len(seenCandidates) != len(expectedCandidates) {
		missing := make([]string, 0, len(expectedCandidates)-len(seenCandidates))
		for key, candidate := range expectedCandidates {
			if !seenCandidates[key] {
				missing = append(missing, candidate)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("candidate audit returned %d/%d candidates; missing: %s", len(seenCandidates), len(expectedCandidates), strings.Join(missing, ", "))
	}
	return nil
}

func (a OpenAICompatibleQueryAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
	return a.NextQueryActionContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) NextQueryActionContext(ctx context.Context, input QueryActionInput) (core.QueryAction, error) {
	decision, err := a.NextQueryTurnContext(ctx, input)
	return decision.Action, err
}

func (a OpenAICompatibleQueryAgent) NextQueryTurn(input QueryActionInput) (core.QueryTurnDecision, error) {
	return a.NextQueryTurnContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) NextQueryTurnContext(ctx context.Context, input QueryActionInput) (core.QueryTurnDecision, error) {
	allReadPaths := queryReadPathInventory(input.Docs)
	input = budgetQueryActionInput(input, 36000)
	var docs strings.Builder
	for i, doc := range input.Docs {
		fmt.Fprintf(&docs, "\n---DOC %d---\npath: %s\nkind: %s\ntitle: %s\n\n%s\n", i+1, doc.Path, doc.Kind, doc.Title, doc.Content)
	}
	var results strings.Builder
	for i, result := range input.Results {
		fmt.Fprintf(&results, "%d. path=%s kind=%s title=%s score=%d snippet=%s\n", i+1, result.Path, result.Kind, result.Title, result.Score, result.Snippet)
	}
	var navigation strings.Builder
	for i, obs := range input.Navigation {
		fmt.Fprintf(&navigation, "Navigation %d: action=%s query=%q path=%q\n", i+1, obs.Action, obs.Query, obs.Path)
		for j, page := range obs.Pages {
			fmt.Fprintf(&navigation, "  %d. path=%s type=%s title=%s score=%d\n", j+1, page.Path, page.Type, page.Title, page.Score)
		}
		if len(obs.Skipped) > 0 {
			fmt.Fprintf(&navigation, "  skipped=%s\n", strings.Join(obs.Skipped, ", "))
		}
	}
	system := `You are the single continuing agent operating a persistent LLM Wiki through tools.
On the first turn, classify the request, decompose material conditions, and choose the first real action in the same response. On later turns, continue from the persisted plan, evidence, candidate ledger, and tool trace.
Return only JSON in this envelope:
{"intent":"wiki_query|direct_chat|system_faq|general_assistant|missing_evidence|unsupported","resolved_question":"standalone question","reasoning_mode":"constraint_satisfaction|fact_lookup|comparison|causal|temporal|negative|synthesis|code_graph","requirements":[{"id":"1","text":"...","kind":"positive|negative"}],"hypotheses":[{"candidate":"...","rationale":"why it may fit the full conjunction","discriminators":["decisive condition"],"suggested_reads":["exact title/path"]}],"require_all_requirements":true,"can_write_back":false,"action":{...one action below...}}
Intent and planning fields are required on step 1 and may be omitted later. action is always required.

Actions:
- {"action":"discover_candidates","rationale":"run independent per-requirement recall, hypothesis generation, and candidate audit"}
- {"action":"read","path":"wiki/... or raw/sources/... or exact wiki title/link/alias","rationale":"why this evidence is needed"}
- {"action":"list_pages","query":"optional title/path/type terms","limit":20,"rationale":"why wiki navigation is needed"}
- {"action":"follow_links","path":"wiki/...","limit":5,"rationale":"why linked wiki pages should be inspected"}
- {"action":"search","query":"search terms","limit":5,"rationale":"why recall is needed"}
- {"action":"graph","query":"symbol, file, relation, route, or concept","limit":5,"rationale":"why graph evidence is needed"}
- {"action":"assess_candidate","candidate":"one candidate","evidence_checks":[{"requirement_id":"1","status":"supported|contradicted|unknown|not_found_in_corpus","evidence_paths":["wiki/..."],"explanation":"candidate-specific finding"}],"rationale":"record the current candidate ledger before switching or filling gaps"}
- {"action":"finish_incomplete","candidate":"best audited real candidate, if any","evidence_checks":[{"requirement_id":"1","status":"supported|contradicted|unknown|not_found_in_corpus","evidence_paths":["wiki/..."],"explanation":"exact unresolved state"}],"answer":"optional concise partial conclusion","rationale":"why the corpus cannot currently close the remaining exact gaps"}
- {"action":"final","candidate":"single checked candidate when applicable","evidence_checks":[{"requirement_id":"1","status":"supported|contradicted|unknown|not_found_in_corpus","evidence_paths":["wiki/..."],"explanation":"brief audit"}],"answer":"final cited answer","rationale":"why every requirement is closed"}
- {"action":"writeback","title":"short synthesis page title","answer":"final cited answer","rationale":"why this synthesis should be saved"}

Rules:
- Every request receives at least this one model turn. direct_chat, system_faq, general_assistant, and unsupported should return final immediately without pretending to use wiki evidence.
- On step 1, classify every requirement as positive or negative. For constraint_satisfaction, the only valid first action is discover_candidates. Do not guess candidates on this turn; the dedicated discovery stage independently recalls every positive condition, generates diverse hypotheses, and audits them.
- Hypotheses created by discovery are provisional reasoning state, never evidence. Derive later candidate decisions from the complete conjunction and the canonical candidate assessments. Include role-inverted or non-obvious candidates when the wording permits them; do not assume that pressure, refusal, or compliance implies an antagonist.
- Never issue a broad query made by concatenating most of the question. After discovery, search only the exact unresolved condition for one active real candidate.
- wiki_query must use read/search/follow_links/graph evidence before final. missing_evidence must perform a search or read attempt before concluding that evidence is absent.
- Prefer wiki navigation before broad search: read wiki/index.md, list_pages, follow_links from relevant pages, then search only when navigation is insufficient.
- list_pages is navigation only, not factual evidence for final answers.
- follow_links reads linked wiki pages and can provide final-answer evidence.
- Search is candidate recall only; use read after search before making factual claims.
- Follow the plan's reasoning_mode. For constraint_satisfaction, you own one continuing candidate-by-requirement ledger across every tool action. Start from the rarest conjunction, not from the first or most famous clue.
- Use assess_candidate when a candidate remains partial or is contradicted. The runtime preserves the ledger. Then retrieve at most one or two exact gaps for that active candidate, switch to another audited real candidate, or use finish_incomplete; do not repeatedly submit an unchanged rejected candidate.
- Treat prior candidate assessments as durable tool state, not as separate agents. Preserve supported checks when new evidence is read, but correct them when a cited passage disproves them.
- Never combine facts about different subjects into one candidate. Evidence paths in evidence_checks must be documents actually read.
- For every check, verify that the candidate is the semantic subject or an actual participant in the stated relation. Keep coupled conditions tied to the same event, and distinguish literal events from figures of speech.
- wiki/index.md, wiki/overview.md, wiki/log.md, and wiki/reviews.md are navigation/aggregate pages and are forbidden in evidence_checks. Cite concrete entity, concept, source-summary, raw-source, or graph evidence instead.
- Copy evidence paths exactly from "All read document paths". A path appearing only in the plan, navigation observations, or search results is not read evidence.
- A negative requirement may use not_found_in_corpus only after searching the supplied corpus scope; phrase it as absence in the current corpus, not universal proof.
- not_found_in_corpus never satisfies a positive requirement. A candidate missing a positive requirement is incomplete and you must keep searching alternatives.
- A candidate must resolve to an actual readable wiki entity page. Never use placeholders such as "none", "unknown", a rationale sentence, or an answer paragraph as candidate.
- Use finish_incomplete only after the strongest audited candidates have been checked and the exact remaining gaps cannot be closed. It is preferable to inventing a closest-match answer.
- If a prior candidate from conversation fails requirements, discard it and generate independent alternatives. Do not answer with the closest partial match.
- Use graph for code questions, impact/trace questions, symbol relationships, routes, tools, and graphify/GitNexus evidence.
- Cite paths in final answers.
- Use writeback instead of final when the answer is a reusable synthesis that should become a wiki/syntheses page. The runtime only records your suggested title; the caller still decides whether to save it.
- Read can use project-relative wiki/raw paths, relative wiki links, or exact wiki page titles/aliases. Resolved files must stay under wiki/ or raw/sources/. Graph evidence may cite raw/code-graphs/.`
	user := fmt.Sprintf(`Question:
%s

Conversation context:
%s

Query plan:
%s

First-turn project context (navigation and guidance, not final-answer evidence):
Purpose:
%s

Schema:
%s

Index:
%s

Overview:
%s

Recent log:
%s

Canonical candidate assessments from prior tool actions:
%s

Step: %d

Trace:
%s

Known search results:
%s

Navigation observations:
%s

All read document paths (aggregate paths are navigation only):
%s

	Read documents:
%s`, input.Question, input.ConversationContext, mustJSON(input.Plan), input.Purpose, input.Schema, input.Index, input.Overview, input.LogTail, mustJSON(input.CandidateAssessments), input.Step, mustJSON(input.Trace), results.String(), navigation.String(), allReadPaths, docs.String())
	retryOpts := a.retryOptions()
	repairInstruction := ""
	return llmretry.DoValue[core.QueryTurnDecision](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (core.QueryTurnDecision, bool, error) {
		attemptUser := user
		if attempt > 0 && repairInstruction != "" {
			attemptUser += "\n\nFORMAT REPAIR REQUIRED:\n" + repairInstruction + "\nReturn exactly one JSON object and no second object, commentary, or markdown fence."
		}
		content, retryable, err := a.chatOnce(ctx, system, attemptUser)
		if err != nil {
			return core.QueryTurnDecision{}, retryable, err
		}
		decision, normalized, err := decodeQueryTurnDecisionWithNormalization(content)
		if err != nil {
			repairInstruction = compactLLMParseFailure(err)
			return core.QueryTurnDecision{}, true, err
		}
		if normalized {
			emitQueryProgressFromContext(ctx, QueryProgressEvent{Type: "llm_output_normalized", Message: "检测到重复的相同 JSON，已按单个动作执行"})
		}
		if strings.TrimSpace(decision.Action.Action) == "" {
			err = fmt.Errorf("llm query turn missing action")
			repairInstruction = compactLLMParseFailure(err)
			return core.QueryTurnDecision{}, true, err
		}
		if err := validateQueryTurnDecision(input, decision); err != nil {
			repairInstruction = compactLLMParseFailure(err)
			return core.QueryTurnDecision{}, true, err
		}
		return decision, false, nil
	})
}

func validateQueryTurnDecision(input QueryActionInput, decision core.QueryTurnDecision) error {
	if input.Step != 1 || !decision.RequireAll || !strings.EqualFold(strings.TrimSpace(decision.ReasoningMode), "constraint_satisfaction") {
		return nil
	}
	if len(decision.Requirements) < 2 {
		return errors.New("constraint first turn must preserve all material requirements")
	}
	for _, requirement := range decision.Requirements {
		kind := strings.ToLower(strings.TrimSpace(requirement.Kind))
		if kind != "positive" && kind != "negative" {
			return fmt.Errorf("requirement %q must be classified as positive or negative", requirement.ID)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(decision.Action.Action), "discover_candidates") {
		return errors.New("constraint first turn must use discover_candidates before naming or testing candidates")
	}
	return nil
}

func (a OpenAICompatibleQueryAgent) VerifyQueryAnswerContext(ctx context.Context, input QueryVerificationInput) (core.QueryVerification, error) {
	docs := budgetVerificationReadDocuments(input.Docs, input.Action, 60000)
	var evidence strings.Builder
	for i, doc := range docs {
		fmt.Fprintf(&evidence, "\n---DOC %d---\npath: %s\nkind: %s\ntitle: %s\n\n%s\n", i+1, doc.Path, doc.Kind, doc.Title, doc.Content)
	}
	system := `You are an independent verifier for a persistent LLM Wiki answer.
Return only JSON matching:
{"accepted":true,"summary":"brief audit","unresolved":[],"contradictions":[],"next_queries":[]}

Verify only against the supplied read documents. Do not trust conversation suggestions or the proposing agent.
For coverage verification, confirm that every requirement is answered, each evidence path supports the stated subject and claim, and uncertainty is stated correctly.
For adversarial verification, actively seek subject mixups, counterexamples, alternative candidates, unsupported negatives, citation gaps, and conclusions that only partially match the question.
Reject whenever a required claim is unknown, contradicted, supported only by an unread path, or assigned to the wrong subject. next_queries should contain concise retrieval queries that could close the gaps.
A relational requirement is satisfied when the candidate is one of the relation's actual participants. In particular, "the candidate has a brotherhood/sworn-brother plot" is directly supported when evidence says the candidate and any other person became or bowed as brothers. Do not call this a subject mixup merely because a brotherhood necessarily has another participant; only reject when the candidate itself did not participate.
A negative requirement with status not_found_in_corpus is not a universal factual claim. Accept it when the trace shows candidate-specific corpus searches (or equivalent broad search/list/follow activity), no read evidence contradicts it, and the answer explicitly scopes the conclusion to the current corpus. Do not demand a source sentence that literally proves an absence.
The mere presence of a page about a place, a search result, a shared source, or a graph/navigation relationship is not evidence that the candidate visited that place. Treat it as a contradiction only when a read passage attributes the visit to that same candidate.`
	user := fmt.Sprintf(`Verification kind: %s
Question: %s
Plan: %s
Proposed final action: %s
Candidate-specific corpus search executed: %t
Tool trace: %s
Read evidence:
%s`, input.Kind, input.Question, mustJSON(input.Plan), mustJSON(input.Action), queryTraceContainsCandidateSearch(input.Trace, input.Action.Candidate), mustJSON(budgetTrace(input.Trace, 20)), evidence.String())
	retryOpts := a.retryOptions()
	return llmretry.DoValue[core.QueryVerification](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (core.QueryVerification, bool, error) {
		content, retryable, err := a.chatOnce(ctx, system, user)
		if err != nil {
			return core.QueryVerification{}, retryable, err
		}
		verification, err := decodeLLMJSONObject[core.QueryVerification](content, "query verification")
		if err != nil {
			return core.QueryVerification{}, true, err
		}
		return verification, false, nil
	})
}

func budgetVerificationReadDocuments(docs []QueryReadDocument, action core.QueryAction, budget int) []QueryReadDocument {
	priority := map[string]bool{}
	for _, check := range action.Checks {
		for _, path := range check.EvidencePaths {
			priority[path] = true
		}
	}
	ordered := make([]QueryReadDocument, 0, len(docs))
	for _, doc := range docs {
		if !priority[doc.Path] {
			ordered = append(ordered, doc)
		}
	}
	for _, doc := range docs {
		if priority[doc.Path] {
			ordered = append(ordered, doc)
		}
	}
	return budgetRecentReadDocuments(ordered, budget)
}

func (a OpenAICompatibleQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	return a.SynthesizeQueryContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) SynthesizeQueryContext(ctx context.Context, input QuerySynthesisInput) (string, error) {
	input = budgetQuerySynthesisInput(input, 72000)
	var docs strings.Builder
	for i, doc := range input.Docs {
		fmt.Fprintf(&docs, "\n---DOC %d---\npath: %s\nkind: %s\ntitle: %s\n\n%s\n", i+1, doc.Path, doc.Kind, doc.Title, doc.Content)
	}
	system := `You answer questions against a persistent LLM-maintained wiki.
Use the provided wiki/raw-source documents as evidence. Cite paths inline when making factual claims.
If the answer reveals a reusable synthesis, end with a short "Writeback candidate" note describing the wiki page that should be created or updated.`
	user := fmt.Sprintf(`Question:
%s

Conversation context:
%s

Query plan:
%s

Trace:
%s

Candidate documents:
%s`, input.Question, input.ConversationContext, mustJSON(input.Plan), mustJSON(input.Trace), docs.String())
	return a.chatContext(ctx, system, user)
}

func (a OpenAICompatibleQueryAgent) chat(system, user string) (string, error) {
	return a.chatContext(context.Background(), system, user)
}

func (a OpenAICompatibleQueryAgent) chatContext(ctx context.Context, system, user string) (string, error) {
	retryOpts := a.retryOptions()
	if retryOpts.MaxElapsed > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, retryOpts.MaxElapsed)
		defer cancel()
		retryOpts.MaxElapsed = 0
	}
	return llmretry.Do(ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (string, bool, error) {
		return a.chatOnce(ctx, system, user)
	})
}

func (a OpenAICompatibleQueryAgent) retryOptions() llmretry.Options {
	return llmretry.Normalize(a.RetryOptions)
}

func queryLLMRetryCallback(ctx context.Context) llmretry.OnRetry {
	progress, _ := ctx.Value(queryProgressContextKey{}).(QueryProgressFunc)
	if progress == nil {
		return nil
	}
	return func(info llmretry.RetryInfo) {
		progress(QueryProgressEvent{
			Type:        "llm_retrying",
			Message:     fmt.Sprintf("LLM 调用失败，%.1fs 后重试", info.Delay.Seconds()),
			Observation: fmt.Sprintf("attempt=%d retries=%d reason=%s", info.Attempt, info.Retries, info.Reason),
		})
	}
}

func emitQueryProgressFromContext(ctx context.Context, event QueryProgressEvent) {
	progress, _ := ctx.Value(queryProgressContextKey{}).(QueryProgressFunc)
	emitQueryProgress(progress, event)
}

func (a OpenAICompatibleQueryAgent) chatOnce(ctx context.Context, system, user string) (string, bool, error) {
	maxInputChars := a.MaxInputChars
	if maxInputChars <= 0 {
		maxInputChars = defaultLLMMaxInputChars
	}
	maxOutputTokens := a.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultLLMMaxOutputTokens
	}
	system, user = promptbudget.BudgetChatInput(system, user, maxInputChars)
	return (llmclient.Client{
		Protocol:         a.Protocol,
		BaseURL:          a.BaseURL,
		APIKey:           a.APIKey,
		Model:            a.Model,
		UserAgent:        a.UserAgent,
		AnthropicVersion: a.AnthropicVersion,
		HTTPClient:       a.Client,
	}).Chat(ctx, llmclient.ChatRequest{
		System: system, User: user, MaxTokens: maxOutputTokens,
		Temperature: 0.2, DisableThinking: a.DisableThinking,
	})
}

type chatCompletionRequest struct {
	Model              string         `json:"model"`
	Messages           []chatMessage  `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func extractJSONObject(content string) string {
	data, _, err := canonicalJSONObject(content)
	if err != nil {
		return strings.TrimSpace(content)
	}
	return string(data)
}

func decodeLLMJSONObject[T any](content string, label string) (T, error) {
	var value T
	data, _, err := canonicalJSONObject(content)
	if err != nil {
		return value, fmt.Errorf("parse %s: %w", label, err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("parse %s: %w", label, err)
	}
	return value, nil
}

func decodeQueryTurnDecision(content string) (core.QueryTurnDecision, error) {
	decision, _, err := decodeQueryTurnDecisionWithNormalization(content)
	return decision, err
}

func decodeQueryTurnDecisionWithNormalization(content string) (core.QueryTurnDecision, bool, error) {
	data, normalized, err := canonicalJSONObject(content)
	if err != nil {
		return core.QueryTurnDecision{}, false, fmt.Errorf("parse llm query action: %w", err)
	}
	// Keep the action endpoint backward compatible with providers and tests that
	// still return the former flat QueryAction shape. Production first turns use
	// the decision envelope, but a flat action remains a valid later-turn reply.
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return core.QueryTurnDecision{}, normalized, fmt.Errorf("parse llm query action: %w", err)
	}
	actionJSON := bytes.TrimSpace(shape["action"])
	if len(actionJSON) > 0 && actionJSON[0] == '{' {
		var decision core.QueryTurnDecision
		if err := json.Unmarshal(data, &decision); err != nil {
			return core.QueryTurnDecision{}, normalized, fmt.Errorf("parse llm query turn: %w", err)
		}
		return decision, normalized, nil
	}
	var action core.QueryAction
	if err := json.Unmarshal(data, &action); err != nil {
		return core.QueryTurnDecision{}, normalized, fmt.Errorf("parse llm query action: %w", err)
	}
	return core.QueryTurnDecision{Action: action}, normalized, nil
}

func canonicalJSONObject(content string) ([]byte, bool, error) {
	content = strings.TrimSpace(content)
	var values []json.RawMessage
	for {
		start := strings.Index(content, "{")
		if start < 0 {
			break
		}
		segment := content[start:]
		decoder := json.NewDecoder(strings.NewReader(segment))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, false, fmt.Errorf("invalid JSON stream: %w; output=%s", err, compactLLMOutput(content))
		}
		var shape any
		if err := json.Unmarshal(raw, &shape); err != nil {
			return nil, false, fmt.Errorf("invalid JSON value: %w", err)
		}
		if _, ok := shape.(map[string]any); !ok {
			return nil, false, errors.New("top-level JSON value must be an object")
		}
		values = append(values, raw)
		offset := int(decoder.InputOffset())
		if offset >= len(segment) {
			break
		}
		// Ignore commentary and markdown fences around a single JSON object, but
		// continue scanning when another object exists so identical duplicates can
		// be normalized and conflicting actions can still be rejected.
		content = segment[offset:]
	}
	if len(values) == 0 {
		return nil, false, errors.New("response contains no JSON object")
	}
	var first any
	if err := json.Unmarshal(values[0], &first); err != nil {
		return nil, false, err
	}
	for _, raw := range values[1:] {
		var next any
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, false, err
		}
		if !reflect.DeepEqual(first, next) {
			return nil, false, fmt.Errorf("response contains %d conflicting JSON objects", len(values))
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, values[0]); err != nil {
		return nil, false, err
	}
	return compact.Bytes(), len(values) > 1, nil
}

func compactLLMOutput(content string) string {
	return strings.ReplaceAll(promptbudget.TrimMiddle(strings.TrimSpace(content), 800), "\n", " ")
}

func compactLLMParseFailure(err error) string {
	if err == nil {
		return "previous response was invalid"
	}
	return promptbudget.TrimMiddle(err.Error(), 1000)
}

func mustJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

func budgetQueryPlanningInput(input QueryPlanningInput) QueryPlanningInput {
	input.Purpose = promptbudget.TrimEnd(input.Purpose, 6000)
	input.Schema = promptbudget.TrimEnd(input.Schema, 6000)
	input.Index = promptbudget.TrimMiddle(input.Index, 30000)
	input.Overview = promptbudget.TrimMiddle(input.Overview, 16000)
	input.LogTail = tailRunes(input.LogTail, 4000)
	return input
}

func budgetQueryActionInput(input QueryActionInput, docsBudget int) QueryActionInput {
	if input.Step == 1 && input.Plan.RequireAll {
		// The first constraint turn only routes and classifies already extracted
		// requirements. Candidate discovery receives the fuller navigation context
		// separately, so sending the whole catalog here only increases latency and
		// the likelihood of upstream gateway failures.
		input.Purpose = promptbudget.TrimEnd(input.Purpose, 3000)
		input.Schema = promptbudget.TrimEnd(input.Schema, 3000)
		input.Index = promptbudget.TrimMiddle(input.Index, 6000)
		input.Overview = promptbudget.TrimMiddle(input.Overview, 4000)
		input.LogTail = tailRunes(input.LogTail, 1000)
	} else {
		input.Purpose = promptbudget.TrimEnd(input.Purpose, 6000)
		input.Schema = promptbudget.TrimEnd(input.Schema, 6000)
		input.Index = promptbudget.TrimMiddle(input.Index, 30000)
		input.Overview = promptbudget.TrimMiddle(input.Overview, 16000)
		input.LogTail = tailRunes(input.LogTail, 4000)
	}
	input.Results = budgetQueryResults(input.Results, 12)
	input.Docs = budgetRecentReadDocuments(prioritizeQueryActionDocuments(input.Docs, input.Results, input.Trace, input.CandidateAssessments), docsBudget)
	input.Navigation = budgetNavigationObservations(input.Navigation, 6, 12)
	input.Trace = budgetTrace(input.Trace, 12)
	return input
}

func prioritizeQueryActionDocuments(docs []QueryReadDocument, results []core.QueryResult, trace []core.QueryTraceStep, assessments []core.QueryCandidateAssessment) []QueryReadDocument {
	if len(docs) == 0 {
		return docs
	}
	priority := map[string]bool{}
	if len(assessments) == 0 {
		for index, result := range results {
			if index >= 8 {
				break
			}
			if !isAggregateWikiPath(result.Path) {
				priority[normalizeQueryEvidencePath(result.Path)] = true
			}
		}
	} else if best := bestCandidateAssessment(assessments); best != nil {
		markCandidateEvidencePriority(priority, docs, best.Candidate, best.Checks)
	}
	for index := len(trace) - 1; index >= 0; index-- {
		action := trace[index].Action
		if strings.TrimSpace(action.Candidate) == "" && len(action.Checks) == 0 {
			continue
		}
		markCandidateEvidencePriority(priority, docs, action.Candidate, action.Checks)
		break
	}
	if len(priority) == 0 {
		return docs
	}
	ordered := make([]QueryReadDocument, 0, len(docs))
	for _, doc := range docs {
		if !priority[normalizeQueryEvidencePath(doc.Path)] {
			ordered = append(ordered, doc)
		}
	}
	for _, doc := range docs {
		if priority[normalizeQueryEvidencePath(doc.Path)] {
			ordered = append(ordered, doc)
		}
	}
	return ordered
}

func markCandidateEvidencePriority(priority map[string]bool, docs []QueryReadDocument, candidate string, checks []core.QueryEvidenceCheck) {
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	for _, doc := range docs {
		if candidate == "" || (!strings.Contains(strings.ToLower(doc.Title), candidate) && !strings.Contains(strings.ToLower(doc.Path), candidate)) {
			continue
		}
		priority[normalizeQueryEvidencePath(doc.Path)] = true
		page := wiki.ParseWikiPage("query", doc.Path, doc.Content)
		for _, source := range page.Sources {
			priority[normalizeQueryEvidencePath(source)] = true
		}
	}
	for _, check := range checks {
		for _, path := range check.EvidencePaths {
			if !isAggregateWikiPath(path) {
				priority[normalizeQueryEvidencePath(path)] = true
			}
		}
	}
}

func queryReadPathInventory(docs []QueryReadDocument) string {
	if len(docs) == 0 {
		return "(none)"
	}
	var out strings.Builder
	seen := map[string]bool{}
	for _, doc := range docs {
		path := strings.TrimSpace(doc.Path)
		key := normalizeQueryEvidencePath(path)
		if path == "" || seen[key] {
			continue
		}
		seen[key] = true
		label := "evidence-eligible"
		if isAggregateWikiPath(path) {
			label = "navigation-only"
		}
		fmt.Fprintf(&out, "- %s (%s)\n", path, label)
	}
	return strings.TrimSpace(out.String())
}

func budgetRecentReadDocuments(docs []QueryReadDocument, totalContentRunes int) []QueryReadDocument {
	if len(docs) == 0 || totalContentRunes <= 0 {
		return nil
	}
	selected := make([]QueryReadDocument, 0, len(docs))
	remaining := totalContentRunes
	for index := len(docs) - 1; index >= 0 && remaining > 0; index-- {
		doc := docs[index]
		perDoc := remaining
		if perDoc > 6000 {
			perDoc = 6000
		}
		original := len([]rune(doc.Content))
		doc.Content = promptbudget.TrimMiddle(doc.Content, perDoc)
		remaining -= len([]rune(doc.Content))
		if note := promptbudget.Annotation(original, len([]rune(doc.Content))); note != "" {
			doc.Content += "\n\n[" + note + "]"
		}
		selected = append(selected, doc)
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected
}

func budgetQuerySynthesisInput(input QuerySynthesisInput, docsBudget int) QuerySynthesisInput {
	input.Results = budgetQueryResults(input.Results, 12)
	input.Docs = budgetReadDocuments(input.Docs, docsBudget)
	input.Trace = budgetTrace(input.Trace, 16)
	return input
}

func budgetQueryResults(results []core.QueryResult, limit int) []core.QueryResult {
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	out := make([]core.QueryResult, 0, len(results))
	for _, result := range results {
		result.Snippet = promptbudget.TrimEnd(result.Snippet, 600)
		out = append(out, result)
	}
	return out
}

func budgetReadDocuments(docs []QueryReadDocument, totalContentRunes int) []QueryReadDocument {
	if len(docs) == 0 {
		return docs
	}
	out := make([]QueryReadDocument, 0, len(docs))
	remaining := totalContentRunes
	for _, doc := range docs {
		if remaining <= 0 {
			doc.Content = ""
			out = append(out, doc)
			continue
		}
		perDoc := remaining
		if perDoc > 6000 {
			perDoc = 6000
		}
		original := len([]rune(doc.Content))
		doc.Content = promptbudget.TrimMiddle(doc.Content, perDoc)
		remaining -= len([]rune(doc.Content))
		if note := promptbudget.Annotation(original, len([]rune(doc.Content))); note != "" {
			doc.Content += "\n\n[" + note + "]"
		}
		out = append(out, doc)
	}
	return out
}

func budgetNavigationObservations(observations []QueryNavigationObservation, observationLimit, pagesLimit int) []QueryNavigationObservation {
	if observationLimit > 0 && len(observations) > observationLimit {
		observations = observations[len(observations)-observationLimit:]
	}
	out := make([]QueryNavigationObservation, 0, len(observations))
	for _, obs := range observations {
		if pagesLimit > 0 && len(obs.Pages) > pagesLimit {
			obs.Pages = obs.Pages[:pagesLimit]
		}
		if len(obs.Skipped) > 10 {
			obs.Skipped = obs.Skipped[:10]
		}
		out = append(out, obs)
	}
	return out
}

func budgetTrace(trace []core.QueryTraceStep, limit int) []core.QueryTraceStep {
	if limit > 0 && len(trace) > limit {
		trace = trace[len(trace)-limit:]
	}
	out := make([]core.QueryTraceStep, 0, len(trace))
	for _, step := range trace {
		step.Observation = promptbudget.TrimEnd(step.Observation, 1000)
		if len([]rune(step.Action.Answer)) > 2000 {
			step.Action.Answer = promptbudget.TrimEnd(step.Action.Answer, 2000)
		}
		if len([]rune(step.Action.Rationale)) > 1000 {
			step.Action.Rationale = promptbudget.TrimEnd(step.Action.Rationale, 1000)
		}
		out = append(out, step)
	}
	return out
}
