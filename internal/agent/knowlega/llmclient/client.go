package llmclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
)

type concurrencyLimiter struct {
	slots chan struct{}
}

var globalConcurrency atomic.Pointer[concurrencyLimiter]

func init() {
	SetGlobalConcurrency(4)
}

// SetGlobalConcurrency applies one process-wide limit to compiler, query,
// review, graph enrichment and every other LLM client using this package.
func SetGlobalConcurrency(limit int) {
	if limit < 1 {
		limit = 1
	}
	globalConcurrency.Store(&concurrencyLimiter{slots: make(chan struct{}, limit)})
}

func acquireGlobal(ctx context.Context) (func(), error) {
	limiter := globalConcurrency.Load()
	if limiter == nil {
		return func() {}, nil
	}
	select {
	case limiter.slots <- struct{}{}:
		return func() { <-limiter.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

const (
	ProtocolOpenAI          = "openai"
	ProtocolAnthropic       = "anthropic"
	DefaultAnthropicVersion = "2023-06-01"
	DefaultUserAgent        = "knowledge-core/0.1"
)

// Client performs one LLM request using either the OpenAI-compatible chat
// completions protocol or the Anthropic Messages protocol. Retry policy stays
// with the caller because query operations need to surface retry progress.
type Client struct {
	BaseURL          string
	APIKey           string
	Model            string
	UserAgent        string
	Protocol         string
	AnthropicVersion string
	HTTPClient       *http.Client
}

type ChatRequest struct {
	System          string
	User            string
	MaxTokens       int
	Temperature     float64
	DisableThinking bool
	Stream          bool
}

func (c Client) Chat(ctx context.Context, input ChatRequest) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if input.MaxTokens <= 0 {
		return "", false, fmt.Errorf("llm max tokens must be positive")
	}
	release, err := acquireGlobal(ctx)
	if err != nil {
		return "", false, err
	}
	defer release()
	switch normalizeProtocol(c.Protocol) {
	case ProtocolOpenAI:
		return c.chatOpenAI(ctx, input)
	case ProtocolAnthropic:
		return c.chatAnthropic(ctx, input)
	default:
		return "", false, fmt.Errorf("unsupported llm protocol %q", c.Protocol)
	}
}

func normalizeProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		return ProtocolOpenAI
	}
	return protocol
}

func (c Client) chatOpenAI(ctx context.Context, input ChatRequest) (string, bool, error) {
	payload := openAIRequest{
		Model: c.Model,
		Messages: []message{
			{Role: "system", Content: input.System},
			{Role: "user", Content: input.User},
		},
		Temperature: input.Temperature,
		MaxTokens:   input.MaxTokens,
		Stream:      input.Stream,
	}
	if input.DisableThinking {
		// Several OpenAI-compatible reasoning gateways otherwise spend the
		// output budget in reasoning_content and return content=null.
		disabled := false
		payload.EnableThinking = &disabled
		payload.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
		payload.ReasoningEffort = "low"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	if input.Stream {
		return c.chatOpenAIStream(ctx, body)
	}
	data, status, retryable, err := c.do(ctx, "/chat/completions", body, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	})
	if err != nil {
		return "", retryable, err
	}
	if status < 200 || status >= 300 {
		return "", retryableStatusResponse(status, data), requestStatusError(status, data)
	}
	return parseOpenAIChatResponse(c.Model, data)
}

func (c Client) chatOpenAIStream(ctx context.Context, body []byte) (string, bool, error) {
	requestURL := protocolEndpoint(c.BaseURL, "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	userAgent := strings.TrimSpace(c.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", true, readErr
		}
		return "", retryableStatusResponse(resp.StatusCode, data), requestStatusError(resp.StatusCode, data)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", true, readErr
		}
		return parseOpenAIChatResponse(c.Model, data)
	}
	var content strings.Builder
	var reasoningFallback strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent json.RawMessage `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return "", true, fmt.Errorf("decode streaming llm response: %w: %s", err, responseSnippet([]byte(data)))
		}
		for _, choice := range event.Choices {
			content.WriteString(openAIContentDelta(choice.Delta.Content))
			reasoningFallback.WriteString(openAIContentDelta(choice.Delta.ReasoningContent))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", true, err
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		// Some compatible reasoning gateways ignore enable_thinking=false and
		// place their structured JSON exclusively in reasoning_content. Keep
		// this as an internal parser fallback; it is never surfaced as UI
		// chain-of-thought.
		result = strings.TrimSpace(reasoningFallback.String())
	}
	if result == "" {
		return "", true, emptyContentError(c.Model, nil)
	}
	return result, false, nil
}

func parseOpenAIChatResponse(model string, data []byte) (string, bool, error) {
	var parsed openAIResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", true, fmt.Errorf("decode llm response: %w: %s", err, responseSnippet(data))
	}
	if len(parsed.Choices) == 0 {
		return "", true, fmt.Errorf("llm returned no choices")
	}
	content := openAIContentText(parsed.Choices[0].Message.Content)
	if content == "" {
		content = strings.TrimSpace(parsed.Choices[0].Text)
	}
	if content == "" {
		// A few gateways return an ordinary JSON response even when stream=true
		// and ignore both supported thinking-disable hints. Match the streaming
		// parser's internal fallback so structured maintenance output is not
		// discarded solely because it arrived in reasoning_content.
		content = openAIContentText(parsed.Choices[0].Message.ReasoningContent)
	}
	if content == "" {
		return "", true, emptyContentError(model, data)
	}
	return content, false, nil
}

func openAIContentDelta(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var content strings.Builder
	for _, part := range parts {
		if part.Type == "" || part.Type == "text" || part.Type == "output_text" {
			content.WriteString(part.Text)
		}
	}
	return content.String()
}

func (c Client) chatAnthropic(ctx context.Context, input ChatRequest) (string, bool, error) {
	payload := anthropicRequest{
		Model:       c.Model,
		System:      input.System,
		Messages:    []message{{Role: "user", Content: input.User}},
		Temperature: input.Temperature,
		MaxTokens:   input.MaxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	version := strings.TrimSpace(c.AnthropicVersion)
	if version == "" {
		version = DefaultAnthropicVersion
	}
	data, status, retryable, err := c.do(ctx, "/messages", body, func(req *http.Request) {
		req.Header.Set("x-api-key", c.APIKey)
		// Anthropic's native API uses x-api-key. Some OpenAI-origin gateways
		// expose the Messages protocol but keep Bearer authentication, so send
		// both representations of the same configured credential.
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("anthropic-version", version)
	})
	if err != nil {
		return "", retryable, err
	}
	if status < 200 || status >= 300 {
		return "", retryableStatusResponse(status, data), requestStatusError(status, data)
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", true, fmt.Errorf("decode llm response: %w: %s", err, responseSnippet(data))
	}
	var textBlocks []string
	for _, block := range parsed.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			textBlocks = append(textBlocks, strings.TrimSpace(block.Text))
		}
	}
	content := strings.TrimSpace(strings.Join(textBlocks, "\n"))
	if content == "" {
		return "", true, emptyContentError(c.Model, data)
	}
	return content, false, nil
}

func retryableStatusResponse(status int, data []byte) bool {
	if llmretry.RetryableStatus(status) {
		return true
	}
	// Some compatible gateways multiplex Codex-backed ChatGPT accounts with
	// ordinary API backends. A routed account can reject a model that another
	// account in the same gateway serves successfully. Let the provider retry
	// this specific account-routing response before escalating it to the source
	// scheduler; ordinary 400 configuration errors remain non-retryable.
	message := strings.ToLower(string(data))
	return status == http.StatusBadRequest && strings.Contains(message, "model is not supported when using codex with a chatgpt account")
}

func (c Client) do(ctx context.Context, endpoint string, body []byte, setHeaders func(*http.Request)) ([]byte, int, bool, error) {
	requestURL := protocolEndpoint(c.BaseURL, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	userAgent := strings.TrimSpace(c.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	setHeaders(req)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, true, err
	}
	return data, resp.StatusCode, false, nil
}

// protocolEndpoint accepts a provider root URL, a /v1 base URL, or the full
// operation URL. This keeps common Anthropic configuration such as
// https://api.anthropic.com from accidentally targeting versionless /messages.
func protocolEndpoint(baseURL, endpoint string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, endpoint) {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + endpoint
	}
	if endpoint == "/messages" {
		return baseURL + "/v1" + endpoint
	}
	// Preserve the long-standing OpenAI-compatible behavior for gateways
	// that expose /chat/completions directly at their configured root.
	return baseURL + endpoint
}

func requestStatusError(status int, data []byte) error {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &parsed)
	message := strings.TrimSpace(parsed.Error.Message)
	if message == "" {
		message = responseSnippet(data)
	}
	return fmt.Errorf("llm request failed: status=%d body=%s", status, message)
}

func emptyContentError(model string, data []byte) error {
	return fmt.Errorf("llm returned empty content (model=%s; response=%s)", model, responseSnippet(data))
}

func responseSnippet(data []byte) string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "<empty>"
	}
	runes := []rune(text)
	if len(runes) > 240 {
		return string(runes[:240]) + "..."
	}
	return text
}

func openAIContentText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if (part.Type == "" || part.Type == "text" || part.Type == "output_text") && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, strings.TrimSpace(part.Text))
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model              string         `json:"model"`
	Messages           []message      `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens"`
	EnableThinking     *bool          `json:"enable_thinking,omitempty"`
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	Stream             bool           `json:"stream,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
}

type anthropicRequest struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}
