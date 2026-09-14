package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type rendezvousTools struct {
	started chan struct{}
	release chan struct{}
}

func (t *rendezvousTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return nil, nil
}
func (t *rendezvousTools) Execute(ctx context.Context, c ToolCall) (ToolResult, error) {
	t.started <- struct{}{}
	select {
	case <-t.release:
		return toolText(c.ID), nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}
func TestPiIndependentToolsRunInParallel(t *testing.T) {
	tools := &rendezvousTools{started: make(chan struct{}, 2), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() { <-tools.started; <-tools.started; close(tools.release) }()
	calls := []ToolCall{{ID: "1", Name: "read", Arguments: json.RawMessage(`{}`)}, {ID: "2", Name: "read", Arguments: json.RawMessage(`{}`)}}
	results, err := parallelPiTools(ctx, TurnInput{Tools: tools, Emit: func(context.Context, NewEntry) (SessionEntry, error) { return SessionEntry{}, nil }}, []ToolDefinition{{Name: "read", ExecutionMode: "parallel"}}, calls, 2, false)
	if err != nil || len(results) != 2 || results[0].IsError || toolResultText(results[1]) != "2" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	results, err = parallelPiTools(ctx, TurnInput{Tools: tools}, []ToolDefinition{{Name: "read", ExecutionMode: "sequential"}}, calls, 2, false)
	if err != nil || results != nil {
		t.Fatalf("sequential tool was prefetched")
	}
}
