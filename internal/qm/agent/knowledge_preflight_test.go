package agent

import (
	"context"
	"strings"
	"testing"
)

type noCallKnowledgeTools struct{ calls int }

func (*noCallKnowledgeTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return nil, nil
}
func (t *noCallKnowledgeTools) Execute(context.Context, ToolCall) (ToolResult, error) {
	t.calls++
	return toolText("unexpected"), nil
}

func TestProjectKnowledgePreparerAddsStaticInstructionsWithoutCallingTools(t *testing.T) {
	tools := &noCallKnowledgeTools{}
	input := TurnInput{Input: "唐僧是谁？", ScopeLabel: "group:web-project-123", Tools: tools}
	result, err := NewProjectKnowledgePreparer().PrepareTurnInput(t.Context(), &input)
	if err != nil || result != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != 0 {
		t.Fatalf("preparer called tools %d times", tools.calls)
	}
	for _, want := range []string{"single knowledge tool", "Search snippets", "common grammatical subject", "action=discover", "no prescribed sequence", "dedicated candidate entity page is not required", "reports pending", "raw sources", "do not guess a candidate", "action=submit", "returned status to be complete", "validation_issues", "present its candidate as the answer", "question, answer, candidate", "positive event", "unrelated extra conditions", "not enough for supported", "writeback"} {
		if !strings.Contains(input.Environment, want) {
			t.Fatalf("environment missing %q: %s", want, input.Environment)
		}
	}
}

func TestProjectKnowledgePreparerIgnoresNonProjectScope(t *testing.T) {
	tools := &noCallKnowledgeTools{}
	input := TurnInput{Input: "hello", ScopeLabel: "personal:alice", Tools: tools, Environment: "base"}
	if result, err := NewProjectKnowledgePreparer().PrepareTurnInput(t.Context(), &input); err != nil || result != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if input.Environment != "base" || tools.calls != 0 {
		t.Fatalf("environment=%q calls=%d", input.Environment, tools.calls)
	}
}
