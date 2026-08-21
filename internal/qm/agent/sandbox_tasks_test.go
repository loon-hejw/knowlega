package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type completedSandboxStore struct {
	enqueued data.EnqueueRuntimeTaskInput
	task     data.RuntimeTask
}

func (s *completedSandboxStore) Enqueue(_ context.Context, input data.EnqueueRuntimeTaskInput) (string, bool, error) {
	s.enqueued = input
	return "sandbox-1", false, nil
}
func (s *completedSandboxStore) Get(context.Context, string) (*data.RuntimeTask, error) {
	return &s.task, nil
}

func TestExecuteToolUsesCategorizedSandboxTask(t *testing.T) {
	resultJSON, _ := json.Marshal(sandboxExecResult{Stdout: "hello", Stderr: "warning", Code: 0})
	store := &completedSandboxStore{task: data.RuntimeTask{Status: "succeeded", Result: resultJSON}}
	tool := &coreToolContext{sandboxTasks: store, workspaceKey: "personal:alice", sessionID: "s1", scopeLabel: "personal:alice"}
	result, err := tool.Execute(t.Context(), ToolCall{ID: "call-1", Name: "execute", Arguments: json.RawMessage(`{"command":"pwd","purpose":"inspect"}`)})
	if err != nil || result.IsError || !strings.Contains(toolResultText(result), "hello") || !strings.Contains(toolResultText(result), "warning") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if store.enqueued.Kind != SandboxExecTaskKind || store.enqueued.SerialKey != "sandbox:personal:alice" || store.enqueued.IdempotencyKey != "tool:s1:call-1" {
		t.Fatalf("enqueued=%#v", store.enqueued)
	}
}

func TestLocalDockerSandboxNeverRunsCommandThroughHostShell(t *testing.T) {
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker-fake")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf 'container-output'\nprintf 'container-warning' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(sandboxExecPayload{WorkspaceKey: "personal:alice", Command: "echo $(host-danger)", Purpose: "test", TimeoutSec: 5})
	handler := (LocalDockerSandbox{WorkspaceRoot: t.TempDir(), DockerBinary: fakeDocker, Image: "test-image"}).Handler()
	raw, err := handler(t.Context(), data.RuntimeTask{ID: "task-1", Kind: SandboxExecTaskKind, PayloadVersion: 1, Payload: payload}, &collectedEvents{})
	if err != nil {
		t.Fatal(err)
	}
	var result sandboxExecResult
	if json.Unmarshal(raw, &result) != nil || result.Code != 7 || result.Stdout != "container-output" || result.Stderr != "container-warning" {
		t.Fatalf("result=%s", raw)
	}
}
