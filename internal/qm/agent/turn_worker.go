package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/worker"
)

const TurnTaskKind = "agent.turn"

type TurnRunner interface {
	RunTurn(context.Context, TurnInput) (TurnResult, error)
}

// TurnRuntimeResolver resolves the approved harness/model snapshot before
// bindings and preflight tools run. Engine implements this interface.
type TurnRuntimeResolver interface {
	Resolve(context.Context, TurnInput) (Choice, error)
}

type TurnTaskQueue interface {
	Enqueue(context.Context, data.EnqueueRuntimeTaskInput) (string, bool, error)
}

type TurnBindings struct {
	Tools              ToolContext
	Emit               func(context.Context, NewEntry) (SessionEntry, error)
	FindToolResult     func(context.Context, string) (ToolResult, bool, error)
	ToolApprovalGate   func(string) bool
	RecordModelCall    func(ModelCallRecord)
	RecordLLMRequest   func(context.Context, LLMRequestRecord) error
	TakePendingSignals func(context.Context, string) ([]TurnSignal, error)
	Tape               func(context.Context, TapeRecord) error
	TapeRows           []TapeRecord
	TapeMode           string
	TapeCovered        bool
	PrepareInput       func(context.Context, *TurnInput) (*TurnResult, error)
	PersistApprovals   func(context.Context, []PendingApproval) error
}

type TurnBindingsResolver interface {
	ResolveTurnBindings(context.Context, TurnTaskPayload) (TurnBindings, error)
}

func EnqueueTurn(ctx context.Context, queue TurnTaskQueue, payload TurnTaskPayload, idempotencyKey string, priority, maxAttempts int) (string, bool, error) {
	if queue == nil || payload.SessionID == "" || payload.Input == "" || payload.SystemPrompt == "" || payload.ScopeLabel == "" || payload.OrgScopeID == "" {
		return "", false, errors.New("turn queue and complete payload are required")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	return queue.Enqueue(ctx, data.EnqueueRuntimeTaskInput{
		Kind: TurnTaskKind, PayloadVersion: 1, Payload: encoded, ScopeID: payload.ScopeLabel,
		IdempotencyKey: idempotencyKey, SerialKey: "session:" + payload.SessionID, Priority: priority, MaxAttempts: maxAttempts,
	})
}

type TurnTaskPayload struct {
	SessionID           string          `json:"sessionId"`
	RunID               string          `json:"runId,omitempty"`
	Input               string          `json:"input"`
	SystemPrompt        string          `json:"systemPrompt"`
	SystemCacheBoundary *int            `json:"systemCacheBoundary,omitempty"`
	ScopeLabel          string          `json:"scopeLabel"`
	OrgScopeID          string          `json:"orgScopeId"`
	Environment         string          `json:"environment,omitempty"`
	WorkspaceKey        string          `json:"workspaceKey,omitempty"`
	Model               string          `json:"model,omitempty"`
	Harness             string          `json:"harness,omitempty"`
	ThinkingLevel       string          `json:"thinkingLevel,omitempty"`
	FastMode            bool            `json:"fastMode,omitempty"`
	ReadOnly            bool            `json:"readOnly,omitempty"`
	ActorID             string          `json:"actorId,omitempty"`
	ApprovedToolKeys    []string        `json:"approvedToolKeys,omitempty"`
	SurfaceTools        bool            `json:"surfaceTools,omitempty"`
	SurfaceName         string          `json:"surfaceName,omitempty"`
	PollFire            bool            `json:"pollFire,omitempty"`
	TurnWallClockMS     int             `json:"turnWallClockMs,omitempty"`
	History             json.RawMessage `json:"history,omitempty"`
	PriorTurns          json.RawMessage `json:"priorTurns,omitempty"`
	Attachments         json.RawMessage `json:"attachments,omitempty"`
	Images              []Image         `json:"images,omitempty"`
	UserEntrySequence   *int            `json:"userEntrySequence,omitempty"`
	TapeMode            string          `json:"tapeMode,omitempty"`
}

type NonRetryableError struct{ Err error }

func (e *NonRetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "agent turn failed permanently"
	}
	return e.Err.Error()
}

func (e *NonRetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewTurnTaskHandler(runner TurnRunner, bindingResolvers ...TurnBindingsResolver) worker.Handler {
	return func(ctx context.Context, task data.RuntimeTask, events worker.EventWriter) (json.RawMessage, error) {
		if runner == nil {
			return nil, worker.Permanent(errors.New("agent turn runner is not configured"))
		}
		if task.Kind != TurnTaskKind || task.PayloadVersion != 1 {
			return nil, worker.Permanent(fmt.Errorf("unsupported agent turn task %s version %d", task.Kind, task.PayloadVersion))
		}
		var payload TurnTaskPayload
		if err := json.Unmarshal(task.Payload, &payload); err != nil || payload.SessionID == "" || payload.Input == "" || payload.SystemPrompt == "" || payload.ScopeLabel == "" || payload.OrgScopeID == "" {
			return nil, worker.Permanent(errors.New("malformed agent turn task payload"))
		}
		turnAttempt := max(task.Attempts, 1)
		input := TurnInput{
			SessionID: payload.SessionID, RunID: payload.RunID, Input: payload.Input, SystemPrompt: payload.SystemPrompt, SystemCacheBoundary: payload.SystemCacheBoundary,
			ScopeLabel: payload.ScopeLabel, OrgScopeID: payload.OrgScopeID, Environment: payload.Environment,
			Model: payload.Model, Harness: payload.Harness, ThinkingLevel: payload.ThinkingLevel, FastMode: payload.FastMode,
			ReadOnly: payload.ReadOnly, SurfaceTools: payload.SurfaceTools, SurfaceName: payload.SurfaceName,
			PollFire: payload.PollFire, TurnWallClockMS: payload.TurnWallClockMS, History: payload.History,
			PriorTurns: payload.PriorTurns, Attachments: payload.Attachments, Images: payload.Images,
			UserEntrySequence: payload.UserEntrySequence,
		}
		if resolver, ok := runner.(TurnRuntimeResolver); ok {
			choice, err := resolver.Resolve(ctx, input)
			if err != nil {
				var permanent *NonRetryableError
				if errors.As(err, &permanent) {
					return nil, worker.Permanent(err)
				}
				return nil, worker.Retry(err, 0)
			}
			input.Harness, input.Model = choice.HarnessID, choice.ModelID
			payload.Harness, payload.Model = choice.HarnessID, choice.ModelID
		}
		if len(bindingResolvers) > 1 {
			return nil, worker.Permanent(errors.New("only one turn bindings resolver may be configured"))
		}
		var bindings TurnBindings
		if len(bindingResolvers) == 1 {
			var err error
			bindings, err = bindingResolvers[0].ResolveTurnBindings(ctx, payload)
			if err != nil {
				return nil, worker.Retry(err, 0)
			}
			input.Tools = bindings.Tools
			input.Emit = bindings.Emit
			input.FindToolResult = bindings.FindToolResult
			input.ToolApprovalGate = bindings.ToolApprovalGate
			input.RecordModelCall = bindings.RecordModelCall
			input.RecordLLMRequest = bindings.RecordLLMRequest
			input.TakePendingSignals = bindings.TakePendingSignals
			input.Tape = bindings.Tape
			input.TapeRows = bindings.TapeRows
			input.TapeMode = bindings.TapeMode
		}
		var eventError error
		var eventErrorMu sync.Mutex
		appendEvent := func(eventType string, encoded json.RawMessage) {
			if err := events.Append(ctx, eventType, encoded); err != nil {
				eventErrorMu.Lock()
				if eventError == nil {
					eventError = err
				}
				eventErrorMu.Unlock()
			}
		}
		phase := func(name string, values map[string]any) {
			payload := map[string]any{"phase": name, "attempt": turnAttempt, "model": input.Model}
			for key, value := range values {
				payload[key] = value
			}
			encoded, _ := json.Marshal(payload)
			appendEvent("phase", encoded)
		}
		phase("preparing", nil)
		if input.Emit != nil {
			persist := input.Emit
			input.Emit = func(entryCtx context.Context, entry NewEntry) (SessionEntry, error) {
				stored, err := persist(entryCtx, entry)
				if err != nil {
					return SessionEntry{}, err
				}
				if runtimeTurnActivity(entry.Type) {
					if entry.Type == "tool_call" {
						var call struct {
							Tool string `json:"tool"`
						}
						_ = json.Unmarshal(entry.Payload, &call)
						phase("using_tool", map[string]any{"tool": call.Tool})
					}
					appendEvent(entry.Type, runtimeTurnActivityPayload(entry.Payload, turnAttempt))
					if entry.Type == "tool_result" {
						phase("thinking", nil)
					}
				}
				return stored, nil
			}
		}
		if binder, ok := input.Tools.(interface {
			BindProgressEmitter(func(context.Context, NewEntry) (SessionEntry, error))
		}); ok {
			binder.BindProgressEmitter(input.Emit)
		}
		answering := false
		deltas := newTurnDeltaWriter(turnAttempt, appendEvent)
		input.OnDelta = func(chunk string) {
			if !answering {
				answering = true
				phase("answering", nil)
			}
			deltas.Write(chunk)
		}
		input.OnTextBlockStart = func() {
			deltas.Flush()
			encoded, _ := json.Marshal(map[string]int{"attempt": turnAttempt})
			appendEvent("text_block_start", encoded)
		}
		input.OnProgress = func(progress Progress) {
			deltas.Flush()
			progress.Attempt = turnAttempt
			encoded, _ := json.Marshal(progress)
			appendEvent("progress", encoded)
		}
		if bindings.PrepareInput != nil {
			preparedResult, err := bindings.PrepareInput(ctx, &input)
			if err != nil {
				deltas.Close()
				return nil, worker.Retry(err, 0)
			}
			if preparedResult != nil {
				deltas.Close()
				if preparedResult.Status == "" {
					preparedResult.Status = "ok"
				}
				if strings.TrimSpace(preparedResult.Reply) != "" && input.Emit != nil {
					payload, _ := json.Marshal(map[string]any{"text": preparedResult.Reply, "status": preparedResult.Status})
					entry, emitErr := input.Emit(ctx, NewEntry{Type: "assistant", Payload: payload, ScopeLabel: input.ScopeLabel})
					if emitErr != nil {
						return nil, worker.Retry(emitErr, 0)
					}
					preparedResult.FinalEntrySequence = &entry.Sequence
				}
				encoded, encodeErr := json.Marshal(preparedResult)
				if encodeErr != nil {
					return nil, worker.Permanent(encodeErr)
				}
				return encoded, nil
			}
		}
		result, err := runner.RunTurn(ctx, input)
		deltas.Close()
		eventErrorMu.Lock()
		streamErr := eventError
		eventErrorMu.Unlock()
		if streamErr != nil {
			return nil, worker.Retry(streamErr, 0)
		}
		if err != nil {
			var permanent *NonRetryableError
			if errors.As(err, &permanent) {
				return nil, worker.Permanent(err)
			}
			return nil, worker.Retry(err, 0)
		}
		if bindings.Tape != nil && bindings.TapeCovered && !result.TapeWriteFailed && result.FinalEntrySequence != nil {
			watermark, _ := json.Marshal(map[string]bool{"turnEnd": true})
			if err := bindings.Tape(ctx, TapeRecord{Kind: "annotation", ScopeLabel: payload.ScopeLabel, Payload: watermark, EntrySequence: result.FinalEntrySequence}); err != nil {
				result.TapeWriteFailed = true
			}
		}
		if result.Status == "" {
			if result.CompletionStatus == "incomplete" {
				result.Status = "incomplete"
			} else if result.Silent {
				result.Status = "silent"
			} else {
				result.Status = "ok"
			}
		}
		if len(result.PendingApprovals) > 0 {
			for index := range result.PendingApprovals {
				if result.PendingApprovals[index].RequestID == "" {
					sum := sha256.Sum256([]byte(payload.SessionID + "\x00" + result.PendingApprovals[index].Command))
					result.PendingApprovals[index].RequestID = fmt.Sprintf("%x", sum[:])[:16]
				}
			}
			if bindings.PersistApprovals != nil {
				if err := bindings.PersistApprovals(ctx, result.PendingApprovals); err != nil {
					return nil, worker.Retry(err, 0)
				}
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, worker.Permanent(err)
		}
		return encoded, nil
	}
}

type turnDeltaWriter struct {
	mu      sync.Mutex
	buffer  strings.Builder
	timer   *time.Timer
	closed  bool
	attempt int
	append  func(string, json.RawMessage)
}

func newTurnDeltaWriter(attempt int, appendEvent func(string, json.RawMessage)) *turnDeltaWriter {
	return &turnDeltaWriter{attempt: attempt, append: appendEvent}
}

func (w *turnDeltaWriter) Write(chunk string) {
	if chunk == "" {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.buffer.WriteString(chunk)
	flush := w.buffer.Len() >= 256
	if !flush && w.timer == nil {
		w.timer = time.AfterFunc(50*time.Millisecond, w.Flush)
	}
	w.mu.Unlock()
	if flush {
		w.Flush()
	}
}

func (w *turnDeltaWriter) Flush() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	chunk := w.buffer.String()
	w.buffer.Reset()
	w.mu.Unlock()
	if chunk == "" {
		return
	}
	encoded, _ := json.Marshal(map[string]any{"attempt": w.attempt, "chunk": chunk})
	w.append("delta", encoded)
}

func (w *turnDeltaWriter) Close() {
	w.Flush()
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func runtimeTurnActivity(entryType string) bool {
	switch entryType {
	case "tool_call", "tool_progress", "tool_result", "thinking", "text":
		return true
	default:
		return false
	}
}

func runtimeTurnActivityPayload(payload json.RawMessage, attempt int) json.RawMessage {
	var value map[string]any
	if json.Unmarshal(payload, &value) != nil {
		value = map[string]any{}
	}
	value["attempt"] = attempt
	if result, ok := value["result"].(string); ok && len(result) > 4096 {
		value["result"] = result[:4096]
		value["truncated"] = true
	}
	encoded, _ := json.Marshal(value)
	return encoded
}
