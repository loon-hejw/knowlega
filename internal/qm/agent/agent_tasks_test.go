package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type fakeAgentTaskStore struct {
	mu          sync.Mutex
	creates     [][]string
	transitions [][]string
	status      map[string]string
}

func (s *fakeAgentTaskStore) CreateTask(_ context.Context, id, sessionID, originRunID, title, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status = map[string]string{}
	}
	s.creates = append(s.creates, []string{id, sessionID, originRunID, title, status})
	s.status[id] = status
	return nil
}

func (s *fakeAgentTaskStore) TransitionTask(_ context.Context, id, expected, next, runID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] != expected {
		return false, nil
	}
	s.transitions = append(s.transitions, []string{id, expected, next, runID})
	s.status[id] = next
	return true, nil
}

func TestCodexChildLifecyclePersistsNodeCompatibleTasks(t *testing.T) {
	store := &fakeAgentTaskStore{}
	entries := []NewEntry{}
	input := TurnInput{SessionID: "session-1", RunID: "run-1", ScopeLabel: "personal:a", Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
		entries = append(entries, entry)
		return SessionEntry{Sequence: len(entries)}, nil
	}}
	state := &codexTurnState{threadID: "parent", input: input, childThreads: map[string]string{}, childStatus: map[string]string{}, childResults: map[string]bool{}}
	adapter := (&CodexAdapter{active: map[string]*codexTurnState{"parent": state}}).WithTaskStore(store)
	adapter.processCodexCollabItem(t.Context(), state, map[string]any{
		"type": "collabAgentToolCall", "tool": "spawnAgent", "id": "spawn-1",
		"prompt": "You are the research subagent. Inspect the durable task flow.", "receiverThreadIds": []any{"child-1"},
	})
	adapter.processCodexCollabItem(t.Context(), state, map[string]any{
		"type": "collabAgentToolCall", "tool": "wait", "agentsStates": map[string]any{
			"child-1": map[string]any{"status": "completed", "message": "done"},
		},
	})
	if len(store.creates) != 1 || store.creates[0][1] != "session-1" || store.creates[0][2] != "run-1" || store.creates[0][3] != "research subagent" {
		t.Fatalf("creates=%#v", store.creates)
	}
	if len(store.transitions) != 1 || store.transitions[0][1] != "in_progress" || store.transitions[0][2] != "completed" {
		t.Fatalf("transitions=%#v", store.transitions)
	}
	if len(entries) != 2 || entries[0].Type != "tool_call" || entries[1].Type != "tool_result" {
		t.Fatalf("entries=%#v", entries)
	}
}

func TestClaudeChildLifecyclePersistsNodeCompatibleTasks(t *testing.T) {
	wire := &fakeClaudeWire{messages: make(chan ClaudeWireMessage, 4)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"call-1","description":"inspect"}`)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"system","subtype":"task_notification","task_id":"task-1","status":"completed","summary":"done"}`)}
	wire.messages <- ClaudeWireMessage{Raw: json.RawMessage(`{"type":"result","subtype":"success","result":"parent done","num_turns":1,"usage":{"input_tokens":2,"output_tokens":1}}`)}
	close(wire.messages)
	store := &fakeAgentTaskStore{}
	adapter, err := NewClaudeAdapter(claudeTestModels(), func(context.Context, config.ModelsConfig, config.ModelHarnessConfig, TurnInput, []ToolDefinition, func(context.Context, ToolCall) ToolResult) (ClaudeWire, error) {
		return wire, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.WithTaskStore(store)
	entries := []NewEntry{}
	_, err = adapter.RunTurn(t.Context(), TurnInput{SessionID: "session-1", RunID: "run-1", Input: "delegate", SystemPrompt: "system", ScopeLabel: "personal:a", Model: "model-claude", Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
		entries = append(entries, entry)
		return SessionEntry{Sequence: len(entries)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.creates) != 1 || len(store.transitions) != 1 || store.transitions[0][2] != "completed" {
		t.Fatalf("creates=%#v transitions=%#v", store.creates, store.transitions)
	}
	var types []string
	for _, entry := range entries {
		types = append(types, entry.Type)
	}
	if strings.Join(types, ",") != "user,tool_call,tool_result,assistant" {
		t.Fatalf("types=%#v", types)
	}
}

func TestCodexTaskTitleMatchesNodeLimit(t *testing.T) {
	if got := codexTaskTitle(nil); got != "subagent task" {
		t.Fatalf("got=%q", got)
	}
	if got := codexTaskTitle(strings.Repeat("x", 140)); len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got=%q len=%d", got, len([]rune(got)))
	}
}
