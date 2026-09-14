package agent

import (
	"strings"
	"testing"
)

func TestPiContextCompactionPreservesWholeRecentToolBatch(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{Text: "User asks for nine conditions. Candidate A was rejected. Re-read raw/sources/ch12.txt for the remaining condition."}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	original := strings.Repeat("original evidence ", 2000)
	messages := []PiMessage{{Role: "user", Content: "question"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "old", Name: "knowledge"}}}, {Role: "tool", ToolCallID: "old", Content: original}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "recent", Name: "knowledge"}}}, {Role: "tool", ToolCallID: "recent", Content: "recent evidence"}}
	got, compaction, err := adapter.preparePiContext(t.Context(), "model-pi", TurnInput{}, messages, 4096)
	if err != nil || compaction == nil || len(got) != 3 || got[1].ToolCalls[0].ID != "recent" || got[2].Content != "recent evidence" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if messages[2].Content != original {
		t.Fatal("durable message was mutated")
	}
	if len(transport.requests[0].Tools) != 0 {
		t.Fatal("compaction may not run task tools")
	}
}
func TestPiContextLeavesSmallConversationUntouched(t *testing.T) {
	adapter, _ := NewPiAdapter(piTestModels(), &fakePiTransport{})
	got, compaction, err := adapter.preparePiContext(t.Context(), "model-pi", TurnInput{}, []PiMessage{{Role: "user", Content: "question"}}, 4096)
	if err != nil || compaction != nil || got[0].Content != "question" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
