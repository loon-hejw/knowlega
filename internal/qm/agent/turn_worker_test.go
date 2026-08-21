package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/worker"
)

type fakeTurnRunner struct {
	input  TurnInput
	err    error
	result *TurnResult
	calls  int
}

func (r *fakeTurnRunner) RunTurn(_ context.Context, input TurnInput) (TurnResult, error) {
	r.calls++
	r.input = input
	if input.OnTextBlockStart != nil {
		input.OnTextBlockStart()
	}
	if input.OnDelta != nil {
		input.OnDelta("hello")
	}
	if input.OnProgress != nil {
		input.OnProgress(Progress{ToolCalls: 2, Tokens: 10})
	}
	if r.result != nil {
		return *r.result, r.err
	}
	return TurnResult{Reply: "done", ModelCalls: 1}, r.err
}

func TestTurnTaskHandlerShortCircuitsPreparedInterruption(t *testing.T) {
	runner := &fakeTurnRunner{}
	entries := []NewEntry{}
	handler := NewTurnTaskHandler(runner, fixedTurnBindings{bindings: TurnBindings{
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: 9}, nil
		},
		PrepareInput: func(context.Context, *TurnInput) (*TurnResult, error) {
			return &TurnResult{Status: "interrupted", Reply: "项目知识查询暂时失败（provider_timeout，已尝试 4 次）。本次未形成经项目证据核实的答案，请重试。"}, nil
		},
	}})
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", RunID: "r1", Input: "question", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	result, err := handler(t.Context(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: 1, Payload: payload}, &collectedEvents{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded TurnResult
	if json.Unmarshal(result, &decoded) != nil || decoded.Status != "interrupted" || !strings.Contains(decoded.Reply, "已尝试 4 次") {
		t.Fatalf("result=%s decoded=%+v", result, decoded)
	}
	if runner.calls != 0 || len(entries) != 1 || entries[0].Type != "assistant" || decoded.FinalEntrySequence != nil {
		t.Fatalf("runner calls=%d entries=%+v decoded=%+v", runner.calls, entries, decoded)
	}
}

type collectedEvents []data.RuntimeTaskEvent

type capturedTurnQueue struct{ input data.EnqueueRuntimeTaskInput }

type fixedTurnBindings struct{ bindings TurnBindings }

func (r fixedTurnBindings) ResolveTurnBindings(context.Context, TurnTaskPayload) (TurnBindings, error) {
	return r.bindings, nil
}

type resolvingTurnRunner struct {
	fakeTurnRunner
	choice Choice
}

func (r *resolvingTurnRunner) Resolve(context.Context, TurnInput) (Choice, error) {
	return r.choice, nil
}

type capturingTurnBindings struct {
	payload TurnTaskPayload
}

func (r *capturingTurnBindings) ResolveTurnBindings(_ context.Context, payload TurnTaskPayload) (TurnBindings, error) {
	r.payload = payload
	return TurnBindings{}, nil
}

func (q *capturedTurnQueue) Enqueue(_ context.Context, input data.EnqueueRuntimeTaskInput) (string, bool, error) {
	q.input = input
	return "task-1", false, nil
}

func (e *collectedEvents) Append(_ context.Context, eventType string, payload json.RawMessage) error {
	*e = append(*e, data.RuntimeTaskEvent{Type: eventType, Payload: payload})
	return nil
}

func TestTurnTaskHandlerRunsEngineAndStreamsEvents(t *testing.T) {
	runner := &fakeTurnRunner{}
	handler := NewTurnTaskHandler(runner)
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", RunID: "r1", Input: "question", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Harness: "codex", Model: "model-codex"})
	events := collectedEvents{}
	result, err := handler(context.Background(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: 1, Payload: payload}, &events)
	if err != nil || runner.input.SessionID != "s1" || runner.input.Harness != "codex" || runner.input.Model != "model-codex" {
		t.Fatalf("input=%#v result=%s err=%v", runner.input, result, err)
	}
	if len(events) != 5 || events[0].Type != "phase" || events[1].Type != "text_block_start" || events[2].Type != "phase" || events[3].Type != "delta" || events[4].Type != "progress" {
		t.Fatalf("events=%#v", events)
	}
	var decoded TurnResult
	if json.Unmarshal(result, &decoded) != nil || decoded.Reply != "done" || decoded.ModelCalls != 1 {
		t.Fatalf("result=%s decoded=%#v", result, decoded)
	}
}

func TestTurnTaskHandlerFreezesRuntimeBeforeBindingsAndPreflight(t *testing.T) {
	runner := &resolvingTurnRunner{choice: Choice{HarnessID: "pi", ModelID: "Qwen3.6-27B-FP8"}}
	bindings := &capturingTurnBindings{}
	handler := NewTurnTaskHandler(runner, bindings)
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", RunID: "r1", Input: "question", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	if _, err := handler(t.Context(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: 1, Payload: payload}, &collectedEvents{}); err != nil {
		t.Fatal(err)
	}
	if runner.input.Harness != "pi" || runner.input.Model != "Qwen3.6-27B-FP8" || bindings.payload.Model != runner.input.Model {
		t.Fatalf("runner=%+v bindings=%+v", runner.input, bindings.payload)
	}
}

func TestEnqueueTurnUsesDurableSessionSerialKey(t *testing.T) {
	queue := &capturedTurnQueue{}
	id, deduped, err := EnqueueTurn(context.Background(), queue, TurnTaskPayload{SessionID: "s1", Input: "question", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme"}, "turn-1", 7, 3)
	if err != nil || deduped || id != "task-1" || queue.input.Kind != TurnTaskKind || queue.input.SerialKey != "session:s1" || queue.input.IdempotencyKey != "turn-1" || queue.input.Priority != 7 || queue.input.MaxAttempts != 3 {
		t.Fatalf("id=%q deduped=%v input=%#v err=%v", id, deduped, queue.input, err)
	}
}

func TestTurnTaskHandlerWritesCoveredTapeWatermark(t *testing.T) {
	finalSequence := 7
	runner := &fakeTurnRunner{result: &TurnResult{Reply: "done", FinalEntrySequence: &finalSequence}}
	written := []TapeRecord{}
	handler := NewTurnTaskHandler(runner, fixedTurnBindings{bindings: TurnBindings{
		TapeCovered: true, TapeMode: "serve",
		Tape: func(_ context.Context, record TapeRecord) error {
			written = append(written, record)
			return nil
		},
	}})
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", Input: "question", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", TapeMode: "serve"})
	if _, err := handler(context.Background(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: 1, Payload: payload}, &collectedEvents{}); err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].EntrySequence == nil || *written[0].EntrySequence != finalSequence || string(written[0].Payload) != `{"turnEnd":true}` || runner.input.TapeMode != "serve" {
		t.Fatalf("written=%#v input=%#v", written, runner.input)
	}
}

func TestTurnTaskHandlerUsesNodeCompatibleCommandApprovalID(t *testing.T) {
	runner := &fakeTurnRunner{result: &TurnResult{PausedOnApproval: true, PendingApprovals: []PendingApproval{{Command: "execute", Reason: "strict"}}}}
	var persisted []PendingApproval
	handler := NewTurnTaskHandler(runner, fixedTurnBindings{bindings: TurnBindings{PersistApprovals: func(_ context.Context, values []PendingApproval) error {
		persisted = append(persisted, values...)
		return nil
	}}})
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", RunID: "r1", Input: "question", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme"})
	result, err := handler(context.Background(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: 1, Payload: payload}, &collectedEvents{})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].RequestID != "9e594a648fe52d58" || !strings.Contains(string(result), `"requestId":"9e594a648fe52d58"`) {
		t.Fatalf("persisted=%#v result=%s", persisted, result)
	}
}

func TestTurnTaskHandlerClassifiesFailures(t *testing.T) {
	payload, _ := json.Marshal(TurnTaskPayload{SessionID: "s1", Input: "question", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme"})
	tests := []struct {
		name      string
		runner    TurnRunner
		version   int
		retryable bool
	}{
		{"transient", &fakeTurnRunner{err: errors.New("overloaded")}, 1, true},
		{"permanent", &fakeTurnRunner{err: &NonRetryableError{Err: errors.New("not approved")}}, 1, false},
		{"version", &fakeTurnRunner{}, 2, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewTurnTaskHandler(test.runner)
			_, err := handler(context.Background(), data.RuntimeTask{Kind: TurnTaskKind, PayloadVersion: test.version, Payload: payload}, &collectedEvents{})
			var taskErr *worker.TaskError
			if !errors.As(err, &taskErr) || taskErr.Retry != test.retryable {
				t.Fatalf("err=%v retryable=%v", err, test.retryable)
			}
		})
	}
}
