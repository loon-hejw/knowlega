package llmclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestGlobalConcurrencyLimitsIndependentClients(t *testing.T) {
	SetGlobalConcurrency(2)
	defer SetGlobalConcurrency(4)
	block := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		<-block
		active.Add(-1)
		return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	})
	client := Client{BaseURL: "https://models.example/v1", APIKey: "key", Model: "model", HTTPClient: &http.Client{Transport: transport}}
	var wg sync.WaitGroup
	for index := 0; index < 4; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = client.Chat(context.Background(), ChatRequest{MaxTokens: 1})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if got := maximum.Load(); got != 2 {
		t.Fatalf("global max=%d want 2", got)
	}
	close(block)
	wg.Wait()
}

func TestClientUsesOpenAIChatCompletionsProtocol(t *testing.T) {
	var requestBody map[string]any
	client := Client{
		BaseURL:   "https://models.example/v1/",
		APIKey:    "openai-key",
		Model:     "test-model",
		UserAgent: "custom-openai-client/9.9",
		Protocol:  ProtocolOpenAI,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://models.example/v1/chat/completions" {
				t.Fatalf("url=%s", req.URL)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer openai-key" {
				t.Fatalf("authorization=%q", got)
			}
			if got := req.Header.Get("User-Agent"); got != "custom-openai-client/9.9" {
				t.Fatalf("user-agent=%q", got)
			}
			if got := req.Header.Get("x-api-key"); got != "" {
				t.Fatalf("unexpected Anthropic key header=%q", got)
			}
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatal(err)
			}
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":[{"type":"text","text":"first"},{"type":"output_text","text":"second"}]}}]}`), nil
		})},
	}
	content, retryable, err := client.Chat(context.Background(), ChatRequest{
		System: "system prompt", User: "user prompt", MaxTokens: 123,
		Temperature: 0.2, DisableThinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryable || content != "first\nsecond" {
		t.Fatalf("content=%q retryable=%v", content, retryable)
	}
	if requestBody["model"] != "test-model" || int(requestBody["max_tokens"].(float64)) != 123 {
		t.Fatalf("request=%+v", requestBody)
	}
	messages := requestBody["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "user prompt" {
		t.Fatalf("messages=%+v", messages)
	}
	thinking := requestBody["chat_template_kwargs"].(map[string]any)
	if requestBody["enable_thinking"] != false || requestBody["reasoning_effort"] != "low" || thinking["enable_thinking"] != false {
		t.Fatalf("thinking=%+v", thinking)
	}
}

func TestClientUsesNonStreamingReasoningContentAsInternalFallback(t *testing.T) {
	client := Client{
		BaseURL: "https://models.example/v1", APIKey: "key", Model: "k2.6", Protocol: ProtocolOpenAI,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":null,"reasoning_content":"{\"issues\":[]}"}}]}`), nil
		})},
	}
	content, retryable, err := client.Chat(t.Context(), ChatRequest{System: "s", User: "u", MaxTokens: 32})
	if err != nil || retryable || content != `{"issues":[]}` {
		t.Fatalf("content=%q retryable=%v err=%v", content, retryable, err)
	}
}

func TestClientStreamsOpenAICompatibleStructuredContent(t *testing.T) {
	var requestBody map[string]any
	client := Client{
		BaseURL: "https://models.example/v1", APIKey: "key", Model: "Qwen3.6-27B-FP8", Protocol: ProtocolOpenAI,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatal(err)
			}
			body := "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"action\\\":\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"\\\"search\\\"}\"}}]}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	content, retryable, err := client.Chat(t.Context(), ChatRequest{System: "s", User: "u", MaxTokens: 32, Stream: true})
	if err != nil || retryable || content != `{"action":"search"}` {
		t.Fatalf("content=%q retryable=%v err=%v", content, retryable, err)
	}
	if requestBody["stream"] != true {
		t.Fatalf("request=%+v", requestBody)
	}
}

func TestClientUsesStreamingReasoningContentOnlyAsStructuredFallback(t *testing.T) {
	client := Client{
		BaseURL: "https://models.example/v1", APIKey: "key", Model: "k2.6", Protocol: ProtocolOpenAI,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"{\\\"action\\\":\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"\\\"search\\\"}\"}}]}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	content, retryable, err := client.Chat(t.Context(), ChatRequest{System: "s", User: "u", MaxTokens: 32, Stream: true})
	if err != nil || retryable || content != `{"action":"search"}` {
		t.Fatalf("content=%q retryable=%v err=%v", content, retryable, err)
	}
}

func TestClientRetriesCodexAccountModelRoutingResponse(t *testing.T) {
	client := Client{
		BaseURL: "https://models.example/v1", APIKey: "key", Model: "gpt-5.4",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusBadRequest, `{"error":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`), nil
		})},
	}
	_, retryable, err := client.Chat(context.Background(), ChatRequest{MaxTokens: 1})
	if err == nil || !retryable {
		t.Fatalf("err=%v retryable=%v; account routing rejection must be retryable", err, retryable)
	}
}

func TestClientUsesAnthropicMessagesProtocol(t *testing.T) {
	var requestBody map[string]any
	client := Client{
		BaseURL:          "https://anthropic.example",
		APIKey:           "anthropic-key",
		Model:            "claude-test",
		UserAgent:        "claude-cli/2.1.205 (external, cli)",
		Protocol:         ProtocolAnthropic,
		AnthropicVersion: "2024-01-01",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://anthropic.example/v1/messages" {
				t.Fatalf("url=%s", req.URL)
			}
			if got := req.Header.Get("x-api-key"); got != "anthropic-key" {
				t.Fatalf("x-api-key=%q", got)
			}
			if got := req.Header.Get("anthropic-version"); got != "2024-01-01" {
				t.Fatalf("anthropic-version=%q", got)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer anthropic-key" {
				t.Fatalf("Authorization=%q", got)
			}
			if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.205 (external, cli)" {
				t.Fatalf("user-agent=%q", got)
			}
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatal(err)
			}
			return jsonResponse(http.StatusOK, `{"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"wiki answer"}]}`), nil
		})},
	}
	content, retryable, err := client.Chat(context.Background(), ChatRequest{
		System: "system prompt", User: "user prompt", MaxTokens: 456,
		Temperature: 0.2, DisableThinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryable || content != "wiki answer" {
		t.Fatalf("content=%q retryable=%v", content, retryable)
	}
	if requestBody["system"] != "system prompt" || int(requestBody["max_tokens"].(float64)) != 456 {
		t.Fatalf("request=%+v", requestBody)
	}
	messages := requestBody["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" || messages[0].(map[string]any)["content"] != "user prompt" {
		t.Fatalf("messages=%+v", messages)
	}
	if _, ok := requestBody["thinking"]; ok {
		t.Fatalf("disable_thinking must be represented by omitting Anthropic thinking: %+v", requestBody)
	}
	if _, ok := requestBody["chat_template_kwargs"]; ok {
		t.Fatalf("OpenAI-only field leaked into Anthropic request: %+v", requestBody)
	}
}

func TestClientDefaultsAndAnthropicErrors(t *testing.T) {
	t.Run("empty protocol defaults to openai", func(t *testing.T) {
		client := Client{
			BaseURL: "https://models.example/v1", APIKey: "key", Model: "model",
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/v1/chat/completions" {
					t.Fatalf("path=%s", req.URL.Path)
				}
				return jsonResponse(http.StatusOK, `{"choices":[{"text":"legacy output"}]}`), nil
			})},
		}
		content, _, err := client.Chat(context.Background(), ChatRequest{MaxTokens: 1})
		if err != nil || content != "legacy output" {
			t.Fatalf("content=%q err=%v", content, err)
		}
	})

	t.Run("anthropic overload is retryable", func(t *testing.T) {
		client := Client{
			BaseURL: "https://anthropic.example/v1", APIKey: "key", Model: "model", Protocol: ProtocolAnthropic,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("anthropic-version") != DefaultAnthropicVersion {
					t.Fatalf("default version=%q", req.Header.Get("anthropic-version"))
				}
				return jsonResponse(http.StatusTooManyRequests, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), nil
			})},
		}
		_, retryable, err := client.Chat(context.Background(), ChatRequest{MaxTokens: 1})
		if err == nil || !retryable || !strings.Contains(err.Error(), "status=429 body=busy") {
			t.Fatalf("retryable=%v err=%v", retryable, err)
		}
	})

	t.Run("anthropic empty text is retryable", func(t *testing.T) {
		client := Client{
			BaseURL: "https://anthropic.example/v1", APIKey: "key", Model: "model", Protocol: ProtocolAnthropic,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, `{"content":[{"type":"thinking","thinking":"only reasoning"}]}`), nil
			})},
		}
		_, retryable, err := client.Chat(context.Background(), ChatRequest{MaxTokens: 1})
		if err == nil || !retryable || !strings.Contains(err.Error(), "empty content") {
			t.Fatalf("retryable=%v err=%v", retryable, err)
		}
	})
}

func TestProtocolEndpointAcceptsRootVersionedAndFullURLs(t *testing.T) {
	tests := []struct {
		base     string
		endpoint string
		want     string
	}{
		{base: "https://models.example", endpoint: "/messages", want: "https://models.example/v1/messages"},
		{base: "https://models.example/", endpoint: "/messages", want: "https://models.example/v1/messages"},
		{base: "https://models.example/v1", endpoint: "/messages", want: "https://models.example/v1/messages"},
		{base: "https://models.example/v1/messages", endpoint: "/messages", want: "https://models.example/v1/messages"},
		{base: "https://models.example", endpoint: "/chat/completions", want: "https://models.example/chat/completions"},
		{base: "https://models.example/v1", endpoint: "/chat/completions", want: "https://models.example/v1/chat/completions"},
		{base: "https://models.example/v1/chat/completions", endpoint: "/chat/completions", want: "https://models.example/v1/chat/completions"},
	}
	for _, tt := range tests {
		if got := protocolEndpoint(tt.base, tt.endpoint); got != tt.want {
			t.Errorf("protocolEndpoint(%q, %q)=%q want %q", tt.base, tt.endpoint, got, tt.want)
		}
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
