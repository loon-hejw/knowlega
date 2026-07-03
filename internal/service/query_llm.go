package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
)

type OpenAICompatibleQueryAgent struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewEnvQueryAgent() (QueryAgent, bool, error) {
	apiKey := config.Value("KB_CORE_LLM_API_KEY", "OPENAI_API_KEY")
	model := config.Value("KB_CORE_LLM_MODEL", "OPENAI_MODEL")
	if apiKey == "" || model == "" {
		return nil, false, nil
	}
	baseURL := config.Value("KB_CORE_LLM_BASE_URL", "OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return OpenAICompatibleQueryAgent{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}, true, nil
}

func (a OpenAICompatibleQueryAgent) PlanQuery(input QueryPlanningInput) (core.QueryPlan, error) {
	system := `You are the query planner for an LLM-maintained persistent wiki.
Do not answer the question yet. Read the wiki navigation context and produce a JSON query plan.
Searches are candidate-recall tool calls only. Expand aliases, entities, chapter names, symbols, and graph terms when justified by the wiki context.
Return only JSON with this shape:
{"intent":"answer_from_persistent_wiki","read_first":["wiki/index.md"],"searches":[{"text":"...","weight":6,"rationale":"..."}],"candidate_limit":10,"answer_mode":"llm_synthesis","can_write_back":true}`
	user := fmt.Sprintf(`Question:
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
%s`, input.Question, input.Purpose, input.Schema, input.Index, input.Overview, input.LogTail)
	content, err := a.chat(system, user)
	if err != nil {
		return core.QueryPlan{}, err
	}
	var plan core.QueryPlan
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &plan); err != nil {
		return core.QueryPlan{}, fmt.Errorf("parse llm query plan: %w: %s", err, content)
	}
	if plan.Question == "" {
		plan.Question = input.Question
	}
	if plan.Intent == "" {
		plan.Intent = "answer_from_persistent_wiki"
	}
	if plan.CandidateLimit <= 0 {
		plan.CandidateLimit = 10
	}
	if len(plan.ReadFirst) == 0 {
		plan.ReadFirst = []string{"wiki/index.md"}
	}
	if plan.AnswerMode == "" {
		plan.AnswerMode = "llm_synthesis"
	}
	return plan, nil
}

func (a OpenAICompatibleQueryAgent) NextQueryAction(input QueryActionInput) (core.QueryAction, error) {
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
%s`, input.Question, mustJSON(input.Plan), input.Step, mustJSON(input.Trace), results.String(), navigation.String(), docs.String())
	content, err := a.chat(system, user)
	if err != nil {
		return core.QueryAction{}, err
	}
	var action core.QueryAction
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &action); err != nil {
		return core.QueryAction{}, fmt.Errorf("parse llm query action: %w: %s", err, content)
	}
	if action.Action == "" {
		return core.QueryAction{}, fmt.Errorf("llm query action missing action: %s", content)
	}
	return action, nil
}

func (a OpenAICompatibleQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	var docs strings.Builder
	for i, doc := range input.Docs {
		fmt.Fprintf(&docs, "\n---DOC %d---\npath: %s\nkind: %s\ntitle: %s\n\n%s\n", i+1, doc.Path, doc.Kind, doc.Title, doc.Content)
	}
	system := `You answer questions against a persistent LLM-maintained wiki.
Use the provided wiki/raw-source documents as evidence. Cite paths inline when making factual claims.
If the answer reveals a reusable synthesis, end with a short "Writeback candidate" note describing the wiki page that should be created or updated.`
	user := fmt.Sprintf(`Question:
%s

Query plan:
%s

Trace:
%s

Candidate documents:
%s`, input.Question, mustJSON(input.Plan), mustJSON(input.Trace), docs.String())
	return a.chat(system, user)
}

func (a OpenAICompatibleQueryAgent) chat(system, user string) (string, error) {
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	payload := chatCompletionRequest{
		Model: a.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.2,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, a.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm request failed: status=%d body=%s", resp.StatusCode, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
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

func mustJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}
