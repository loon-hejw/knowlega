package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type fakeClaudeWire struct {
	messages chan ClaudeWireMessage
	sent     []any
	closed   bool
}

func (w *fakeClaudeWire) Send(_ context.Context, value any) error {
	w.sent = append(w.sent, value)
	return nil
}
func (w *fakeClaudeWire) Messages() <-chan ClaudeWireMessage { return w.messages }
func (*fakeClaudeWire) Interrupt() error                     { return nil }
func (w *fakeClaudeWire) Close(context.Context) error        { w.closed = true; return nil }

func TestClaudeAdapterConsumesSDKWireAndInProcessMCPTools(t *testing.T) {
	wire := &fakeClaudeWire{messages: make(chan ClaudeWireMessage, 4)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":1},"content":[{"type":"thinking","thinking":"inspect"}]}}`)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","content_block":{"type":"text"}}}`)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}`)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"result","subtype":"success","result":"done","num_turns":1,"usage":{"input_tokens":12,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":1}}`)}
	close(wire.messages)
	models := claudeTestModels()
	tools := &fakePiTools{}
	adapter, err := NewClaudeAdapter(models, func(ctx context.Context, _ config.ModelsConfig, _ config.ModelHarnessConfig, _ TurnInput, definitions []ToolDefinition, execute func(context.Context, ToolCall) ToolResult) (ClaudeWire, error) {
		if len(definitions) < 2 {
			t.Fatalf("definitions=%#v", definitions)
		}
		result := execute(ctx, ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)})
		if toolResultText(result) != "file contents" {
			t.Fatalf("result=%#v", result)
		}
		return wire, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []NewEntry{}
	tape := []TapeRecord{}
	deltas := []string{}
	blocks := 0
	result, err := adapter.RunTurn(t.Context(), TurnInput{
		SessionID: "session-1", Input: "read", SystemPrompt: "system", ScopeLabel: "personal:a", OrgScopeID: "org:a", Model: "model-claude", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
		Tape:    func(_ context.Context, record TapeRecord) error { tape = append(tape, record); return nil },
		OnDelta: func(delta string) { deltas = append(deltas, delta) }, OnTextBlockStart: func() { blocks++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "done" || result.ModelCalls != 1 || result.FinalEntrySequence == nil || len(tools.calls) != 1 || blocks != 1 || !reflect.DeepEqual(deltas, []string{"done"}) {
		t.Fatalf("result=%#v tools=%#v blocks=%d deltas=%#v", result, tools.calls, blocks, deltas)
	}
	types := make([]string, len(entries))
	for index := range entries {
		types[index] = entries[index].Type
	}
	if !reflect.DeepEqual(types, []string{"user", "tool_call", "tool_result", "thinking", "assistant"}) || len(tape) != 2 || tape[0].Harness != "claude" || len(wire.sent) != 1 || !wire.closed {
		t.Fatalf("types=%#v tape=%#v sent=%#v closed=%v", types, tape, wire.sent, wire.closed)
	}
}

func TestClaudeMCPHandlerListsAndExecutesGoTools(t *testing.T) {
	calls := []ToolCall{}
	handler := claudeMCPHandler("secret", []ToolDefinition{{Name: "read", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(_ context.Context, call ToolCall) ToolResult {
		calls = append(calls, call)
		return ToolResult{Content: []ToolContent{{Type: "text", Text: "contents"}}}
	})
	request := httptest.NewRequest(http.MethodPost, "/mcp?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"path":"README.md"}}}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(calls) != 1 || calls[0].Name != "read" || !strings.Contains(response.Body.String(), "contents") {
		t.Fatalf("code=%d calls=%#v body=%s", response.Code, calls, response.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", unauthorized.Code)
	}
}

func TestClaudeProcessRuntimeStarts(t *testing.T) {
	if os.Getenv("QM_CLAUDE_INTEGRATION") != "1" {
		t.Skip("QM_CLAUDE_INTEGRATION is not set")
	}
	models := claudeTestModels()
	harness, _ := models.Harness("claude")
	wire, err := NewClaudeCLIRuntime(t.Context(), models, harness, TurnInput{Model: "model-claude", SystemPrompt: "system", ScopeLabel: "org:test", OrgScopeID: "org:test"}, nil, func(context.Context, ToolCall) ToolResult { return ToolResult{} })
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.Close(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func claudeTestModels() config.ModelsConfig {
	return config.ModelsConfig{DefaultHarness: "claude", Request: config.ModelRequestConfig{MaxOutputTokens: 4096}, Providers: []config.ModelProviderConfig{{ID: "gateway", Protocol: "anthropic", BaseURL: "https://models.example", APIKey: "test", Models: []config.ModelDefinition{{ID: "model-claude", ContextWindow: 200000, MaxTokens: 8192}}}}, Harnesses: []config.ModelHarnessConfig{{ID: "claude", Provider: "gateway", ModelIDs: []string{"model-claude"}, DefaultModel: "model-claude"}}}
}
