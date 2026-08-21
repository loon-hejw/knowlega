package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

func TestPiHTTPTransportOpenAICompatibleToolRequest(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("path=%s auth=%s", request.URL.Path, request.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"model":"model-pi","choices":[{"message":{"content":"checking","reasoning_content":"reason","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`))
	}))
	defer server.Close()
	models := config.ModelsConfig{
		Request:   config.ModelRequestConfig{Retries: 1, TimeoutSeconds: 5},
		Providers: []config.ModelProviderConfig{{ID: "gateway", Protocol: "openai", BaseURL: server.URL + "/v1", APIKey: "test-key", Models: []config.ModelDefinition{{ID: "model-pi"}}}},
		Harnesses: []config.ModelHarnessConfig{{ID: "pi", Provider: "gateway", ModelIDs: []string{"model-pi"}, DefaultModel: "model-pi"}},
	}
	transport, err := NewPiHTTPTransport(models)
	if err != nil {
		t.Fatal(err)
	}
	delta := ""
	blocks := 0
	completion, err := transport.Complete(context.Background(), PiCompletionRequest{
		Model: "model-pi", SystemPrompt: "system", MaxOutputTokens: 100, DisableThinking: true, ThinkingLevel: "high",
		Messages: []PiMessage{{Role: "user", Content: "read", Images: []Image{{MIMEType: "image/png", DataBase64: strings.Repeat("a", 300)}}}},
		Tools:    []ToolDefinition{{Name: "read", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, func(value string) { delta += value }, func() { blocks++ })
	if err != nil {
		t.Fatal(err)
	}
	if completion.Text != "checking" || completion.Thinking != "reason" || len(completion.ToolCalls) != 1 || completion.Usage.Input != 12 || completion.Usage.CacheRead != 4 || delta != "checking" || blocks != 1 {
		t.Fatalf("completion=%#v delta=%q blocks=%d", completion, delta, blocks)
	}
	if requestBody["reasoning_effort"] != "high" || requestBody["enable_thinking"] != false || requestBody["tool_choice"] != "auto" {
		t.Fatalf("request=%#v", requestBody)
	}
	if strings.Contains(string(completion.Request), strings.Repeat("a", 300)) || !strings.Contains(string(completion.Request), "image 322 chars omitted") {
		t.Fatalf("captured request was not image-redacted: %s", completion.Request)
	}
}

func TestOpenAIPiPayloadUsesLowReasoningFallbackWhenThinkingIsDisabled(t *testing.T) {
	payload := openAIPiPayload(PiCompletionRequest{Model: "k2.6", MaxOutputTokens: 4096, DisableThinking: true})
	if payload["enable_thinking"] != false || payload["reasoning_effort"] != "low" {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestParseAnthropicPiResponse(t *testing.T) {
	completion, err := parseAnthropicPiResponse([]byte(`{"model":"claude-test","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"checking"},{"type":"tool_use","id":"tool-1","name":"read","input":{"path":"README.md"}}],"usage":{"input_tokens":20,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}`))
	if err != nil || completion.Text != "checking" || completion.Thinking != "reason" || len(completion.ToolCalls) != 1 || completion.Usage.Input != 20 || completion.Usage.Output != 4 || completion.Usage.CacheRead != 5 || completion.Usage.CacheWrite != 2 {
		t.Fatalf("completion=%#v err=%v", completion, err)
	}
}

func TestParseOpenAIPiStreamPreservesIncrementalTextAndToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"model":"model-pi","choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world","tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"README.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`,
		``,
		`data: [DONE]`,
	}, "\n")
	deltas := []string{}
	blocks := 0
	completion, err := parseOpenAIPiStream(strings.NewReader(stream), func(delta string) { deltas = append(deltas, delta) }, func() { blocks++ })
	if err != nil || completion.Text != "Hello world" || len(completion.ToolCalls) != 1 || string(completion.ToolCalls[0].Arguments) != `{"path":"README.md"}` || completion.Usage.Input != 11 || completion.Usage.CacheRead != 3 || blocks != 1 || !reflect.DeepEqual(deltas, []string{"Hello", " world"}) {
		t.Fatalf("completion=%#v deltas=%#v blocks=%d err=%v", completion, deltas, blocks, err)
	}
}

func TestParseAnthropicPiStreamPreservesThinkingTextAndToolInput(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":9,"cache_read_input_tokens":2}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		``,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool-1","name":"read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README.md\"}"}}`,
		``,
		`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
	}, "\n")
	delta := ""
	blocks := 0
	completion, err := parseAnthropicPiStream(strings.NewReader(stream), func(value string) { delta += value }, func() { blocks++ })
	if err != nil || completion.Text != "answer" || completion.Thinking != "reason" || len(completion.ToolCalls) != 1 || string(completion.ToolCalls[0].Arguments) != `{"path":"README.md"}` || completion.Usage.Input != 9 || completion.Usage.Output != 5 || delta != "answer" || blocks != 1 {
		t.Fatalf("completion=%#v delta=%q blocks=%d err=%v", completion, delta, blocks, err)
	}
}

func TestAnthropicPiPayloadAppliesNodeCompatibleSystemCacheBoundary(t *testing.T) {
	boundary := 8
	payload := anthropicPiPayload(PiCompletionRequest{
		Model: "claude-test", SystemPrompt: "stable😀\nrest", SystemCacheBoundary: &boundary, MaxOutputTokens: 100,
		Tools: []ToolDefinition{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	system, ok := payload["system"].([]any)
	if !ok || len(system) != 2 || system[0].(map[string]any)["text"] != "stable😀" || system[1].(map[string]any)["text"] != "\nrest" {
		t.Fatalf("system=%#v", payload["system"])
	}
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("tools=%#v", tools)
	}
}

func TestMarshalPiPayloadElidesImagesToRequestByteBudget(t *testing.T) {
	payload := openAIPiPayload(PiCompletionRequest{Model: "model-pi", MaxOutputTokens: 100, Messages: []PiMessage{{Role: "user", Content: "look", Images: []Image{{MIMEType: "image/png", DataBase64: strings.Repeat("a", 500)}}}}})
	body, err := marshalPiPayloadWithinBudget(payload, "openai", 600)
	if err != nil || len(body) > 600 || strings.Contains(string(body), strings.Repeat("a", 500)) || !strings.Contains(string(body), "image removed") {
		t.Fatalf("len=%d body=%s err=%v", len(body), body, err)
	}
}

func TestAnthropicPiThinkingMatchesLegacyAndAdaptiveModes(t *testing.T) {
	legacy := anthropicPiPayload(PiCompletionRequest{Model: "claude-test", MaxOutputTokens: 4096, ThinkingLevel: "high"})
	capPiModelOutput(legacy, config.ModelDefinition{MaxTokens: 8192})
	thinking := legacy["thinking"].(map[string]any)
	if legacy["max_tokens"] != 8192 || thinking["type"] != "enabled" || thinking["budget_tokens"] != 7168 {
		t.Fatalf("legacy=%#v", legacy)
	}
	adaptive := anthropicPiPayload(PiCompletionRequest{Model: "claude-test", MaxOutputTokens: 4096, ThinkingLevel: "ultracode", AdaptiveThinking: true})
	if adaptive["thinking"].(map[string]any)["type"] != "adaptive" || adaptive["output_config"].(map[string]string)["effort"] != "max" {
		t.Fatalf("adaptive=%#v", adaptive)
	}
	disabled := anthropicPiPayload(PiCompletionRequest{Model: "claude-test", MaxOutputTokens: 4096, ThinkingLevel: "high", DisableThinking: true})
	if disabled["thinking"].(map[string]string)["type"] != "disabled" {
		t.Fatalf("disabled=%#v", disabled)
	}
}
