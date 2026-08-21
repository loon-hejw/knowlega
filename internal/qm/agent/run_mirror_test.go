package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/worker"
)

type mirrorRecorder struct{ terminal []bool }

func (*mirrorRecorder) StartRuntime(context.Context, string) error                     { return nil }
func (*mirrorRecorder) CompleteRuntime(context.Context, string, json.RawMessage) error { return nil }
func (m *mirrorRecorder) FailRuntime(_ context.Context, _ string, _ string, terminal bool) error {
	m.terminal = append(m.terminal, terminal)
	return nil
}

func TestMirrorTurnRunsMakesPermanentFailureTerminalImmediately(t *testing.T) {
	mirror := &mirrorRecorder{}
	handler := MirrorTurnRuns(func(context.Context, data.RuntimeTask, worker.EventWriter) (json.RawMessage, error) {
		return nil, worker.Permanent(errors.New("invalid request"))
	}, mirror)
	payload, _ := json.Marshal(TurnTaskPayload{RunID: "run-1"})
	_, err := handler(t.Context(), data.RuntimeTask{Payload: payload, Attempts: 1, MaxAttempts: 3}, &collectedEvents{})
	if err == nil || len(mirror.terminal) != 1 || !mirror.terminal[0] {
		t.Fatalf("err=%v terminal=%v", err, mirror.terminal)
	}
}

type inactiveMirrorRecorder struct{}

func (*inactiveMirrorRecorder) StartRuntime(context.Context, string) error {
	return data.ErrRuntimeRunNotActive
}
func (*inactiveMirrorRecorder) CompleteRuntime(context.Context, string, json.RawMessage) error {
	return data.ErrRuntimeRunNotActive
}
func (*inactiveMirrorRecorder) FailRuntime(context.Context, string, string, bool) error {
	return data.ErrRuntimeRunNotActive
}

func TestMirrorTurnRunsDoesNotRequeueAnAlreadyTerminalRun(t *testing.T) {
	handler := MirrorTurnRuns(func(context.Context, data.RuntimeTask, worker.EventWriter) (json.RawMessage, error) {
		t.Fatal("terminal run must not execute the turn handler")
		return nil, nil
	}, &inactiveMirrorRecorder{})
	payload, _ := json.Marshal(TurnTaskPayload{RunID: "run-terminal"})
	_, err := handler(t.Context(), data.RuntimeTask{Payload: payload, Attempts: 2, MaxAttempts: 3}, &collectedEvents{})
	var taskErr *worker.TaskError
	if !errors.As(err, &taskErr) || taskErr.Retry {
		t.Fatalf("err=%v", err)
	}
}
