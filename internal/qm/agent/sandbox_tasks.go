package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/worker"
)

const SandboxExecTaskKind = "sandbox.exec"

type SandboxTaskStore interface {
	Enqueue(context.Context, data.EnqueueRuntimeTaskInput) (string, bool, error)
	Get(context.Context, string) (*data.RuntimeTask, error)
}

type sandboxExecPayload struct {
	WorkspaceKey string `json:"workspaceKey"`
	Command      string `json:"command"`
	Purpose      string `json:"purpose"`
	TimeoutSec   int    `json:"timeoutSec"`
}

type sandboxExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Code     int    `json:"code"`
	TimedOut bool   `json:"timedOut"`
}

func (t *coreToolContext) executeSandbox(ctx context.Context, call ToolCall) (ToolResult, error) {
	if t.readOnly {
		return toolError("[this is a read-only wake — commands cannot be executed here]"), nil
	}
	if t.sandboxTasks == nil {
		return toolError("[execute unavailable] no Go sandbox executor is configured for this workspace"), nil
	}
	var input struct {
		Command    string `json:"command"`
		Purpose    string `json:"purpose"`
		TimeoutSec int    `json:"timeout_seconds"`
	}
	if decodeToolArguments(call.Arguments, &input) != nil || strings.TrimSpace(input.Command) == "" || strings.TrimSpace(input.Purpose) == "" {
		return toolError("[error] execute requires `command` and `purpose`."), nil
	}
	if input.TimeoutSec <= 0 {
		input.TimeoutSec = 600
	}
	if input.TimeoutSec > 3600 {
		input.TimeoutSec = 3600
	}
	payload, _ := json.Marshal(sandboxExecPayload{WorkspaceKey: t.workspaceKey, Command: input.Command, Purpose: input.Purpose, TimeoutSec: input.TimeoutSec})
	id, _, err := t.sandboxTasks.Enqueue(ctx, data.EnqueueRuntimeTaskInput{Kind: SandboxExecTaskKind, PayloadVersion: 1, Payload: payload, ScopeID: t.scopeLabel, IdempotencyKey: "tool:" + t.sessionID + ":" + call.ID, SerialKey: "sandbox:" + t.workspaceKey, MaxAttempts: 2})
	if err != nil {
		return ToolResult{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := t.sandboxTasks.Get(ctx, id)
		if err != nil {
			return ToolResult{}, err
		}
		if task == nil {
			return ToolResult{}, errors.New("sandbox task disappeared")
		}
		switch task.Status {
		case "succeeded":
			var result sandboxExecResult
			if json.Unmarshal(task.Result, &result) != nil {
				return ToolResult{}, errors.New("sandbox returned an invalid result")
			}
			text := result.Stdout
			if result.Stderr != "" {
				text += "\n[stderr]\n" + result.Stderr
			}
			text += "\n[exit code " + strconv.Itoa(result.Code) + "]"
			if result.TimedOut {
				text += " [timed out]"
			}
			out := toolText(capToolText(strings.TrimSpace(text)))
			out.IsError = result.Code != 0 || result.TimedOut
			return out, nil
		case "failed", "dead", "cancelled":
			return toolError("[execute failed] " + stringValue(task.LastError)), nil
		}
		select {
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

type LocalDockerSandbox struct {
	WorkspaceRoot string
	DockerBinary  string
	Image         string
}

func (s LocalDockerSandbox) Handler() worker.Handler {
	return func(ctx context.Context, task data.RuntimeTask, events worker.EventWriter) (json.RawMessage, error) {
		if task.Kind != SandboxExecTaskKind || task.PayloadVersion != 1 {
			return nil, worker.Permanent(fmt.Errorf("unsupported sandbox task %s version %d", task.Kind, task.PayloadVersion))
		}
		var payload sandboxExecPayload
		if json.Unmarshal(task.Payload, &payload) != nil || strings.TrimSpace(payload.WorkspaceKey) == "" || strings.TrimSpace(payload.Command) == "" {
			return nil, worker.Permanent(errors.New("malformed sandbox exec task"))
		}
		root, err := sandboxWorkspaceRoot(s.WorkspaceRoot, payload.WorkspaceKey)
		if err != nil {
			return nil, worker.Permanent(err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, worker.Retry(err, 0)
		}
		timeout := payload.TimeoutSec
		if timeout <= 0 {
			timeout = 600
		}
		runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		docker := firstNonEmpty(strings.TrimSpace(s.DockerBinary), "docker")
		image := firstNonEmpty(strings.TrimSpace(s.Image), "qm-sandbox-local:latest")
		name := "qm-task-" + strings.ReplaceAll(task.ID, "-", "")
		if len(name) > 50 {
			name = name[:50]
		}
		command := exec.CommandContext(runCtx, docker, "run", "--rm", "--name", name, "--label", "qm.sandbox=1", "-v", root+":/root/workspace", "-w", "/root/workspace", image, "/bin/sh", "-lc", payload.Command)
		var stdout, stderr cappedBuffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err = command.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				code = 124
			} else {
				return nil, worker.Retry(err, time.Second)
			}
		}
		encoded, _ := json.Marshal(sandboxExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code, TimedOut: errors.Is(runCtx.Err(), context.DeadlineExceeded)})
		return encoded, nil
	}
}

func sandboxWorkspaceRoot(root, key string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || strings.TrimSpace(root) == "" {
		return "", errors.New("agent workspace root is required")
	}
	return filepath.Join(abs, safeWorkspaceKey(key)), nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(value []byte) (int, error) {
	const limit = 1 << 20
	original := len(value)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}
