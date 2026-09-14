package agent

import (
	"context"
	"fmt"
	"sync"
)

// parallelPiTools only runs explicitly independent tools. A sequential member
// makes the entire batch sequential, matching Pi's tool execution contract.
func parallelPiTools(ctx context.Context, input TurnInput, definitions []ToolDefinition, calls []ToolCall, allowed int, truncated bool) ([]ToolResult, error) {
	if truncated || len(calls) < 2 || allowed < len(calls) || input.Tools == nil || input.ToolApprovalGate != nil {
		return nil, nil
	}
	modes := map[string]string{}
	for _, definition := range definitions {
		modes[definition.Name] = definition.ExecutionMode
	}
	for _, call := range calls {
		if modes[call.Name] != "parallel" || validatePiToolCall(call, definitions) != nil {
			return nil, nil
		}
	}
	for _, call := range calls {
		if _, err := input.Emit(ctx, NewEntry{Type: "tool_call", Payload: callPayloadForPi(call), ScopeLabel: input.ScopeLabel}); err != nil {
			return nil, err
		}
	}
	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call ToolCall) {
			defer wg.Done()
			result, err := input.Tools.Execute(ctx, call)
			if err != nil {
				result = toolError(fmt.Sprintf("[tool failed] %v", err))
			}
			results[i] = result
		}(i, call)
	}
	wg.Wait()
	return results, nil
}
