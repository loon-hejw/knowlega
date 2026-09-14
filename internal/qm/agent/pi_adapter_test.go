package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type fakePiTransport struct {
	requests    []PiCompletionRequest
	completions []PiCompletion
	err         error
}

type blockingStreamPiTransport struct {
	release chan struct{}
}

func (t *blockingStreamPiTransport) Complete(_ context.Context, _ PiCompletionRequest, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	if onTextBlockStart != nil {
		onTextBlockStart()
	}
	if onDelta != nil {
		onDelta("streamed")
	}
	<-t.release
	return PiCompletion{Text: "streamed"}, nil
}

func (*blockingStreamPiTransport) Close(context.Context) error { return nil }

type sequencedPiTransport struct {
	requests []PiCompletionRequest
	steps    []struct {
		completion PiCompletion
		err        error
	}
}

func (t *sequencedPiTransport) Complete(_ context.Context, request PiCompletionRequest, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	t.requests = append(t.requests, request)
	if len(t.steps) == 0 {
		return PiCompletion{}, errors.New("unexpected extra model request")
	}
	step := t.steps[0]
	t.steps = t.steps[1:]
	if step.err != nil {
		return PiCompletion{}, step.err
	}
	if step.completion.Text != "" {
		if onTextBlockStart != nil {
			onTextBlockStart()
		}
		if onDelta != nil {
			onDelta(step.completion.Text)
		}
	}
	return step.completion, nil
}

func (*sequencedPiTransport) Close(context.Context) error { return nil }

func (t *fakePiTransport) Complete(_ context.Context, request PiCompletionRequest, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	t.requests = append(t.requests, request)
	if t.err != nil {
		return PiCompletion{}, t.err
	}
	completion := t.completions[0]
	t.completions = t.completions[1:]
	if completion.Text != "" {
		if onTextBlockStart != nil {
			onTextBlockStart()
		}
		if onDelta != nil {
			onDelta(completion.Text)
		}
	}
	return completion, nil
}

func (*fakePiTransport) Close(context.Context) error { return nil }

func TestPiAdapterStreamsNonKnowledgeDeltaBeforeCompletionReturns(t *testing.T) {
	transport := &blockingStreamPiTransport{release: make(chan struct{})}
	adapter, err := NewPiAdapter(piTestModels(), transport)
	if err != nil {
		t.Fatal(err)
	}
	seenDelta := make(chan string, 1)
	seenBlock := make(chan struct{}, 1)
	resultCh := make(chan TurnResult, 1)
	errCh := make(chan error, 1)
	released := false
	release := func() {
		if !released {
			close(transport.release)
			released = true
		}
	}
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), TurnInput{
			SessionID: "session-streaming", Input: "hello", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
			OnDelta:          func(delta string) { seenDelta <- delta },
			OnTextBlockStart: func() { seenBlock <- struct{}{} },
			Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
				return SessionEntry{Sequence: 1, Type: entry.Type}, nil
			},
		})
		resultCh <- result
		errCh <- runErr
	}()
	defer release()
	select {
	case delta := <-seenDelta:
		if delta != "streamed" {
			t.Fatalf("delta=%q", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("non-Knowledge delta was not forwarded before Complete returned")
	}
	select {
	case <-seenBlock:
	case <-time.After(time.Second):
		t.Fatal("text block start was not forwarded before Complete returned")
	}
	release()
	select {
	case result := <-resultCh:
		if result.Reply != "streamed" {
			t.Fatalf("result=%+v", result)
		}
		if runErr := <-errCh; runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not finish after releasing provider")
	}
}

type fakePiTools struct {
	calls []ToolCall
}

func (*fakePiTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *fakePiTools) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	t.calls = append(t.calls, call)
	return ToolResult{Content: []ToolContent{{Type: "text", Text: "file contents"}}}, nil
}

type knowledgeStallTools struct {
	calls int
}

func (*knowledgeStallTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeStallTools) Execute(context.Context, ToolCall) (ToolResult, error) {
	t.calls++
	return toolTextWithDetails(`{"status":"incomplete","unresolved_requirement_ids":["9"],"validation_issues":[{"code":"negative_search_not_recorded","requirement_id":"9"}]}`, json.RawMessage(`{"kind":"knowledge","action":"submit","sources":[]}`)), nil
}

type knowledgeCompleteTools struct {
	calls int
}

func (*knowledgeCompleteTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeCompleteTools) Execute(context.Context, ToolCall) (ToolResult, error) {
	t.calls++
	return toolTextWithDetails(`{"status":"complete","answer":"答案：唐太宗。","candidate":"唐太宗"}`, json.RawMessage(`{"kind":"knowledge","action":"submit","status":"complete","sources":[]}`)), nil
}

type knowledgeReadTools struct {
	calls int
}

func (*knowledgeReadTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeReadTools) Execute(context.Context, ToolCall) (ToolResult, error) {
	t.calls++
	return toolTextWithDetails(`{"path":"wiki/entities/example.md"}`, json.RawMessage(`{"kind":"knowledge","action":"read","sources":[{"path":"wiki/entities/example.md","evidence":true}]}`)), nil
}

type knowledgeNavigationTools struct {
	calls int
}

func (*knowledgeNavigationTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeNavigationTools) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	t.calls++
	switch piKnowledgeAction(call) {
	case "read":
		return toolTextWithDetails(`{"path":"wiki/evidence.md"}`, json.RawMessage(`{"kind":"knowledge","action":"read","sources":[{"path":"wiki/evidence.md","evidence":true}]}`)), nil
	case "submit":
		return toolTextWithDetails(`{"status":"complete","answer":"答案：候选。","candidate":"候选"}`, json.RawMessage(`{"kind":"knowledge","action":"submit","status":"complete","sources":[]}`)), nil
	default:
		return toolTextWithDetails(`[]`, json.RawMessage(`{"kind":"knowledge","action":"search","sources":[]}`)), nil
	}
}

func knowledgeSearchCalls(count int) []ToolCall {
	calls := make([]ToolCall, 0, count)
	for index := 0; index < count; index++ {
		calls = append(calls, ToolCall{ID: fmt.Sprintf("search-%d", index), Name: "knowledge", Arguments: json.RawMessage(fmt.Sprintf(`{"action":"search","query":"query-%d"}`, index))})
	}
	return calls
}

func knowledgeReadCalls(count int) []ToolCall {
	calls := make([]ToolCall, 0, count)
	for index := 0; index < count; index++ {
		calls = append(calls, ToolCall{ID: fmt.Sprintf("read-%d", index), Name: "knowledge", Arguments: json.RawMessage(fmt.Sprintf(`{"action":"read","path":"wiki/evidence-%d.md"}`, index))})
	}
	return calls
}

func TestPiAdapterRunsToolLoopAndPersistsTranscript(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{Text: "I will inspect it.", Thinking: "Need evidence.", Model: "model-pi", ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}, Request: json.RawMessage(`{"step":0}`), Usage: PiUsage{Input: 20, Output: 5, CacheRead: 4}},
		{Text: "The file says hello.", Model: "model-pi", Request: json.RawMessage(`{"step":1}`), Usage: PiUsage{Input: 30, Output: 6, CacheRead: 10}},
	}}
	adapter, err := NewPiAdapter(piTestModels(), transport)
	if err != nil {
		t.Fatal(err)
	}
	tools := &fakePiTools{}
	entries := []NewEntry{}
	requests := []LLMRequestRecord{}
	tape := []TapeRecord{}
	deltas := []string{}
	blocks := 0
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "What is in the file?", SystemPrompt: "Be helpful.", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Images: []Image{{MIMEType: "image/png", DataBase64: "secret-image-bytes"}},
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			sequence := len(entries) - 1
			return SessionEntry{SessionID: "session-1", Sequence: sequence, Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel}, nil
		},
		RecordLLMRequest: func(_ context.Context, request LLMRequestRecord) error {
			requests = append(requests, request)
			return nil
		},
		Tape: func(_ context.Context, record TapeRecord) error {
			tape = append(tape, record)
			return nil
		},
		OnDelta: func(delta string) { deltas = append(deltas, delta) }, OnTextBlockStart: func() { blocks++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "The file says hello." || result.ModelCalls != 2 || len(tools.calls) != 1 || len(requests) != 2 || blocks != 2 {
		t.Fatalf("result=%#v tools=%#v requests=%#v blocks=%d", result, tools.calls, requests, blocks)
	}
	types := make([]string, 0, len(entries))
	for _, entry := range entries {
		types = append(types, entry.Type)
	}
	wantTypes := []string{"user", "thinking", "text", "tool_call", "tool_result", "assistant"}
	if !reflect.DeepEqual(types, wantTypes) || !reflect.DeepEqual(deltas, []string{"I will inspect it.", "The file says hello."}) {
		t.Fatalf("types=%#v deltas=%#v", types, deltas)
	}
	if len(transport.requests) != 2 || len(transport.requests[1].Messages) < 3 || transport.requests[1].Messages[len(transport.requests[1].Messages)-1].Role != "tool" {
		t.Fatalf("transport requests=%#v", transport.requests)
	}
	if len(tape) != 5 || tape[0].Kind != "message" || tape[0].Harness != "pi" || strings.Contains(string(tape[0].Payload), transport.requests[0].Messages[len(transport.requests[0].Messages)-1].Images[0].DataBase64) || tape[4].Kind != "annotation" {
		t.Fatalf("tape=%#v", tape)
	}
}

func TestCompactPiMessagesBoundsOldToolOutputsAndPreservesRecentEvidence(t *testing.T) {
	messages := make([]PiMessage, 0, 40)
	for index := 0; index < 40; index++ {
		messages = append(messages, PiMessage{Role: "tool", Content: strings.Repeat(fmt.Sprintf("evidence-%d ", index), 1000)})
	}
	compacted := compactPiMessages(messages, 24000)
	if got := piMessageChars(compacted); got > 24000 {
		t.Fatalf("compacted messages exceed limit: %d", got)
	}
	if !strings.Contains(compacted[len(compacted)-1].Content, "evidence-39") {
		t.Fatalf("recent evidence was lost: %q", compacted[len(compacted)-1].Content)
	}
	if !strings.Contains(compacted[0].Content, "earlier tool output compacted") {
		t.Fatalf("old tool output was not compacted: %q", compacted[0].Content)
	}
}

func TestPiAdapterPausesBeforeUnapprovedTool(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{}`)}}, Request: json.RawMessage(`{}`)}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &fakePiTools{}
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "read", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		ToolApprovalGate: func(string) bool { return false },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || !result.PausedOnApproval || len(result.PendingApprovals) != 1 || len(tools.calls) != 0 {
		t.Fatalf("result=%#v calls=%#v err=%v", result, tools.calls, err)
	}
	if got := []string{entries[0].Type, entries[1].Type, entries[2].Type, entries[3].Type}; !reflect.DeepEqual(got, []string{"user", "tool_call", "tool_result", "assistant"}) {
		t.Fatalf("entries=%#v", entries)
	}
}

func TestPiAdapterClassifiesProviderConfigurationErrors(t *testing.T) {
	transport := &fakePiTransport{err: &piProviderError{status: 400, body: "bad request"}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	_, err := adapter.RunTurn(context.Background(), TurnInput{SessionID: "s", Input: "hi", SystemPrompt: "system", ScopeLabel: "personal:a", OrgScopeID: "org:a", Model: "model-pi", Emit: func(context.Context, NewEntry) (SessionEntry, error) { return SessionEntry{}, nil }})
	var permanent *NonRetryableError
	if !errors.As(err, &permanent) {
		t.Fatalf("err=%v", err)
	}
}

func TestPiAdapterRepromptsEmptyInitialAddressedResponse(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{Thinking: "reasoning without a final answer", Model: "model-pi", Request: json.RawMessage(`{"step":0}`)},
		{Text: "Recovered final answer.", Model: "model-pi", Request: json.RawMessage(`{"step":1}`)},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "answer me", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || result.Reply != "Recovered final answer." || result.ModelCalls != 2 || len(transport.requests) != 2 {
		t.Fatalf("result=%#v requests=%#v err=%v", result, transport.requests, err)
	}
	last := transport.requests[1].Messages[len(transport.requests[1].Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "ended with an empty message") {
		t.Fatalf("recovery prompt=%#v", last)
	}
	if got := []string{entries[0].Type, entries[1].Type, entries[2].Type}; !reflect.DeepEqual(got, []string{"user", "thinking", "assistant"}) {
		t.Fatalf("entries=%#v", entries)
	}
}

func TestPiAdapterBudgetProducesIncompleteResultInsteadOfEmptyFailure(t *testing.T) {
	models := piTestModels()
	models.Harnesses[0].Runtime.SoftModelCalls = 1
	models.Harnesses[0].Runtime.MaxModelCalls = 1
	models.Harnesses[0].Runtime.MaxToolCalls = 1
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}, Request: json.RawMessage(`{"step":0}`)},
		{Text: "当前只完成了部分核验，仍有条件未确认。", Model: "model-pi", Request: json.RawMessage(`{"final":true}`)},
	}}
	adapter, _ := NewPiAdapter(models, transport)
	tools := &fakePiTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.CompletionStatus != "incomplete" || result.Status != "incomplete" || result.Reply == "" || result.ToolCalls != 1 || result.ModelCalls != 1 || len(transport.requests) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPiAdapterHardModelLimitIncludesFallbackAndDoesNotFinalizePastCap(t *testing.T) {
	models := piTestModelsWithFallback()
	models.Harnesses[0].Runtime.MaxModelCalls = 1
	models.Harnesses[0].Runtime.MaxToolCalls = 10
	transport := &sequencedPiTransport{steps: []struct {
		completion PiCompletion
		err        error
	}{{completion: PiCompletion{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}}}}}
	adapter, _ := NewPiAdapter(models, transport)
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-hard-cap", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: &fakePiTools{},
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.ModelCalls != 1 || len(transport.requests) != 1 || result.Reply == "" {
		t.Fatalf("result=%+v requests=%d err=%v", result, len(transport.requests), err)
	}
}

func TestPiAdapterFallbackCountsAgainstHardModelLimit(t *testing.T) {
	models := piTestModelsWithFallback()
	models.Harnesses[0].Runtime.MaxModelCalls = 2
	transport := &sequencedPiTransport{steps: []struct {
		completion PiCompletion
		err        error
	}{
		{err: errors.New("primary unavailable")},
		{completion: PiCompletion{Text: "fallback partial result", Model: "fallback-pi"}},
	}}
	adapter, _ := NewPiAdapter(models, transport)
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-fallback-cap", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.FallbackModel != "fallback-pi" || result.ModelCalls != 2 || len(transport.requests) != 2 || result.Reply != "fallback partial result" {
		t.Fatalf("result=%+v requests=%d err=%v", result, len(transport.requests), err)
	}
}

func TestPiAdapterPersistsIncompleteResultWhenProviderTimesOut(t *testing.T) {
	transport := &fakePiTransport{err: context.DeadlineExceeded}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-timeout", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" || strings.TrimSpace(result.Reply) == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.ToolCalls != 0 || result.ModelCalls < 1 || len(entries) == 0 || entries[len(entries)-1].Type != "assistant" {
		t.Fatalf("result=%+v entries=%#v", result, entries)
	}
}

func TestPiAdapterStopsAtToolBudgetBeforeStartingAnotherToolStep(t *testing.T) {
	models := piTestModels()
	models.Harnesses[0].Runtime.MaxModelCalls = 4
	models.Harnesses[0].Runtime.MaxToolCalls = 1
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}, Request: json.RawMessage(`{"step":0}`)},
		{Text: "基于已读取证据给出部分结论。", Request: json.RawMessage(`{"final":true}`)},
	}}
	adapter, _ := NewPiAdapter(models, transport)
	tools := &fakePiTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-tool-budget", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.CompletionStatus != "incomplete" || result.Status != "incomplete" || result.Reply == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(tools.calls) != 1 || len(transport.requests) != 2 || len(transport.requests[1].Tools) != 0 {
		t.Fatalf("tool calls=%d requests=%#v", len(tools.calls), transport.requests)
	}
}

func TestPiAdapterCapsMultipleToolCallsAtBudget(t *testing.T) {
	models := piTestModels()
	models.Harnesses[0].Runtime.MaxModelCalls = 4
	models.Harnesses[0].Runtime.MaxToolCalls = 1
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{
			{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"one"}`)},
			{ID: "call-2", Name: "read", Arguments: json.RawMessage(`{"path":"two"}`)},
		}},
		{Text: "已停止扩展搜索并总结。"},
	}}
	adapter, _ := NewPiAdapter(models, transport)
	tools := &fakePiTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-multi-tool-budget", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.Reply == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(tools.calls) != 1 || result.ToolCalls != 1 || len(transport.requests) != 2 || len(transport.requests[1].Tools) != 0 {
		t.Fatalf("tool calls=%d result=%+v requests=%#v", len(tools.calls), result, transport.requests)
	}
}

func TestPiAdapterSkipsRepeatedIdenticalToolActions(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}},
		{ToolCalls: []ToolCall{{ID: "call-2", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}},
		{ToolCalls: []ToolCall{{ID: "call-3", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}}},
		{Text: "完成。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &fakePiTools{}
	progress := []Progress{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "核验", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		OnProgress: func(value Progress) { progress = append(progress, value) },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Reply != "完成。" || len(tools.calls) != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, len(tools.calls), err)
	}
	stalled := false
	for _, value := range progress {
		stalled = stalled || value.Stalled
	}
	if !stalled {
		t.Fatalf("progress did not report the repeated-action redirect: %+v", progress)
	}
}

func TestPiAdapterStopsWhenKnowledgeValidationDoesNotAdvance(t *testing.T) {
	if got := piKnowledgeAction(ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit"}`)}); got != "submit" {
		t.Fatalf("knowledge action=%q", got)
	}
	if !piKnowledgeSubmitIncomplete(knowledgeStallToolsResult()) {
		t.Fatal("incomplete knowledge result was not detected")
	}
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "submit-1", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q1"}`)}}},
		{ToolCalls: []ToolCall{{ID: "submit-2", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q2"}`)}}},
		{ToolCalls: []ToolCall{{ID: "submit-3", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q3"}`)}}},
		{Text: "答案：金鼻白毛老鼠精。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeStallTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-stall", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" || result.Reply == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != knowledgeSubmitStallLimit || len(transport.requests) != knowledgeSubmitStallLimit {
		t.Fatalf("stall guard calls=%d requests=%d", tools.calls, len(transport.requests))
	}
	if !strings.Contains(result.Reason, "validation did not advance") {
		t.Fatalf("reason=%q", result.Reason)
	}
	if strings.Contains(result.Reply, "金鼻白毛老鼠精") || !strings.Contains(result.Reply, "尚未确认的条件：9") || strings.Contains(result.Reply, "negative_search_not_recorded") {
		t.Fatalf("unsafe incomplete reply=%q", result.Reply)
	}
}

func TestPiAdapterEvaluatesKnowledgeProgressAfterEntireToolBatch(t *testing.T) {
	calls := knowledgeSearchCalls(knowledgeNoProgressLimit)
	calls = append(calls, ToolCall{ID: "read-after-searches", Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/evidence.md"}`)})
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: calls},
		{ToolCalls: []ToolCall{{ID: "submit-after-read", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：候选。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeNavigationTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-batch-progress", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.Reply != "答案：候选。" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != knowledgeNoProgressLimit+2 || len(transport.requests) != 2 {
		t.Fatalf("calls=%d requests=%d", tools.calls, len(transport.requests))
	}
	for _, message := range transport.requests[1].Messages {
		if strings.Contains(message.Content, "Knowledge actions have not added evidence") {
			t.Fatalf("late evidence action should clear the batch stall: %#v", transport.requests[1].Messages)
		}
	}
}

func TestPiAdapterPromptsForSubmitBeforeStoppingStalledKnowledgeNavigation(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: knowledgeSearchCalls(knowledgeNoProgressLimit)},
		{ToolCalls: []ToolCall{{ID: "submit-after-redirect", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：候选。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeNavigationTools{}
	progress := []Progress{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-submit-redirect", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		OnProgress: func(value Progress) { progress = append(progress, value) },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.Reply != "答案：候选。" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != knowledgeNoProgressLimit+1 || len(transport.requests) != 2 {
		t.Fatalf("calls=%d requests=%d", tools.calls, len(transport.requests))
	}
	lastMessage := transport.requests[1].Messages[len(transport.requests[1].Messages)-1]
	if lastMessage.Role != "user" || !strings.Contains(lastMessage.Content, "call knowledge action=submit") {
		t.Fatalf("redirect=%#v", lastMessage)
	}
	redirected := false
	for _, value := range progress {
		redirected = redirected || (value.Stalled && value.Phase == "finalizing" && value.Strategy == "submit")
	}
	if !redirected {
		t.Fatalf("progress did not expose submit redirect: %+v", progress)
	}
}

func TestPiAdapterDoesNotForceSubmitWhileEvidenceAdvances(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: knowledgeReadCalls((knowledgeNoProgressLimit * 2))},
		{ToolCalls: []ToolCall{{ID: "submit-after-evidence-limit", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：候选。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeNavigationTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-pre-submit-limit", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.Reply != "答案：候选。" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != (knowledgeNoProgressLimit*2)+1 || len(transport.requests) != 2 {
		t.Fatalf("calls=%d requests=%d", tools.calls, len(transport.requests))
	}
	for _, message := range transport.requests[1].Messages {
		if strings.Contains(message.Content, "Knowledge actions have not added evidence") {
			t.Fatalf("evidence collection triggered a premature submit reminder: %#v", message)
		}
	}
}

func TestPiAdapterCandidateSearchesAdvanceIncompleteSubmitRepair(t *testing.T) {
	searches := make([]ToolCall, 0, knowledgeNoProgressLimit+2)
	for index := 0; index < knowledgeNoProgressLimit+2; index++ {
		searches = append(searches, ToolCall{
			ID:   fmt.Sprintf("candidate-search-%d", index),
			Name: "knowledge",
			Arguments: json.RawMessage(fmt.Sprintf(
				`{"action":"search","query":"condition-%d","candidate":"候选","requirement_id":"r%d"}`,
				index, index,
			)),
		})
	}
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "submit-incomplete", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{ToolCalls: searches},
		{ToolCalls: []ToolCall{{ID: "submit-complete", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：候选。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeRepairSequenceTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-candidate-search-repair", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.Reply != "答案：候选。" || len(transport.requests) != 3 {
		t.Fatalf("candidate repair did not converge: result=%+v requests=%d err=%v", result, len(transport.requests), err)
	}
}

type knowledgeRepairSequenceTools struct{ submits int }

func (*knowledgeRepairSequenceTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeRepairSequenceTools) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	if piKnowledgeAction(call) == "submit" {
		t.submits++
		if t.submits == 1 {
			return toolTextWithDetails(`{"status":"incomplete","unresolved_requirement_ids":["r1"]}`, json.RawMessage(`{"kind":"knowledge","action":"submit","status":"incomplete","sources":[]}`)), nil
		}
		return toolTextWithDetails(`{"status":"complete","answer":"答案：候选。","candidate":"候选"}`, json.RawMessage(`{"kind":"knowledge","action":"submit","status":"complete","sources":[]}`)), nil
	}
	return toolTextWithDetails(`[]`, json.RawMessage(`{"kind":"knowledge","action":"search","sources":[]}`)), nil
}

func TestPiAdapterReplacesCandidateReturnedAfterIncompleteKnowledgeSubmit(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "submit-1", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：金鼻白毛老鼠精。"},
		{Text: "答案仍是金鼻白毛老鼠精。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeStallTools{}
	var deltas []string
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-incomplete", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		OnDelta: func(delta string) { deltas = append(deltas, delta) },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Reply, "金鼻白毛老鼠精") || len(deltas) != 0 {
		t.Fatalf("reply=%q deltas=%#v", result.Reply, deltas)
	}
	if !strings.Contains(result.Reply, "尚未确认的条件：9") || strings.Contains(result.Reply, "negative_search_not_recorded") {
		t.Fatalf("incomplete details missing from reply=%q", result.Reply)
	}
}

func TestPiAdapterBuffersCandidateUntilKnowledgeSubmitIsValidated(t *testing.T) {
	models := piTestModels()
	models.Harnesses[0].Runtime.MaxModelCalls = 1
	models.Harnesses[0].Runtime.MaxToolCalls = 1
	transport := &fakePiTransport{completions: []PiCompletion{{
		Text:      "答案：金鼻白毛老鼠精。",
		ToolCalls: []ToolCall{{ID: "submit-inline", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit"}`)}},
	}}}
	adapter, _ := NewPiAdapter(models, transport)
	tools := &knowledgeStallTools{}
	deltas := []string{}
	entries := []NewEntry{}
	tape := []TapeRecord{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-inline", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		OnDelta: func(delta string) { deltas = append(deltas, delta) },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1, Type: entry.Type}, nil
		},
		Tape: func(_ context.Context, record TapeRecord) error {
			tape = append(tape, record)
			return nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(deltas) != 0 {
		t.Fatalf("candidate streamed before validation: %#v", deltas)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.Payload), "金鼻白毛老鼠精") {
			t.Fatalf("candidate persisted in entry=%#v", entry)
		}
	}
	for _, record := range tape {
		if strings.Contains(string(record.Payload), "金鼻白毛老鼠精") {
			t.Fatalf("candidate persisted in tape=%#v", record)
		}
	}
	if lint := LintTapeFold(FoldTape(tape)); !lint.OK {
		t.Fatalf("knowledge tool tape is not ordered: %#v", lint.Problems)
	}
}

func TestPiAdapterWithholdsCandidateAfterKnowledgeEvidenceWithoutSubmit(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "read-before-answer", Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/entities/example.md"}`)}}},
		{Text: "答案：金鼻白毛老鼠精。"},
		{Text: "答案仍然是金鼻白毛老鼠精。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeReadTools{}
	deltas := []string{}
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-no-submit", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		OnDelta: func(delta string) { deltas = append(deltas, delta) },
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Reply, "金鼻白毛老鼠精") || len(deltas) != 0 {
		t.Fatalf("candidate escaped before submit: reply=%q deltas=%#v", result.Reply, deltas)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.Payload), "金鼻白毛老鼠精") {
			t.Fatalf("candidate persisted in entry=%#v", entry)
		}
	}
	if len(transport.requests) != 3 || !strings.Contains(transport.requests[2].Messages[len(transport.requests[2].Messages)-1].Content, "attempted to finish before validating") {
		t.Fatalf("submit redirect missing: requests=%+v", transport.requests)
	}
}

func TestPiAdapterRedirectsPrematureKnowledgeAnswerToSubmit(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "read-before-answer", Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/entities/example.md"}`)}}},
		{Text: "答案：未经验证的候选。"},
		{ToolCalls: []ToolCall{{ID: "submit-after-answer", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{Text: "答案：候选。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeNavigationTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-premature-answer", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.Reply != "答案：候选。" || len(transport.requests) != 3 {
		t.Fatalf("result=%+v requests=%d err=%v", result, len(transport.requests), err)
	}
}

type knowledgeRepairTransport struct {
	requests int
}

func (t *knowledgeRepairTransport) Complete(_ context.Context, _ PiCompletionRequest, _ func(string), _ func()) (PiCompletion, error) {
	t.requests++
	if t.requests == 1 {
		return PiCompletion{ToolCalls: []ToolCall{{ID: "submit-call", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit"}`)}}}, nil
	}
	arguments := json.RawMessage(fmt.Sprintf(`{"action":"read","path":"wiki/evidence-%d.md"}`, t.requests))
	return PiCompletion{ToolCalls: []ToolCall{{ID: fmt.Sprintf("read-%d", t.requests), Name: "knowledge", Arguments: arguments}}}, nil
}

func (*knowledgeRepairTransport) Close(context.Context) error { return nil }

type knowledgeRepairTools struct {
	calls int
}

func (*knowledgeRepairTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeRepairTools) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	t.calls++
	if piKnowledgeAction(call) == "submit" {
		return toolTextWithDetails(`{"status":"incomplete","unresolved_requirement_ids":["9"]}`, json.RawMessage(`{"kind":"knowledge","action":"submit","sources":[]}`)), nil
	}
	return ToolResult{Content: []ToolContent{{Type: "text", Text: `{"ok":false,"code":"knowledge_action_failed"}`}}, IsError: true}, nil
}

func TestPiAdapterBoundsKnowledgeSearchAfterWorkspaceBecomesBlocked(t *testing.T) {
	transport := &knowledgeRepairTransport{}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeRepairTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-repair", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	wantCalls := knowledgeBlockedToolLimit + 1
	if tools.calls != wantCalls || transport.requests != wantCalls || result.ToolCalls != wantCalls {
		t.Fatalf("calls=%d requests=%d result=%+v", tools.calls, transport.requests, result)
	}
	if !strings.Contains(result.Reason, "bounded evidence collection") || strings.Contains(result.Reply, "答案：") {
		t.Fatalf("reason=%q reply=%q", result.Reason, result.Reply)
	}
}

func TestPiAdapterStopsImmediatelyWhenKnowledgeWorkspaceIsProcessing(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "status-call", Name: "knowledge", Arguments: json.RawMessage(`{"action":"status"}`)}}},
		{Text: "答案：金鼻白毛老鼠精。"},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeProcessingTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-processing", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.calls != 1 || len(transport.requests) != 1 || result.ToolCalls != 1 {
		t.Fatalf("calls=%d requests=%d result=%+v", tools.calls, len(transport.requests), result)
	}
	if !strings.Contains(result.Reason, "workspace is not ready") || strings.Contains(result.Reply, "金鼻白毛老鼠精") {
		t.Fatalf("reason=%q reply=%q", result.Reason, result.Reply)
	}
}

type knowledgeProcessingTools struct {
	calls int
}

func (*knowledgeProcessingTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (t *knowledgeProcessingTools) Execute(context.Context, ToolCall) (ToolResult, error) {
	t.calls++
	return toolText(`{"status":"processing"}`), nil
}

type knowledgeHaltBatchTools struct {
	knowledgeCalls int
	writebackCalls int
}

func (*knowledgeHaltBatchTools) Definitions(context.Context, ToolOptions) ([]ToolDefinition, error) {
	return []ToolDefinition{
		{Name: "knowledge", Description: "knowledge", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "writeback", Description: "writeback", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}, nil
}

func (t *knowledgeHaltBatchTools) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	if call.Name == "knowledge" {
		t.knowledgeCalls++
		return toolText(`{"status":"processing"}`), nil
	}
	t.writebackCalls++
	return toolText(`{"ok":true}`), nil
}

func TestPiAdapterSkipsLaterToolsAfterKnowledgeHaltInSameCompletion(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{
		ToolCalls: []ToolCall{
			{ID: "status-inline", Name: "knowledge", Arguments: json.RawMessage(`{"action":"status"}`)},
			{ID: "writeback-inline", Name: "writeback", Arguments: json.RawMessage(`{"action":"writeback"}`)},
		},
	}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeHaltBatchTools{}
	tape := []TapeRecord{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-halt-batch", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
		Tape: func(_ context.Context, record TapeRecord) error {
			tape = append(tape, record)
			return nil
		},
	})
	if err != nil || result.Status != "incomplete" || result.CompletionStatus != "incomplete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tools.knowledgeCalls != 1 || tools.writebackCalls != 0 {
		t.Fatalf("knowledgeCalls=%d writebackCalls=%d", tools.knowledgeCalls, tools.writebackCalls)
	}
	rows := FoldTape(tape)
	if lint := LintTapeFold(rows); !lint.OK {
		t.Fatalf("halted batch left invalid tape: %#v rows=%s", lint.Problems, rows)
	}
}

func TestPiAdapterAllowsCandidateAfterCompleteKnowledgeSubmit(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: []ToolCall{{ID: "submit-1", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
		{ToolCalls: []ToolCall{{ID: "submit-duplicate", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","question":"q"}`)}}},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeCompleteTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-complete", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || result.CompletionStatus != "ok" || result.Reply != "答案：唐太宗。" || tools.calls != 1 || len(transport.requests) != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, tools.calls, err)
	}
}

func TestPiAdapterSkipsCallsAfterCompleteKnowledgeSubmitInSameBatch(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{ToolCalls: []ToolCall{
		{ID: "submit-1", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit","answer":"答案：唐太宗。","question":"q"}`)},
		{ID: "search-after-submit", Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"唐太宗"}`)},
	}}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeCompleteTools{}
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-knowledge-complete-batch", Input: "核验", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1, Type: entry.Type, Payload: entry.Payload}, nil
		},
	})
	if err != nil || result.CompletionStatus != "ok" || result.Reply != "答案：唐太宗。" || tools.calls != 1 || len(transport.requests) != 1 {
		t.Fatalf("result=%+v calls=%d requests=%d err=%v", result, tools.calls, len(transport.requests), err)
	}
	if len(entries) != 6 || entries[3].Type != "tool_call" || entries[4].Type != "tool_result" {
		t.Fatalf("entries=%#v", entries)
	}
	var skipped struct {
		IsError bool `json:"isError"`
		Details struct {
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	}
	if json.Unmarshal(entries[4].Payload, &skipped) != nil || !skipped.IsError || skipped.Details.ErrorCode != "post_validation_call_skipped" {
		t.Fatalf("skipped result=%s", entries[4].Payload)
	}
}

func TestPiKnowledgeValidationCompleteSubmitIsTerminal(t *testing.T) {
	state := piKnowledgeValidation{}
	state.observe("submit", toolText(`{"status":"complete","answer":"答案：唐太宗。"}`))
	state.noteCall("search")
	if !state.submitComplete || state.needsSubmit() || state.answer != "答案：唐太宗。" {
		t.Fatalf("state=%+v", state)
	}
}

func TestPiKnowledgeValidationBlocksWorkspaceFailuresUntilCompleteSubmit(t *testing.T) {
	cases := []struct {
		name   string
		result ToolResult
	}{
		{name: "empty", result: toolText(`{"status":"empty"}`)},
		{name: "pending", result: toolText(`{"status":"pending"}`)},
		{name: "failed", result: ToolResult{Content: []ToolContent{{Type: "text", Text: `{"ok":false,"code":"knowledge_failed"}`}}, IsError: true}},
		{name: "unavailable", result: ToolResult{Content: []ToolContent{{Type: "text", Text: `{"ok":false,"code":"knowledge_unavailable"}`}}, IsError: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := piKnowledgeValidation{}
			state.observe("status", testCase.result)
			if !state.blocked {
				t.Fatalf("state=%+v", state)
			}
			state.observe("status", toolText(`{"status":"ready"}`))
			if !state.blocked {
				t.Fatal("ready status cleared a blocked validation")
			}
			state.observe("submit", toolText(`{"status":"complete"}`))
			if state.blocked {
				t.Fatalf("complete submit did not clear state=%+v", state)
			}
		})
	}
}

func TestPiKnowledgeValidationDoesNotBlockAfterRecoverableActionError(t *testing.T) {
	state := piKnowledgeValidation{}
	state.observe("read", ToolResult{Content: []ToolContent{{Type: "text", Text: `{"ok":false,"code":"knowledge_action_failed","action":"read"}`}}, IsError: true})
	if state.blocked || state.halt || !state.needsSubmit() {
		t.Fatalf("recoverable action error poisoned validation state: %+v", state)
	}
	state.observe("submit", toolText(`{"status":"complete"}`))
	if state.blocked || state.needsSubmit() {
		t.Fatalf("complete submit did not recover state: %+v", state)
	}
}

func TestPiKnowledgeCandidateSearchProgressRequiresReturnedResults(t *testing.T) {
	call := ToolCall{
		Name:      "knowledge",
		Arguments: json.RawMessage(`{"action":"search","query":"到过 花果山","candidate":"唐僧","requirement_id":"9"}`),
	}
	if piKnowledgeRepairProgress(call, toolTextWithDetails(`[]`, json.RawMessage(`{"kind":"knowledge","action":"search","sources":[]}`))) {
		t.Fatal("zero-result candidate search must not reset validation convergence")
	}
	if !piKnowledgeRepairProgress(call, toolTextWithDetails(`[{"path":"raw/sources/chapter-001.txt"}]`, json.RawMessage(`{"kind":"knowledge","action":"search","sources":[{"path":"raw/sources/chapter-001.txt","evidence":false}]}`))) {
		t.Fatal("candidate search with a returned source should count as repair progress")
	}
	if piKnowledgeRepairProgress(call, ToolResult{Content: []ToolContent{{Type: "text", Text: "[]"}}, Details: json.RawMessage(`{"kind":"knowledge","action":"search","sources":[{"path":"raw/sources/chapter-001.txt"}]}`), IsError: true}) {
		t.Fatal("failed candidate search must not count as repair progress")
	}
}

func knowledgeStallToolsResult() ToolResult {
	return toolTextWithDetails(`{"status":"incomplete"}`, json.RawMessage(`{"kind":"knowledge","action":"submit","sources":[]}`))
}

func TestPiAdapterFinishSilentlyOnlyTerminatesPollTurns(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{{ToolCalls: []ToolCall{{ID: "call-1", Name: "finish_silently", Arguments: json.RawMessage(`{"reason":"no changes"}`)}}, Request: json.RawMessage(`{}`)}}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", Input: "poll", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi", PollFire: true,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || !result.Silent || result.Reply != "" || len(entries) != 4 || entries[3].Type != "assistant" {
		t.Fatalf("result=%#v entries=%#v err=%v", result, entries, err)
	}
	definitions := transport.requests[0].Tools
	found := false
	for _, definition := range definitions {
		found = found || definition.Name == "finish_silently"
	}
	if !found {
		t.Fatalf("definitions=%#v", definitions)
	}
}

type blockingPiTransport struct{}

func (*blockingPiTransport) Complete(ctx context.Context, _ PiCompletionRequest, _ func(string), _ func()) (PiCompletion, error) {
	<-ctx.Done()
	return PiCompletion{}, ctx.Err()
}

func (*blockingPiTransport) Close(context.Context) error { return nil }

func TestPiAdapterConsumesDurableAbortSignal(t *testing.T) {
	adapter, _ := NewPiAdapter(piTestModels(), &blockingPiTransport{})
	var once sync.Once
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", RunID: "run-1", Input: "work", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
		TakePendingSignals: func(context.Context, string) ([]TurnSignal, error) {
			var signals []TurnSignal
			once.Do(func() { signals = []TurnSignal{{Kind: "abort"}} })
			return signals, nil
		},
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || !result.Stopped || result.Reply != "(stopped)" || len(entries) != 2 || entries[1].Type != "assistant" {
		t.Fatalf("result=%#v entries=%#v err=%v", result, entries, err)
	}
}

type delayedPiTransport struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (t *delayedPiTransport) Complete(_ context.Context, request PiCompletionRequest, _ func(string), _ func()) (PiCompletion, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if call == 1 {
		close(t.started)
		time.Sleep(20 * time.Millisecond)
		return PiCompletion{Text: "first", Request: json.RawMessage(`{}`)}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	return PiCompletion{Text: "after " + last.Content, Request: json.RawMessage(`{}`)}, nil
}

func (*delayedPiTransport) Close(context.Context) error { return nil }

func TestPiAdapterInjectsDurableSteerIntoNextModelStep(t *testing.T) {
	transport := &delayedPiTransport{started: make(chan struct{})}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	var once sync.Once
	entries := []NewEntry{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "session-1", RunID: "run-1", Input: "start", SystemPrompt: "system", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", Model: "model-pi",
		TakePendingSignals: func(context.Context, string) ([]TurnSignal, error) {
			<-transport.started
			var signals []TurnSignal
			once.Do(func() {
				signals = []TurnSignal{{Kind: "steer", Text: "new direction", Payload: json.RawMessage(`{"ts":"123.45"}`)}}
			})
			return signals, nil
		},
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			entries = append(entries, entry)
			return SessionEntry{Sequence: len(entries) - 1}, nil
		},
	})
	if err != nil || result.Reply != "after new direction" || result.ModelCalls != 2 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	types := []string{}
	for _, entry := range entries {
		types = append(types, entry.Type)
	}
	if !reflect.DeepEqual(types, []string{"user", "user", "text", "assistant"}) || !strings.Contains(string(entries[1].Payload), `"steered":true`) || !strings.Contains(string(entries[1].Payload), `"ts":"123.45"`) {
		t.Fatalf("entries=%#v", entries)
	}
}

func piTestModels() config.ModelsConfig {
	return config.ModelsConfig{
		DefaultHarness: "pi", Request: config.ModelRequestConfig{MaxOutputTokens: 4096},
		Providers: []config.ModelProviderConfig{{ID: "gateway", Protocol: "openai", BaseURL: "https://example.invalid/v1", APIKey: "test", Models: []config.ModelDefinition{{ID: "model-pi"}}}},
		Harnesses: []config.ModelHarnessConfig{{ID: "pi", Provider: "gateway", ModelIDs: []string{"model-pi"}, DefaultModel: "model-pi"}},
	}
}

func piTestModelsWithFallback() config.ModelsConfig {
	models := piTestModels()
	models.Providers[0].Models = append(models.Providers[0].Models, config.ModelDefinition{ID: "fallback-pi"})
	models.Harnesses[0].ModelIDs = append(models.Harnesses[0].ModelIDs, "fallback-pi")
	models.Harnesses[0].Runtime.FallbackModels = []string{"fallback-pi"}
	return models
}

func TestPiAdapterAllowsEvidenceAfterSubmitReminder(t *testing.T) {
	transport := &fakePiTransport{completions: []PiCompletion{
		{ToolCalls: knowledgeSearchCalls(knowledgeNoProgressLimit)},
		{ToolCalls: []ToolCall{{ID: "repair-read", Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/evidence.md"}`)}}},
		{ToolCalls: []ToolCall{{ID: "submit", Name: "knowledge", Arguments: json.RawMessage(`{"action":"submit"}`)}}},
	}}
	adapter, _ := NewPiAdapter(piTestModels(), transport)
	tools := &knowledgeNavigationTools{}
	result, err := adapter.RunTurn(context.Background(), TurnInput{
		SessionID: "repair-after-reminder", Input: "verify", SystemPrompt: "system", ScopeLabel: "group:web-project-p1", Model: "model-pi", Tools: tools,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			return SessionEntry{Sequence: 1, Type: entry.Type}, nil
		},
	})
	if err != nil || result.Status == "incomplete" || tools.calls != knowledgeNoProgressLimit+2 || result.EvidenceCount == 0 {
		t.Fatalf("repair was blocked: result=%+v calls=%d err=%v", result, tools.calls, err)
	}
}
