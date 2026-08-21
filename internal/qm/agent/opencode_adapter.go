package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type OpenCodePrompt struct {
	ModelProvider, ModelID, System string
	Tools                          map[string]bool
	Parts                          []map[string]any
}

type OpenCodeMessage struct {
	Info  map[string]any   `json:"info"`
	Parts []map[string]any `json:"parts"`
}

type OpenCodeBridgeState struct {
	SystemPrompt string
	History      []OpenCodeMessage
	Definitions  []ToolDefinition
	Execute      func(context.Context, ToolCall) ToolResult
	Capture      func(context.Context, json.RawMessage)
	OnEvent      func(context.Context, map[string]any)
}

type OpenCodeRuntime interface {
	CreateSession(context.Context, string) (string, error)
	Register(string, *OpenCodeBridgeState)
	Unregister(string)
	Prompt(context.Context, string, OpenCodePrompt) (OpenCodeMessage, error)
	PromptAsync(context.Context, string, OpenCodePrompt) error
	WaitIdle(context.Context, string) error
	Messages(context.Context, string) ([]OpenCodeMessage, error)
	Abort(context.Context, string) error
	DeleteSession(context.Context, string) error
	Close(context.Context) error
}

type OpenCodeRuntimeFactory func(context.Context, config.ModelsConfig, config.ModelHarnessConfig, []ToolDefinition) (OpenCodeRuntime, error)

type OpenCodeAdapter struct {
	models  config.ModelsConfig
	harness config.ModelHarnessConfig
	factory OpenCodeRuntimeFactory

	mu                  sync.Mutex
	runtime             OpenCodeRuntime
	definitionSignature string
}

func NewOpenCodeAdapter(models config.ModelsConfig, factory OpenCodeRuntimeFactory) (*OpenCodeAdapter, error) {
	harness, ok := models.Harness("opencode")
	if !ok {
		return nil, errors.New("opencode harness is not configured")
	}
	if factory == nil {
		factory = NewOpenCodeProcessRuntime
	}
	return &OpenCodeAdapter{models: models, harness: harness, factory: factory}, nil
}

func (a *OpenCodeAdapter) Profile() Profile { return Profiles()["opencode"] }

func (a *OpenCodeAdapter) ResetSession(context.Context, string) error { return nil }

func (a *OpenCodeAdapter) Close(ctx context.Context) error {
	a.mu.Lock()
	runtime := a.runtime
	a.runtime = nil
	a.mu.Unlock()
	if runtime != nil {
		return runtime.Close(ctx)
	}
	return nil
}

func (a *OpenCodeAdapter) ensureRuntime(ctx context.Context, definitions []ToolDefinition) (OpenCodeRuntime, error) {
	signature := openCodeDefinitionSignature(definitions)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runtime != nil {
		if a.definitionSignature != signature {
			return nil, &NonRetryableError{Err: errors.New("opencode tool definitions changed after its plugin runtime started")}
		}
		return a.runtime, nil
	}
	runtime, err := a.factory(ctx, a.models, a.harness, definitions)
	if err != nil {
		return nil, err
	}
	a.runtime = runtime
	a.definitionSignature = signature
	return runtime, nil
}

func (a *OpenCodeAdapter) RunTurn(ctx context.Context, input TurnInput) (TurnResult, error) {
	if input.Emit == nil {
		return TurnResult{}, &NonRetryableError{Err: errors.New("opencode session entry persistence is not configured")}
	}
	model := utilityModel(input.Model, a.harness.DefaultModel)
	provider, ok := a.models.ProviderForModel(model)
	if !ok || !contains(a.harness.ModelIDs, model) {
		return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("opencode model %s is not approved", model)}
	}
	definitions := []ToolDefinition{}
	runtimeDefinitions := []ToolDefinition{}
	var err error
	if input.Tools != nil {
		definitions, err = input.Tools.Definitions(ctx, ToolOptions{ReadOnly: input.ReadOnly, SurfaceTools: input.SurfaceTools, SurfaceName: input.SurfaceName})
		if err != nil {
			return TurnResult{}, err
		}
		runtimeDefinitions, err = openCodeAllDefinitions(ctx, input.Tools, input.SurfaceName)
		if err != nil {
			return TurnResult{}, err
		}
	}
	definitions = piLifecycleTools(definitions, input.SurfaceTools)
	runtimeDefinitions = piLifecycleTools(runtimeDefinitions, false)
	runtimeDefinitions = piLifecycleTools(runtimeDefinitions, true)
	aliased := openCodeDefinitions(definitions)
	aliasedRuntime := openCodeDefinitions(runtimeDefinitions)
	runtime, err := a.ensureRuntime(ctx, aliasedRuntime)
	if err != nil {
		return TurnResult{}, err
	}
	wallMS := input.TurnWallClockMS
	if wallMS <= 0 && a.harness.Runtime.TurnWallClockSeconds > 0 {
		wallMS = a.harness.Runtime.TurnWallClockSeconds * 1000
	}
	turnCtx, cancel := context.WithCancel(ctx)
	if wallMS > 0 {
		turnCtx, cancel = context.WithTimeout(ctx, time.Duration(wallMS)*time.Millisecond)
	}
	defer cancel()
	sessionID, err := runtime.CreateSession(turnCtx, "qm:"+input.SessionID)
	if err != nil {
		return TurnResult{}, err
	}
	defer func() {
		runtime.Unregister(sessionID)
		_ = runtime.DeleteSession(context.Background(), sessionID)
	}()
	userEntry, err := turnUserEntry(turnCtx, input)
	if err != nil {
		return TurnResult{}, err
	}
	state := &openCodeTurnState{input: input, userEntry: userEntry, model: model}
	bridge := &OpenCodeBridgeState{
		SystemPrompt: input.SystemPrompt + "\n\nOpenCode tool aliases: workspace_execute is foreground execute; workspace_read reads workspace files; workspace_write writes workspace files.",
		History:      openCodeReplayHistory(input.History, sessionID, provider.ID, model), Definitions: aliasedRuntime,
		Execute: func(callCtx context.Context, call ToolCall) ToolResult {
			return state.executeTool(callCtx, call)
		},
		Capture: func(captureCtx context.Context, request json.RawMessage) {
			state.capture(captureCtx, request)
		},
		OnEvent: func(eventCtx context.Context, event map[string]any) {
			state.processEvent(eventCtx, event)
		},
	}
	runtime.Register(sessionID, bridge)
	stopSignals := startOpenCodeSignals(turnCtx, input, runtime, sessionID, provider.ID, model, &state.stopped)
	defer stopSignals()
	promptText := strings.TrimSpace(strings.Join(nonEmpty(input.Input, input.Environment), "\n\n"))
	parts := []map[string]any{{"type": "text", "text": promptText}}
	for index, image := range input.Images {
		parts = append(parts, map[string]any{"type": "file", "mime": image.MIMEType, "filename": fmt.Sprintf("image-%d", index+1), "url": "data:" + image.MIMEType + ";base64," + image.DataBase64})
	}
	enabled := map[string]bool{"task": !input.ReadOnly}
	for _, definition := range aliased {
		enabled[definition.Name] = true
	}
	response, err := runtime.Prompt(turnCtx, sessionID, OpenCodePrompt{ModelProvider: provider.ID, ModelID: model, System: input.SystemPrompt, Tools: enabled, Parts: parts})
	if err != nil {
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			_ = runtime.Abort(context.Background(), sessionID)
			return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("OpenCode turn exceeded %s wall clock", time.Duration(wallMS)*time.Millisecond)}
		}
		if state.stopped.Load() {
			return a.finishStoppedOpenCode(turnCtx, input, state, userEntry)
		}
		return TurnResult{}, err
	}
	if failure, permanent := openCodeAssistantFailure(response.Info); failure != "" {
		if permanent {
			return TurnResult{}, &NonRetryableError{Err: errors.New(failure)}
		}
		return TurnResult{}, errors.New(failure)
	}
	messages, listErr := runtime.Messages(turnCtx, sessionID)
	if listErr != nil {
		return TurnResult{}, listErr
	}
	for _, message := range messages {
		for _, part := range message.Parts {
			state.processPart(turnCtx, part, "")
		}
	}
	result := TurnResult{ModelCalls: int(state.modelCalls.Load()), PendingApprovals: state.approvals(), PausedOnApproval: state.paused.Load(), Silent: state.silent.Load(), Stopped: state.stopped.Load()}
	for _, message := range messages {
		role, _ := message.Info["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		payload, _ := json.Marshal(stripOpenCodeDataURLs(message))
		record := TapeRecord{Kind: "message", Harness: "opencode", ScopeLabel: input.ScopeLabel, Payload: payload}
		if role == "user" && !state.tapedUser {
			state.tapedUser = true
			record.EntrySequence = &userEntry.Sequence
			record.BareText = input.Input
		}
		if !writePiTape(turnCtx, input, record) {
			result.TapeWriteFailed = true
		}
	}
	for _, thinking := range openCodeReasoning(response.Parts) {
		payload, _ := json.Marshal(map[string]string{"thinking": thinking})
		if _, err := input.Emit(turnCtx, NewEntry{Type: "thinking", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
			return TurnResult{}, err
		}
	}
	reply := openCodeText(response.Parts)
	result.Reply = reply
	assistantPayload, _ := json.Marshal(map[string]any{"text": reply, "stopped": result.Stopped})
	finalEntry, err := input.Emit(turnCtx, NewEntry{Type: "assistant", Payload: assistantPayload, ScopeLabel: input.ScopeLabel})
	if err != nil {
		return TurnResult{}, err
	}
	result.FinalEntrySequence = &finalEntry.Sequence
	return result, nil
}

type openCodeTurnState struct {
	input                   TurnInput
	userEntry               SessionEntry
	model                   string
	modelCalls              atomic.Int32
	stopped, paused, silent atomic.Bool

	mu        sync.Mutex
	pending   []PendingApproval
	seenText  map[string]string
	tapedUser bool
}

func (s *openCodeTurnState) executeTool(ctx context.Context, call ToolCall) ToolResult {
	call.Name = openCodeCoreToolName(call.Name)
	payload, _ := json.Marshal(map[string]any{"tool": call.Name, "callId": call.ID, "arguments": rawJSONObject(call.Arguments)})
	_, _ = s.input.Emit(ctx, NewEntry{Type: "tool_call", Payload: payload, ScopeLabel: s.input.ScopeLabel})
	var result ToolResult
	if s.input.ToolApprovalGate != nil && !toolApprovalExempt(call.Name) && !s.input.ToolApprovalGate(call.Name) {
		const reason = "strict posture: this tool call requires human approval"
		s.mu.Lock()
		s.pending = append(s.pending, PendingApproval{Command: call.Name, Reason: reason, Kind: "approval", ApprovalKey: "tool:" + call.Name})
		s.mu.Unlock()
		s.paused.Store(true)
		result = ToolResult{Content: []ToolContent{{Type: "text", Text: "[blocked: needs human approval] " + reason}}, IsError: true, Terminate: true}
	} else if call.Name == "finish_silently" || call.Name == "stay_silent" {
		result = finishSilentlyResult(s.input.PollFire || call.Name == "stay_silent")
	} else if s.input.Tools == nil {
		result = ToolResult{Content: []ToolContent{{Type: "text", Text: "[error] tool context is unavailable"}}, IsError: true, Terminate: true}
	} else {
		var err error
		result, err = s.input.Tools.Execute(ctx, call)
		if err != nil {
			result = ToolResult{Content: []ToolContent{{Type: "text", Text: "[tool failed] " + err.Error()}}, IsError: true}
		}
	}
	resultPayload := toolResultEntryPayload(call, result)
	_, _ = s.input.Emit(ctx, NewEntry{Type: "tool_result", Payload: resultPayload, ScopeLabel: s.input.ScopeLabel})
	if result.Silent {
		s.silent.Store(true)
	}
	return result
}

func toolResultEntryPayload(call ToolCall, result ToolResult) json.RawMessage {
	value := map[string]any{"tool": call.Name, "callId": call.ID, "result": toolResultText(result), "isError": result.IsError}
	if len(result.Details) > 0 && string(result.Details) != "null" && json.Valid(result.Details) {
		value["details"] = rawJSONObject(result.Details)
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func (s *openCodeTurnState) capture(ctx context.Context, request json.RawMessage) {
	step := int(s.modelCalls.Add(1)) - 1
	if s.input.RecordModelCall != nil {
		s.input.RecordModelCall(ModelCallRecord{Model: s.model, InputTokens: (len(request) + 3) / 4, EntryCount: historyEntryCount(s.input.History)})
	}
	if s.input.RecordLLMRequest != nil {
		_ = s.input.RecordLLMRequest(ctx, LLMRequestRecord{TurnSequence: &s.userEntry.Sequence, Step: step, Model: s.model, Request: request})
	}
}

func (s *openCodeTurnState) processEvent(ctx context.Context, event map[string]any) {
	payload, _ := event["payload"].(map[string]any)
	if payload["type"] != "message.part.updated" {
		return
	}
	properties, _ := payload["properties"].(map[string]any)
	part, _ := properties["part"].(map[string]any)
	delta, _ := properties["delta"].(string)
	s.processPart(ctx, part, delta)
}

func (s *openCodeTurnState) processPart(_ context.Context, part map[string]any, delta string) {
	if part["type"] != "text" || delta == "" {
		return
	}
	s.mu.Lock()
	if s.seenText == nil {
		s.seenText = map[string]string{}
	}
	id := fmt.Sprint(part["id"])
	_, seen := s.seenText[id]
	s.seenText[id] = fmt.Sprint(part["text"])
	s.mu.Unlock()
	if !seen && s.input.OnTextBlockStart != nil {
		s.input.OnTextBlockStart()
	}
	if s.input.OnDelta != nil {
		s.input.OnDelta(delta)
	}
}

func (s *openCodeTurnState) approvals() []PendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PendingApproval(nil), s.pending...)
}

func (a *OpenCodeAdapter) finishStoppedOpenCode(ctx context.Context, input TurnInput, state *openCodeTurnState, userEntry SessionEntry) (TurnResult, error) {
	payload, _ := json.Marshal(map[string]any{"text": "(stopped)", "stopped": true})
	entry, err := input.Emit(ctx, NewEntry{Type: "assistant", Payload: payload, ScopeLabel: input.ScopeLabel})
	if err != nil {
		return TurnResult{}, err
	}
	return TurnResult{Reply: "(stopped)", Stopped: true, ModelCalls: int(state.modelCalls.Load()), FinalEntrySequence: &entry.Sequence}, nil
}

func startOpenCodeSignals(ctx context.Context, input TurnInput, runtime OpenCodeRuntime, sessionID, provider, model string, stopped *atomic.Bool) func() {
	if input.TakePendingSignals == nil || input.RunID == "" {
		return func() {}
	}
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			signals, err := input.TakePendingSignals(pollCtx, input.RunID)
			if err == nil {
				for _, signal := range signals {
					switch signal.Kind {
					case "abort":
						stopped.Store(true)
						_ = runtime.Abort(context.Background(), sessionID)
					case "steer":
						payload, _ := json.Marshal(map[string]any{"text": signal.Text, "ts": signal.CreatedAt, "steered": true})
						_, _ = input.Emit(pollCtx, NewEntry{Type: "user", Payload: payload, ScopeLabel: input.ScopeLabel})
						_ = runtime.PromptAsync(pollCtx, sessionID, OpenCodePrompt{ModelProvider: provider, ModelID: model, Parts: []map[string]any{{"type": "text", "text": signal.Text}}})
					}
				}
			}
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func openCodeDefinitions(definitions []ToolDefinition) []ToolDefinition {
	result := make([]ToolDefinition, len(definitions))
	for index, definition := range definitions {
		result[index] = definition
		result[index].Name = openCodeToolName(definition.Name)
	}
	return result
}

func openCodeAllDefinitions(ctx context.Context, tools ToolContext, surfaceName string) ([]ToolDefinition, error) {
	byName := map[string]ToolDefinition{}
	for _, options := range []ToolOptions{{SurfaceTools: true, SurfaceName: surfaceName}, {SurfaceTools: false, SurfaceName: surfaceName}} {
		definitions, err := tools.Definitions(ctx, options)
		if err != nil {
			return nil, err
		}
		for _, definition := range definitions {
			byName[definition.Name] = definition
		}
	}
	result := make([]ToolDefinition, 0, len(byName))
	for _, definition := range byName {
		result = append(result, definition)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func openCodeToolName(name string) string {
	switch name {
	case "execute":
		return "workspace_execute"
	case "read":
		return "workspace_read"
	case "write":
		return "workspace_write"
	default:
		return name
	}
}

func openCodeCoreToolName(name string) string {
	switch name {
	case "workspace_execute":
		return "execute"
	case "workspace_read":
		return "read"
	case "workspace_write":
		return "write"
	default:
		return name
	}
}

func openCodeDefinitionSignature(definitions []ToolDefinition) string {
	copy := append([]ToolDefinition(nil), definitions...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].Name < copy[j].Name })
	encoded, _ := json.Marshal(copy)
	return string(encoded)
}

func openCodeText(parts []map[string]any) string {
	var text strings.Builder
	for _, part := range parts {
		if part["type"] == "text" {
			text.WriteString(fmt.Sprint(part["text"]))
		}
	}
	return strings.TrimSpace(text.String())
}

func openCodeReasoning(parts []map[string]any) []string {
	result := []string{}
	for _, part := range parts {
		if part["type"] == "reasoning" && strings.TrimSpace(fmt.Sprint(part["text"])) != "" {
			result = append(result, fmt.Sprint(part["text"]))
		}
	}
	return result
}

func openCodeAssistantFailure(info map[string]any) (string, bool) {
	errorValue, _ := info["error"].(map[string]any)
	if len(errorValue) == 0 {
		return "", false
	}
	name := fmt.Sprint(errorValue["name"])
	if name == "MessageAbortedError" || name == "MessageOutputLengthError" {
		return "", false
	}
	data, _ := errorValue["data"].(map[string]any)
	detail := strings.TrimSpace(fmt.Sprint(data["message"]))
	message := "OpenCode provider error (" + name + ")"
	if detail != "" && detail != "<nil>" {
		message += ": " + detail
	}
	permanent := name == "ProviderAuthError" || name == "APIError" && data["isRetryable"] == false
	return message, permanent
}

func stripOpenCodeDataURLs(message OpenCodeMessage) OpenCodeMessage {
	copy := message
	copy.Info = cloneAnyMap(message.Info)
	copy.Parts = make([]map[string]any, len(message.Parts))
	for index, part := range message.Parts {
		copy.Parts[index] = cloneAnyMap(part)
		if url, ok := copy.Parts[index]["url"].(string); ok && strings.HasPrefix(url, "data:") {
			delete(copy.Parts[index], "url")
			copy.Parts[index]["omitted"] = true
		}
	}
	return copy
}

func cloneAnyMap(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func openCodeReplayHistory(raw json.RawMessage, sessionID, provider, model string) []OpenCodeMessage {
	messages := piMessagesFromHistory(raw)
	result := make([]OpenCodeMessage, 0, len(messages))
	for index, message := range messages {
		role := message.Role
		if role == "tool" {
			continue
		}
		parts := []map[string]any{}
		if message.Content != "" {
			parts = append(parts, map[string]any{"id": fmt.Sprintf("part_%s_%d", sessionID, index), "sessionID": sessionID, "messageID": fmt.Sprintf("msg_%s_%d", sessionID, index), "type": "text", "text": message.Content})
		}
		result = append(result, OpenCodeMessage{Info: map[string]any{"id": fmt.Sprintf("msg_%s_%d", sessionID, index), "sessionID": sessionID, "role": role, "providerID": provider, "modelID": model}, Parts: parts})
	}
	return result
}
