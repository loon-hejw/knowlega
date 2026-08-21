package agent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type fakeOpenCodeRuntime struct {
	state    *OpenCodeBridgeState
	prompt   OpenCodePrompt
	messages []OpenCodeMessage
	closed   bool
}

func (*fakeOpenCodeRuntime) CreateSession(context.Context, string) (string, error) {
	return "oc-session", nil
}
func (r *fakeOpenCodeRuntime) Register(_ string, state *OpenCodeBridgeState) { r.state = state }
func (*fakeOpenCodeRuntime) Unregister(string)                               {}
func (r *fakeOpenCodeRuntime) Prompt(ctx context.Context, _ string, prompt OpenCodePrompt) (OpenCodeMessage, error) {
	r.prompt = prompt
	r.state.Capture(ctx, json.RawMessage(`{"messages":[{"role":"user"}]}`))
	result := r.state.Execute(ctx, ToolCall{ID: "call-1", Name: "workspace_read", Arguments: json.RawMessage(`{"path":"README.md"}`)})
	if toolResultText(result) != "file contents" {
		return OpenCodeMessage{}, context.Canceled
	}
	return OpenCodeMessage{Info: map[string]any{"role": "assistant"}, Parts: []map[string]any{{"type": "reasoning", "text": "inspect"}, {"type": "text", "text": "done"}}}, nil
}
func (*fakeOpenCodeRuntime) PromptAsync(context.Context, string, OpenCodePrompt) error { return nil }
func (*fakeOpenCodeRuntime) WaitIdle(context.Context, string) error                    { return nil }
func (r *fakeOpenCodeRuntime) Messages(context.Context, string) ([]OpenCodeMessage, error) {
	return r.messages, nil
}
func (*fakeOpenCodeRuntime) Abort(context.Context, string) error         { return nil }
func (*fakeOpenCodeRuntime) DeleteSession(context.Context, string) error { return nil }
func (r *fakeOpenCodeRuntime) Close(context.Context) error               { r.closed = true; return nil }

func TestOpenCodeAdapterPreservesPluginTransportAndTranscript(t *testing.T) {
	models := openCodeTestModels()
	runtime := &fakeOpenCodeRuntime{messages: []OpenCodeMessage{
		{Info: map[string]any{"role": "user"}, Parts: []map[string]any{{"type": "file", "url": "data:image/png;base64,secret"}}},
		{Info: map[string]any{"role": "assistant"}, Parts: []map[string]any{{"type": "text", "text": "done"}}},
	}}
	adapter, err := NewOpenCodeAdapter(models, func(context.Context, config.ModelsConfig, config.ModelHarnessConfig, []ToolDefinition) (OpenCodeRuntime, error) {
		return runtime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []NewEntry{}
	tape := []TapeRecord{}
	modelCalls := []ModelCallRecord{}
	tools := &fakePiTools{}
	result, err := adapter.RunTurn(t.Context(), TurnInput{
		SessionID: "session-1", Input: "read", SystemPrompt: "system", ScopeLabel: "personal:a", OrgScopeID: "org:a", Model: "model-oc", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
		Tape:            func(_ context.Context, record TapeRecord) error { tape = append(tape, record); return nil },
		RecordModelCall: func(record ModelCallRecord) { modelCalls = append(modelCalls, record) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "done" || result.ModelCalls != 1 || result.FinalEntrySequence == nil || len(tools.calls) != 1 || tools.calls[0].Name != "read" {
		t.Fatalf("result=%#v calls=%#v", result, tools.calls)
	}
	types := make([]string, len(entries))
	for index := range entries {
		types[index] = entries[index].Type
	}
	if !reflect.DeepEqual(types, []string{"user", "tool_call", "tool_result", "thinking", "assistant"}) {
		t.Fatalf("entries=%#v", entries)
	}
	if runtime.prompt.ModelProvider != "gateway" || runtime.prompt.ModelID != "model-oc" || !runtime.prompt.Tools["workspace_read"] || runtime.prompt.Tools["read"] {
		t.Fatalf("prompt=%#v", runtime.prompt)
	}
	if len(tape) != 2 || tape[0].Harness != "opencode" || string(tape[0].Payload) == "" || containsJSONText(tape[0].Payload, "secret") {
		t.Fatalf("tape=%#v", tape)
	}
	if len(modelCalls) != 1 || runtime.state == nil || len(runtime.state.History) != 0 {
		t.Fatalf("modelCalls=%#v state=%#v", modelCalls, runtime.state)
	}
}

func TestOpenCodeAssistantFailureClassification(t *testing.T) {
	message, permanent := openCodeAssistantFailure(map[string]any{"error": map[string]any{"name": "ProviderAuthError", "data": map[string]any{"message": "bad key"}}})
	if !permanent || message != "OpenCode provider error (ProviderAuthError): bad key" {
		t.Fatalf("message=%q permanent=%v", message, permanent)
	}
	message, permanent = openCodeAssistantFailure(map[string]any{"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "busy", "isRetryable": true}}})
	if permanent || message == "" {
		t.Fatalf("message=%q permanent=%v", message, permanent)
	}
}

func TestOpenCodeConfigurationUsesOnlyConfiguredGateway(t *testing.T) {
	models := openCodeTestModels()
	provider, _ := models.Provider("gateway")
	configuration := openCodeConfiguration(provider, []ToolDefinition{{Name: "workspace_read"}}, "/tmp/plugin.ts")
	enabled := configuration["enabled_providers"].([]string)
	providers := configuration["provider"].(map[string]any)
	if !reflect.DeepEqual(enabled, []string{"gateway"}) || len(providers) != 1 || providers["gateway"] == nil {
		t.Fatalf("configuration=%#v", configuration)
	}
}

func TestOpenCodeBridgePluginIsEmbeddedInGoRuntime(t *testing.T) {
	text := string(openCodeBridgePlugin)
	for _, required := range []string{"@opencode-ai/plugin", "OPENCODE_BRIDGE_URL", "experimental.chat.messages.transform", "export default OpenCodeBridgePlugin"} {
		if !strings.Contains(text, required) {
			t.Fatalf("embedded OpenCode bridge is missing %q", required)
		}
	}
	if strings.Contains(text, "qm/src/harness/opencode-plugin") {
		t.Fatal("embedded bridge must not depend on the Node source tree")
	}
}

func TestOpenCodeExecCommandUsesShellForPostinstallPlaceholder(t *testing.T) {
	path := t.TempDir() + "/opencode"
	if err := os.WriteFile(path, []byte("echo missing postinstall >&2\nexit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := openCodeExecCommand(path, "serve")
	if command.Path != "/bin/sh" || len(command.Args) != 3 || command.Args[1] != path || command.Args[2] != "serve" {
		t.Fatalf("path=%q args=%#v", command.Path, command.Args)
	}
}

func TestOpenCodeProcessRuntimeStarts(t *testing.T) {
	if os.Getenv("QM_OPENCODE_INTEGRATION") != "1" {
		t.Skip("QM_OPENCODE_INTEGRATION is not set")
	}
	models := openCodeTestModels()
	harness, _ := models.Harness("opencode")
	harness.Runtime.StartupTimeoutSeconds = 30
	runtime, err := NewOpenCodeProcessRuntime(t.Context(), models, harness, []ToolDefinition{{Name: "finish_silently", Description: "finish", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func openCodeTestModels() config.ModelsConfig {
	return config.ModelsConfig{
		DefaultHarness: "opencode", Request: config.ModelRequestConfig{MaxOutputTokens: 4096},
		Providers: []config.ModelProviderConfig{{ID: "gateway", Protocol: "openai", BaseURL: "https://models.example/v1", APIKey: "test", Models: []config.ModelDefinition{{ID: "model-oc", Name: "OpenCode Model", ContextWindow: 128000, MaxTokens: 8192}}}},
		Harnesses: []config.ModelHarnessConfig{{ID: "opencode", Provider: "gateway", ModelIDs: []string{"model-oc"}, DefaultModel: "model-oc"}},
	}
}

func containsJSONText(raw json.RawMessage, value string) bool {
	return strings.Contains(string(raw), value)
}
