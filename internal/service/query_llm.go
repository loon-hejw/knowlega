package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/llmclient"
	"github.com/hejw/knowledge-core/internal/llmretry"
	"github.com/hejw/knowledge-core/internal/promptbudget"
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
			Retries:   cfg.Retries,
			BaseDelay: cfg.RetryBaseDelay.Duration,
			MaxDelay:  cfg.RetryMaxDelay.Duration,
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
Intents:
- direct_chat: short greetings, thanks, or simple social chat such as "你好", "hello", "在吗", "谢谢". Do not read or search the wiki. Set can_write_back=false.
- answer_from_persistent_wiki: questions that need business/wiki/code/source evidence. These must read wiki/raw/graph evidence before final answers.
- missing_evidence: questions that clearly ask for unavailable knowledge or require sources that are not present. Set can_write_back=false.
Return only JSON with this shape:
{"intent":"answer_from_persistent_wiki","read_first":["wiki/index.md"],"searches":[{"text":"...","weight":6,"rationale":"..."}],"candidate_limit":10,"answer_mode":"llm_synthesis","can_write_back":true}`
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

func (a OpenAICompatibleQueryAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
	return a.NextQueryActionContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) NextQueryActionContext(ctx context.Context, input QueryActionInput) (core.QueryAction, error) {
	input = budgetQueryActionInput(input, 52000)
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
	system := `You are operating a persistent LLM Wiki through tools.
Choose exactly one next action and return only JSON.

Actions:
- {"action":"read","path":"wiki/... or raw/sources/... or exact wiki title/link/alias","rationale":"why this evidence is needed"}
- {"action":"list_pages","query":"optional title/path/type terms","limit":20,"rationale":"why wiki navigation is needed"}
- {"action":"follow_links","path":"wiki/...","limit":5,"rationale":"why linked wiki pages should be inspected"}
- {"action":"search","query":"search terms","limit":5,"rationale":"why recall is needed"}
- {"action":"graph","query":"symbol, file, relation, route, or concept","limit":5,"rationale":"why graph evidence is needed"}
- {"action":"final","answer":"final cited answer","rationale":"why enough evidence has been read"}
- {"action":"writeback","title":"short synthesis page title","answer":"final cited answer","rationale":"why this synthesis should be saved"}

Rules:
- Prefer wiki navigation before broad search: read wiki/index.md, list_pages, follow_links from relevant pages, then search only when navigation is insufficient.
- list_pages is navigation only, not factual evidence for final answers.
- follow_links reads linked wiki pages and can provide final-answer evidence.
- Search is candidate recall only; use read after search before making factual claims.
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

Step: %d

Trace:
%s

Known search results:
%s

Navigation observations:
%s

	Read documents:
%s`, input.Question, input.ConversationContext, mustJSON(input.Plan), input.Step, mustJSON(input.Trace), results.String(), navigation.String(), docs.String())
	retryOpts := a.retryOptions()
	return llmretry.DoValue[core.QueryAction](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (core.QueryAction, bool, error) {
		content, retryable, err := a.chatOnce(ctx, system, user)
		if err != nil {
			return core.QueryAction{}, retryable, err
		}
		action, err := decodeLLMJSONObject[core.QueryAction](content, "llm query action")
		if err != nil {
			return core.QueryAction{}, true, err
		}
		if strings.TrimSpace(action.Action) == "" {
			return core.QueryAction{}, true, fmt.Errorf("llm query action missing action: %s", content)
		}
		return action, false, nil
	})
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
	content = strings.TrimSpace(content)
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end >= start {
		return content[start : end+1]
	}
	return content
}

func decodeLLMJSONObject[T any](content string, label string) (T, error) {
	var value T
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &value); err != nil {
		return value, fmt.Errorf("parse %s: %w: %s", label, err, content)
	}
	return value, nil
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
	input.Results = budgetQueryResults(input.Results, 12)
	input.Docs = budgetReadDocuments(input.Docs, docsBudget)
	input.Navigation = budgetNavigationObservations(input.Navigation, 6, 12)
	input.Trace = budgetTrace(input.Trace, 12)
	return input
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
