package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hejw/knowledge-core/internal/llmretry"
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
	}
	if input.DisableThinking {
		// Several OpenAI-compatible reasoning gateways otherwise spend the
		// output budget in reasoning_content and return content=null.
		payload.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
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
		return "", true, emptyContentError(c.Model, data)
	}
	return content, false, nil
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
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
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
