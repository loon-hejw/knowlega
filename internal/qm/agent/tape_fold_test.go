package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFoldTapeMatchesNodeCompactionAndInterruptSemantics(t *testing.T) {
	entryZero := 0
	entryTwo := 2
	rows := []TapeRecord{
		{Kind: "context_event", Payload: json.RawMessage(`{"event":"legacy_import","messages":[{"role":"user","content":[{"type":"text","text":"old"}]},{"role":"assistant","content":[{"type":"text","text":"answer"}]}]}`), CoversEntrySequence: &entryZero},
		{Kind: "message", Harness: "pi", Payload: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"new"}]}`)},
		{Kind: "message", Harness: "pi", Payload: json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}]}`)},
		{Kind: "annotation", Payload: json.RawMessage(`{"turnEnd":true}`), EntrySequence: &entryTwo},
		{Kind: "context_event", Payload: json.RawMessage(`{"event":"compaction","text":"summary"}`), CoversEntrySequence: &entryZero, CreatedAt: 100},
		{Kind: "context_event", Payload: json.RawMessage(`{"event":"interrupt"}`), CreatedAt: 101},
	}
	fold := FoldTape(rows)
	if len(fold) != 4 || !strings.Contains(string(fold[0]), "Earlier conversation summary") || tapeMessageRole(fold[3]) != "toolResult" {
		t.Fatalf("fold=%s", fold)
	}
	if lint := LintTapeFold(fold); !lint.OK {
		t.Fatalf("lint=%#v fold=%s", lint, fold)
	}
	plan := PlanTapeSeed(rows, "pi", "serve")
	if len(plan.Seed) != 4 || plan.Seed[3].Role != "tool" || plan.Seed[3].ToolCallID != "call-1" {
		t.Fatalf("plan=%#v", plan)
	}
}

func TestTapePlanRejectsForeignHarnessAndInvalidFold(t *testing.T) {
	foreign := PlanTapeSeed([]TapeRecord{{Kind: "message", Harness: "codex", Payload: json.RawMessage(`{"role":"user","content":"hello"}`)}}, "pi", "serve")
	if foreign.Skip != "foreign-harness" || foreign.Seed != nil {
		t.Fatalf("foreign=%#v", foreign)
	}
	invalid := PlanTapeSeed([]TapeRecord{{Kind: "message", Harness: "pi", Payload: json.RawMessage(`{"role":"assistant","content":"hello"}`)}}, "pi", "serve")
	if invalid.Lint.OK || invalid.Seed != nil {
		t.Fatalf("invalid=%#v", invalid)
	}
}

func TestPiAdapterServesValidTapeInsteadOfPayloadHistory(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{Text: "ok", Model: "model-pi", Request: json.RawMessage(`{}`)}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	history, _ := json.Marshal([]SessionEntry{{Type: "user", Payload: json.RawMessage(`{"text":"history"}`)}})
	rows := []TapeRecord{
		{Kind: "message", Harness: "pi", Payload: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"tape"}]}`)},
		{Kind: "message", Harness: "pi", Payload: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"answer"}]}`)},
	}
	_, err := adapter.RunTurn(t.Context(), TurnInput{
		SessionID: "session-1", Input: "next", SystemPrompt: "system", ScopeLabel: "personal:a", OrgScopeID: "org:a", Model: "model-pi",
		History: history, TapeRows: rows, TapeMode: "serve",
		Emit: func(context.Context, NewEntry) (SessionEntry, error) { return SessionEntry{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := transport.requests[0].Messages
	want := []string{"tape", "answer", "next"}
	texts := make([]string, len(got))
	for index := range got {
		texts[index] = got[index].Content
	}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("messages=%#v", got)
	}
}
