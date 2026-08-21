package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type fakeCodexRPC struct {
	notification CodexNotificationHandler
	request      CodexRequestHandler
	methods      []string
}

func (r *fakeCodexRPC) Request(ctx context.Context, method string, params any, output any) error {
	r.methods = append(r.methods, method)
	var value any
	switch method {
	case "thread/start":
		value = map[string]any{"thread": map[string]string{"id": "thread-1"}, "model": "model-codex"}
	case "thread/inject_items":
		value = true
	case "turn/start":
		toolResult, err := r.request(ctx, "item/tool/call", json.RawMessage(`{"threadId":"thread-1","tool":"read","callId":"call-1","arguments":{"path":"README.md"}}`))
		if err != nil || toolResult == nil {
			return errors.New("tool request failed")
		}
		value = map[string]any{"turn": map[string]string{"id": "turn-1", "status": "inProgress"}}
		usage, _ := json.Marshal(map[string]any{"threadId": "thread-1", "tokenUsage": map[string]any{"total": map[string]int{"inputTokens": 20}, "last": map[string]int{"inputTokens": 20}}})
		r.notification(ctx, "thread/tokenUsage/updated", usage)
		delta, _ := json.Marshal(map[string]any{"threadId": "thread-1", "delta": "done"})
		r.notification(ctx, "item/agentMessage/delta", delta)
		item, _ := json.Marshal(map[string]any{"threadId": "thread-1", "item": map[string]any{"type": "agentMessage", "text": "done", "phase": "final_answer"}})
		r.notification(ctx, "item/completed", item)
		completed, _ := json.Marshal(map[string]any{
			"threadId": "thread-1",
			"turn": map[string]any{
				"id": "turn-1", "status": "completed",
				"items": []any{
					map[string]any{"type": "reasoning", "summary": []string{"inspect"}},
					map[string]any{"type": "agentMessage", "text": "done", "phase": "final_answer"},
				},
			},
		})
		r.notification(ctx, "turn/completed", completed)
	case "turn/interrupt", "turn/steer":
		value = true
	default:
		return errors.New("unexpected method " + method)
	}
	if output != nil {
		encoded, _ := json.Marshal(value)
		return json.Unmarshal(encoded, output)
	}
	return nil
}
func (*fakeCodexRPC) Notify(context.Context, string, any) error { return nil }
func (*fakeCodexRPC) Close(context.Context) error               { return nil }

func TestCodexAdapterRunsJSONRPCDynamicToolTurn(t *testing.T) {
	models := codexTestModels()
	rpc := &fakeCodexRPC{}
	adapter, err := NewCodexAdapter(models, func(_ context.Context, _ config.ModelsConfig, _ config.ModelHarnessConfig, notification CodexNotificationHandler, request CodexRequestHandler) (CodexRPC, string, func(), error) {
		rpc.notification, rpc.request = notification, request
		return rpc, "/tmp/codex-jail", func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []NewEntry{}
	tape := []TapeRecord{}
	modelCalls := []ModelCallRecord{}
	deltas := []string{}
	tools := &fakePiTools{}
	result, err := adapter.RunTurn(t.Context(), TurnInput{
		SessionID: "session-1", Input: "read", SystemPrompt: "system", ScopeLabel: "personal:a", OrgScopeID: "org:a", Model: "model-codex", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
		Tape:            func(_ context.Context, record TapeRecord) error { tape = append(tape, record); return nil },
		RecordModelCall: func(record ModelCallRecord) { modelCalls = append(modelCalls, record) },
		OnDelta:         func(delta string) { deltas = append(deltas, delta) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "done" || result.ModelCalls != 1 || result.FinalEntrySequence == nil || len(tools.calls) != 1 || tools.calls[0].Name != "read" {
		t.Fatalf("result=%#v tools=%#v", result, tools.calls)
	}
	types := make([]string, len(entries))
	for index := range entries {
		types[index] = entries[index].Type
	}
	if !reflect.DeepEqual(types, []string{"user", "tool_call", "tool_result", "thinking", "assistant"}) || !reflect.DeepEqual(deltas, []string{"done"}) {
		t.Fatalf("types=%#v deltas=%#v", types, deltas)
	}
	if !reflect.DeepEqual(rpc.methods, []string{"thread/start", "turn/start"}) || len(tape) != 2 || tape[0].Harness != "codex" || len(modelCalls) != 1 {
		t.Fatalf("methods=%#v tape=%#v modelCalls=%#v", rpc.methods, tape, modelCalls)
	}
}

func TestCodexHelpersMatchNodeContract(t *testing.T) {
	long := strings.Repeat("x", 65)
	if codexReplayCallID(long) == long || len(codexReplayCallID(long)) != 64 || codexReasoningEffort("max") != "" || codexReasoningEffort("xhigh") != "xhigh" {
		t.Fatal("codex helpers diverged")
	}
	if _, ok := classifyCodexError(errors.New("401 invalid api key")).(*NonRetryableError); !ok {
		t.Fatal("provider auth error must be permanent")
	}
	items := stripCodexInputImages([]map[string]any{{"type": "image", "url": "data:image/png;base64,secret"}})
	if items[0]["url"] != "[image bytes omitted]" {
		t.Fatalf("items=%#v", items)
	}
}

func TestCodexCollabThreadsUseNodeChildToolBoundary(t *testing.T) {
	entries := []NewEntry{}
	tools := &fakePiTools{}
	input := TurnInput{ScopeLabel: "personal:a", Tools: tools, Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
		entries = append(entries, entry)
		return SessionEntry{Sequence: len(entries)}, nil
	}}
	state := &codexTurnState{
		threadID: "parent", input: input, tools: map[string]ToolDefinition{"read": {Name: "read"}, "finish_silently": {Name: "finish_silently"}},
		completed: make(chan CodexTurn, 1), childThreads: map[string]string{}, childStatus: map[string]string{}, childResults: map[string]bool{},
	}
	state.toolState = openCodeTurnState{input: input}
	adapter := &CodexAdapter{active: map[string]*codexTurnState{"parent": state}}
	adapter.processCodexCollabItem(t.Context(), state, map[string]any{"type": "collabAgentToolCall", "tool": "spawnAgent", "id": "spawn-1", "prompt": "inspect", "receiverThreadIds": []any{"child"}})
	if adapter.active["child"] != state || len(entries) != 1 || entries[0].Type != "tool_call" {
		t.Fatalf("active=%#v entries=%#v", adapter.active, entries)
	}
	if _, err := adapter.request(t.Context(), "item/tool/call", json.RawMessage(`{"threadId":"child","tool":"read","callId":"c1","arguments":{"path":"README.md"}}`)); err != nil {
		t.Fatalf("allowed child tool failed: %v", err)
	}
	if _, err := adapter.request(t.Context(), "item/tool/call", json.RawMessage(`{"threadId":"child","tool":"finish_silently","callId":"c2","arguments":{}}`)); err == nil || !strings.Contains(err.Error(), "child requested unavailable") {
		t.Fatalf("unsafe child tool err=%v", err)
	}
	adapter.notification(t.Context(), "turn/completed", json.RawMessage(`{"threadId":"child","turn":{"id":"child-turn","status":"completed"}}`))
	select {
	case <-state.completed:
		t.Fatal("child completion terminated the parent turn")
	default:
	}
}

func TestCodexProcessRuntimeInitializes(t *testing.T) {
	if os.Getenv("QM_CODEX_INTEGRATION") != "1" {
		t.Skip("QM_CODEX_INTEGRATION is not set")
	}
	models := codexTestModels()
	harness, _ := models.Harness("codex")
	rpc, _, cleanup, err := NewCodexProcessRuntime(t.Context(), models, harness, func(context.Context, string, json.RawMessage) {}, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("unexpected request")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := rpc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func codexTestModels() config.ModelsConfig {
	return config.ModelsConfig{
		DefaultHarness: "codex", Request: config.ModelRequestConfig{MaxOutputTokens: 4096},
		Providers: []config.ModelProviderConfig{{ID: "gateway", Protocol: "openai", BaseURL: "https://models.example/v1", APIKey: "test", Models: []config.ModelDefinition{{ID: "model-codex", ContextWindow: 128000, MaxTokens: 8192}}}},
		Harnesses: []config.ModelHarnessConfig{{ID: "codex", Provider: "gateway", ModelIDs: []string{"model-codex"}, DefaultModel: "model-codex"}},
	}
}
