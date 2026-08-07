package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type QueryAgent interface {
	Answer(context.Context, QueryPrompt) (QueryDraft, error)
}

type QueryPlanner interface {
	Plan(context.Context, QueryPlanningPrompt) ([]QueryAction, error)
}

type CompilerAgent interface {
	Compile(context.Context, CompilePrompt) (CompileDraft, error)
}

type QueryPrompt struct {
	Question            string
	ConversationContext string
	Documents           []Document
}

type QueryPlanningPrompt struct {
	Question            string
	ConversationContext string
	Navigation          []Document
}

type QueryAction struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Query string `json:"query"`
}

type QueryDraft struct {
	Answer                  string   `json:"answer"`
	CitationPaths           []string `json:"citations"`
	SuggestedWritebackTitle string   `json:"suggested_writeback_title"`
	IncompleteReason        string   `json:"incomplete_reason"`
}

type CompilePrompt struct {
	SourcePath string
	SourceText string
	Documents  []Document
}

type GeneratedPage struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type CompileDraft struct {
	Pages   []GeneratedPage `json:"pages"`
	Reviews []string        `json:"reviews"`
}

type OpenAICompatibleQueryAgent struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewOpenAICompatibleQueryAgent(baseURL, apiKey, model string, timeout time.Duration) (*OpenAICompatibleQueryAgent, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("knowledge LLM base_url, api_key, and model are required")
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	return &OpenAICompatibleQueryAgent{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, client: &http.Client{Timeout: timeout}}, nil
}

func (a *OpenAICompatibleQueryAgent) Answer(ctx context.Context, prompt QueryPrompt) (QueryDraft, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	request := struct {
		Model          string    `json:"model"`
		Messages       []message `json:"messages"`
		ResponseFormat any       `json:"response_format"`
	}{
		Model:          a.model,
		Messages:       []message{{Role: "system", Content: "You maintain a persistent Markdown LLM Wiki. Answer only from the supplied read documents. Return JSON with answer, citations, suggested_writeback_title, and incomplete_reason. citations must contain only supplied document paths. If evidence is insufficient, say so in incomplete_reason."}, {Role: "user", Content: renderQueryPrompt(prompt)}},
		ResponseFormat: map[string]string{"type": "json_object"},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return QueryDraft{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return QueryDraft{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+a.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return QueryDraft{}, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return QueryDraft{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return QueryDraft{}, fmt.Errorf("knowledge LLM returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(contents, &envelope); err != nil || len(envelope.Choices) == 0 {
		return QueryDraft{}, errors.New("knowledge LLM returned no completion")
	}
	var draft QueryDraft
	if err := json.Unmarshal([]byte(envelope.Choices[0].Message.Content), &draft); err != nil {
		return QueryDraft{}, fmt.Errorf("knowledge LLM returned invalid query JSON: %w", err)
	}
	if strings.TrimSpace(draft.Answer) == "" {
		return QueryDraft{}, errors.New("knowledge LLM returned an empty answer")
	}
	return draft, nil
}

func (a *OpenAICompatibleQueryAgent) Plan(ctx context.Context, prompt QueryPlanningPrompt) ([]QueryAction, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	request := struct {
		Model          string    `json:"model"`
		Messages       []message `json:"messages"`
		ResponseFormat any       `json:"response_format"`
	}{
		Model:          a.model,
		Messages:       []message{{Role: "system", Content: "Plan a bounded LLM Wiki query. Return JSON {actions:[...]}. Allowed action kinds: read with a supplied wiki/raw path, search with a query, final. Read navigation is already complete. Use search only for candidate recall; prefer direct read when navigation identifies a page. Use at most six actions and finish with final."}, {Role: "user", Content: renderPlanningPrompt(prompt)}},
		ResponseFormat: map[string]string{"type": "json_object"},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+a.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("knowledge LLM returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(contents, &envelope); err != nil || len(envelope.Choices) == 0 {
		return nil, errors.New("knowledge LLM returned no query plan")
	}
	var plan struct {
		Actions []QueryAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(envelope.Choices[0].Message.Content), &plan); err != nil {
		return nil, fmt.Errorf("knowledge LLM returned invalid query plan JSON: %w", err)
	}
	return plan.Actions, nil
}

func (a *OpenAICompatibleQueryAgent) Compile(ctx context.Context, prompt CompilePrompt) (CompileDraft, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	request := struct {
		Model          string    `json:"model"`
		Messages       []message `json:"messages"`
		ResponseFormat any       `json:"response_format"`
	}{
		Model:          a.model,
		Messages:       []message{{Role: "system", Content: "You compile immutable source material into a persistent Markdown LLM Wiki. Return JSON with pages and reviews. Every page must be Markdown under wiki/, start with YAML frontmatter, include sources containing the supplied raw source path, and use [[wikilinks]] in the body when relevant. Include updates for wiki/index.md, wiki/overview.md, and wiki/log.md plus a source summary under wiki/sources/. Preserve evidence and do not invent unsupported facts."}, {Role: "user", Content: renderCompilePrompt(prompt)}},
		ResponseFormat: map[string]string{"type": "json_object"},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return CompileDraft{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompileDraft{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+a.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return CompileDraft{}, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return CompileDraft{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return CompileDraft{}, fmt.Errorf("knowledge LLM returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(contents, &envelope); err != nil || len(envelope.Choices) == 0 {
		return CompileDraft{}, errors.New("knowledge LLM returned no compilation")
	}
	var draft CompileDraft
	if err := json.Unmarshal([]byte(envelope.Choices[0].Message.Content), &draft); err != nil {
		return CompileDraft{}, fmt.Errorf("knowledge LLM returned invalid compilation JSON: %w", err)
	}
	return draft, nil
}

func renderQueryPrompt(prompt QueryPrompt) string {
	var builder strings.Builder
	builder.WriteString("Question:\n")
	builder.WriteString(prompt.Question)
	if strings.TrimSpace(prompt.ConversationContext) != "" {
		builder.WriteString("\n\nConversation context:\n")
		builder.WriteString(prompt.ConversationContext)
	}
	builder.WriteString("\n\nRead documents:\n")
	for _, document := range prompt.Documents {
		builder.WriteString("\n--- ")
		builder.WriteString(document.Path)
		builder.WriteString(" (" + document.Kind + ") ---\n")
		builder.WriteString(document.Content)
	}
	return builder.String()
}

func renderPlanningPrompt(prompt QueryPlanningPrompt) string {
	var builder strings.Builder
	builder.WriteString("Question:\n")
	builder.WriteString(prompt.Question)
	if strings.TrimSpace(prompt.ConversationContext) != "" {
		builder.WriteString("\n\nConversation context:\n")
		builder.WriteString(prompt.ConversationContext)
	}
	builder.WriteString("\n\nAlready-read navigation:\n")
	for _, document := range prompt.Navigation {
		builder.WriteString("\n--- ")
		builder.WriteString(document.Path)
		builder.WriteString(" ---\n")
		builder.WriteString(document.Content)
	}
	return builder.String()
}

func renderCompilePrompt(prompt CompilePrompt) string {
	var builder strings.Builder
	builder.WriteString("Raw source path: ")
	builder.WriteString(prompt.SourcePath)
	builder.WriteString("\n\nRaw source:\n")
	builder.WriteString(prompt.SourceText)
	builder.WriteString("\n\nCurrent navigation documents:\n")
	for _, document := range prompt.Documents {
		builder.WriteString("\n--- ")
		builder.WriteString(document.Path)
		builder.WriteString(" ---\n")
		builder.WriteString(document.Content)
	}
	return builder.String()
}
