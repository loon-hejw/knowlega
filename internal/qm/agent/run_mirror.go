package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/worker"
)

type TurnRunMirror interface {
	StartRuntime(context.Context, string) error
	CompleteRuntime(context.Context, string, json.RawMessage) error
	FailRuntime(context.Context, string, string, bool) error
}

func MirrorTurnRuns(handler worker.Handler, runs TurnRunMirror) worker.Handler {
	return func(ctx context.Context, task data.RuntimeTask, events worker.EventWriter) (json.RawMessage, error) {
		if handler == nil || runs == nil {
			return nil, worker.Permanent(errors.New("turn handler and run mirror are required"))
		}
		var payload TurnTaskPayload
		if err := json.Unmarshal(task.Payload, &payload); err != nil || payload.RunID == "" {
			return nil, worker.Permanent(errors.New("turn task is missing its run id"))
		}
		if err := runs.StartRuntime(ctx, payload.RunID); err != nil {
			if errors.Is(err, data.ErrRuntimeRunNotActive) {
				return nil, worker.Permanent(err)
			}
			return nil, worker.Retry(err, 0)
		}
		result, err := handler(ctx, task, events)
		if err != nil {
			terminal := task.Attempts >= task.MaxAttempts
			var taskErr *worker.TaskError
			if errors.As(err, &taskErr) && !taskErr.Retry {
				terminal = true
			}
			if mirrorErr := runs.FailRuntime(context.WithoutCancel(ctx), payload.RunID, err.Error(), terminal); mirrorErr != nil {
				if errors.Is(mirrorErr, data.ErrRuntimeRunNotActive) {
					return nil, worker.Permanent(err)
				}
				return nil, worker.Retry(errors.Join(err, mirrorErr), 0)
			}
			return nil, err
		}
		if err := runs.CompleteRuntime(context.WithoutCancel(ctx), payload.RunID, result); err != nil {
			if errors.Is(err, data.ErrRuntimeRunNotActive) {
				return nil, worker.Permanent(err)
			}
			return nil, worker.Retry(err, 0)
		}
		return result, nil
	}
}
