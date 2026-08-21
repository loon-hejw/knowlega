package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type CodexRuntimeFactory func(context.Context, config.ModelsConfig, config.ModelHarnessConfig, CodexNotificationHandler, CodexRequestHandler) (CodexRPC, string, func(), error)

type CodexAdapter struct {
	models  config.ModelsConfig
	harness config.ModelHarnessConfig
	factory CodexRuntimeFactory
	tasks   AgentTaskStore

	mu      sync.Mutex
	rpc     CodexRPC
	jail    string
	cleanup func()
	active  map[string]*codexTurnState
}

type codexTurnState struct {
	threadID string
	input    TurnInput
	model    string
	user     SessionEntry
	tools    map[string]ToolDefinition

	completed    chan CodexTurn
	itemsMu      sync.Mutex
	items        []map[string]any
	childThreads map[string]string
	childStatus  map[string]string
	childResults map[string]bool
	toolState    openCodeTurnState
	modelCalls   atomic.Int32
	firstOutput  atomic.Int64
	stopped      atomic.Bool
	tapeFailed   atomic.Bool
	inputTokens  atomic.Int64
	taskErrMu    sync.Mutex
	taskErr      error
}

type CodexTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Items []map[string]any `json:"items,omitempty"`
}

func NewCodexAdapter(models config.ModelsConfig, factory CodexRuntimeFactory) (*CodexAdapter, error) {
	harness, ok := models.Harness("codex")
	if !ok {
		return nil, errors.New("codex harness is not configured")
	}
	adapter := &CodexAdapter{models: models, harness: harness, factory: factory, active: map[string]*codexTurnState{}}
	if adapter.factory == nil {
		adapter.factory = NewCodexProcessRuntime
	}
	return adapter, nil
}

func (a *CodexAdapter) Profile() Profile                         { return Profiles()["codex"] }
func (*CodexAdapter) ResetSession(context.Context, string) error { return nil }

func (a *CodexAdapter) WithTaskStore(tasks AgentTaskStore) *CodexAdapter {
	a.tasks = tasks
	return a
}

func (a *CodexAdapter) Close(ctx context.Context) error {
	a.mu.Lock()
	rpc, cleanup := a.rpc, a.cleanup
	a.rpc, a.cleanup = nil, nil
	a.active = map[string]*codexTurnState{}
	a.mu.Unlock()
	var err error
	if rpc != nil {
		err = rpc.Close(ctx)
	}
	if cleanup != nil {
		cleanup()
	}
	return err
}

func (a *CodexAdapter) ensureRuntime(ctx context.Context) (CodexRPC, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rpc != nil {
		return a.rpc, a.jail, nil
	}
	rpc, jail, cleanup, err := a.factory(ctx, a.models, a.harness, a.notification, a.request)
	if err != nil {
		return nil, "", err
	}
	a.rpc, a.jail, a.cleanup = rpc, jail, cleanup
	return rpc, jail, nil
}

func (a *CodexAdapter) RunTurn(ctx context.Context, input TurnInput) (TurnResult, error) {
	if input.Emit == nil {
		return TurnResult{}, &NonRetryableError{Err: errors.New("codex session entry persistence is not configured")}
	}
	model := utilityModel(input.Model, a.harness.DefaultModel)
	if _, ok := a.models.ProviderForModel(model); !ok || !contains(a.harness.ModelIDs, model) {
		return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("codex model %s is not approved", model)}
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
	rpc, jail, err := a.ensureRuntime(turnCtx)
	if err != nil {
		return TurnResult{}, err
	}
	definitions := []ToolDefinition{}
	if input.Tools != nil {
		definitions, err = input.Tools.Definitions(turnCtx, ToolOptions{ReadOnly: input.ReadOnly, SurfaceTools: input.SurfaceTools, SurfaceName: input.SurfaceName})
		if err != nil {
			return TurnResult{}, err
		}
	}
	definitions = piLifecycleTools(definitions, input.SurfaceTools)
	dynamicTools := make([]map[string]any, 0, len(definitions))
	toolMap := map[string]ToolDefinition{}
	for _, definition := range definitions {
		toolMap[definition.Name] = definition
		dynamicTools = append(dynamicTools, map[string]any{"type": "function", "name": definition.Name, "description": definition.Description, "inputSchema": rawJSONObject(definition.InputSchema)})
	}
	threadRequest := map[string]any{
		"model": model, "cwd": jail, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true,
		"baseInstructions":      input.SystemPrompt,
		"developerInstructions": "Use the supplied dynamic QM tools for all workspace, execution, memory, history, and surface operations. The built-in working directory is an empty read-only control jail, not the user's workspace.",
		"dynamicTools":          dynamicTools, "experimentalRawEvents": true, "environments": []any{},
		"config": map[string]any{"web_search": "disabled", "features": map[string]bool{"shell_tool": false, "unified_exec": false, "shell_snapshot": false, "apps": false, "plugins": false, "browser_use": false, "browser_use_external": false, "computer_use": false, "image_generation": false, "in_app_browser": false, "multi_agent": !input.ReadOnly, "request_permissions_tool": false, "tool_suggest": false}},
	}
	if effort := codexReasoningEffort(input.ThinkingLevel); effort != "" {
		threadRequest["config"].(map[string]any)["model_reasoning_effort"] = effort
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	if err := rpc.Request(turnCtx, "thread/start", threadRequest, &started); err != nil {
		return TurnResult{}, classifyCodexError(err)
	}
	if started.Thread.ID == "" {
		return TurnResult{}, errors.New("Codex thread/start returned no thread id")
	}
	threadID := started.Thread.ID
	if replay := codexReplayItems(input.History); len(replay) > 0 {
		if err := rpc.Request(turnCtx, "thread/inject_items", map[string]any{"threadId": threadID, "items": replay}, nil); err != nil {
			return TurnResult{}, err
		}
	}
	user, err := turnUserEntry(turnCtx, input)
	if err != nil {
		return TurnResult{}, err
	}
	state := &codexTurnState{
		threadID: threadID, input: input, model: model, user: user, tools: toolMap,
		completed: make(chan CodexTurn, 1), childThreads: map[string]string{}, childStatus: map[string]string{}, childResults: map[string]bool{},
	}
	state.toolState = openCodeTurnState{input: input, userEntry: user, model: model}
	a.mu.Lock()
	a.active[threadID] = state
	a.mu.Unlock()
	defer func() {
		_ = a.failOpenCodexTasks(state)
		a.mu.Lock()
		for activeThread, candidate := range a.active {
			if candidate == state {
				delete(a.active, activeThread)
			}
		}
		a.mu.Unlock()
	}()
	prior := ""
	if len(codexReplayItems(input.History)) == 0 {
		prior = seedPriorTurnsText(input.PriorTurns)
	}
	inputText := strings.TrimSpace(strings.Join(nonEmpty(prior, input.Input, input.Environment), "\n\n"))
	turnInput := []map[string]any{{"type": "text", "text": inputText, "text_elements": []any{}}}
	for _, image := range input.Images {
		turnInput = append(turnInput, map[string]any{"type": "image", "url": "data:" + image.MIMEType + ";base64," + image.DataBase64})
	}
	tapeUser, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": inputText}}})
	if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "codex", ScopeLabel: input.ScopeLabel, Payload: tapeUser, BareText: input.Input, EntrySequence: &user.Sequence}) {
		state.tapeFailed.Store(true)
	}
	requestSnapshot, _ := json.Marshal(map[string]any{"threadStart": codexRedactThreadRequest(threadRequest), "replay": codexReplayItems(input.History), "input": stripCodexInputImages(turnInput)})
	startedAt := time.Now()
	var turnStarted struct {
		Turn CodexTurn `json:"turn"`
	}
	if err := rpc.Request(turnCtx, "turn/start", map[string]any{"threadId": threadID, "input": turnInput, "model": model}, &turnStarted); err != nil {
		return TurnResult{}, classifyCodexError(err)
	}
	turnID := turnStarted.Turn.ID
	stopSignals := startCodexSignals(turnCtx, input, rpc, threadID, &turnID, state)
	defer stopSignals()
	var completed CodexTurn
	select {
	case completed = <-state.completed:
	case <-turnCtx.Done():
		_ = rpc.Request(context.Background(), "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("Codex turn exceeded %s wall clock", time.Duration(wallMS)*time.Millisecond)}
		}
		return TurnResult{}, turnCtx.Err()
	}
	if state.modelCalls.Load() == 0 {
		state.modelCalls.Store(1)
		if input.RecordModelCall != nil {
			input.RecordModelCall(ModelCallRecord{Model: model, InputTokens: (len(requestSnapshot) + 3) / 4, EntryCount: historyEntryCount(input.History)})
		}
	}
	if input.RecordLLMRequest != nil {
		duration := int(time.Since(startedAt).Milliseconds())
		var ttft *int
		if first := state.firstOutput.Load(); first > 0 {
			value := int(first - startedAt.UnixMilli())
			ttft = &value
		}
		_ = input.RecordLLMRequest(turnCtx, LLMRequestRecord{TurnSequence: &user.Sequence, Step: 0, Model: model, Request: requestSnapshot, Truncated: len(input.Images) > 0, TTFTMS: ttft, DurationMS: &duration, Transport: json.RawMessage(fmt.Sprintf(`{"modelId":%q}`, model))})
	}
	if completed.Status == "failed" {
		message := "Codex turn failed"
		if completed.Error != nil && completed.Error.Message != "" {
			message = completed.Error.Message
		}
		return TurnResult{}, classifyCodexError(errors.New(message))
	}
	if len(completed.Items) == 0 {
		state.itemsMu.Lock()
		completed.Items = append([]map[string]any(nil), state.items...)
		state.itemsMu.Unlock()
	}
	result := TurnResult{Stopped: state.stopped.Load(), ModelCalls: int(state.modelCalls.Load()), TapeWriteFailed: state.tapeFailed.Load(), PendingApprovals: state.toolState.approvals(), PausedOnApproval: state.toolState.paused.Load(), Silent: state.toolState.silent.Load()}
	terminal := result.Silent || result.PausedOnApproval
	if !terminal {
		result.Reply = codexTurnText(completed)
	}
	for _, thinking := range codexTurnReasoning(completed) {
		payload, _ := json.Marshal(map[string]string{"thinking": thinking})
		if _, err := input.Emit(turnCtx, NewEntry{Type: "thinking", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
			return TurnResult{}, err
		}
	}
	if result.Reply != "" && !terminal {
		payload, _ := json.Marshal(map[string]any{"text": result.Reply, "stopped": result.Stopped})
		entry, err := input.Emit(turnCtx, NewEntry{Type: "assistant", Payload: payload, ScopeLabel: input.ScopeLabel})
		if err != nil {
			return TurnResult{}, err
		}
		result.FinalEntrySequence = &entry.Sequence
	} else {
		payload, _ := json.Marshal(map[string]string{"text": ""})
		entry, err := input.Emit(turnCtx, NewEntry{Type: "assistant", Payload: payload, ScopeLabel: input.ScopeLabel})
		if err != nil {
			return TurnResult{}, err
		}
		result.FinalEntrySequence = &entry.Sequence
	}
	if err := a.failOpenCodexTasks(state); err != nil {
		return TurnResult{}, err
	}
	if err := state.getTaskError(); err != nil {
		return TurnResult{}, err
	}
	return result, nil
}

func (a *CodexAdapter) notification(ctx context.Context, method string, raw json.RawMessage) {
	var params map[string]any
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	threadID, _ := params["threadId"].(string)
	a.mu.Lock()
	state := a.active[threadID]
	a.mu.Unlock()
	if state == nil {
		return
	}
	switch method {
	case "thread/tokenUsage/updated":
		inputTokens := codexInputTokenUpdate(params, state.inputTokens.Load())
		if inputTokens > 0 {
			state.inputTokens.Add(int64(inputTokens))
			state.modelCalls.Add(1)
			if state.input.RecordModelCall != nil {
				state.input.RecordModelCall(ModelCallRecord{Model: state.model, InputTokens: inputTokens, EntryCount: historyEntryCount(state.input.History)})
			}
		}
	case "item/agentMessage/delta":
		if threadID == state.threadID {
			if delta, ok := params["delta"].(string); ok {
				state.firstOutput.CompareAndSwap(0, time.Now().UnixMilli())
				if state.input.OnDelta != nil {
					state.input.OnDelta(delta)
				}
			}
		}
	case "item/started":
		item, _ := params["item"].(map[string]any)
		if item != nil {
			a.processCodexCollabItem(ctx, state, item)
		}
	case "item/completed":
		item, _ := params["item"].(map[string]any)
		if item != nil {
			state.itemsMu.Lock()
			state.items = append(state.items, item)
			state.itemsMu.Unlock()
			payload, _ := json.Marshal(item)
			if !writePiTape(ctx, state.input, TapeRecord{Kind: "message", Harness: "codex", ScopeLabel: state.input.ScopeLabel, Payload: payload}) {
				state.tapeFailed.Store(true)
			}
			a.processCodexCollabItem(ctx, state, item)
		}
	case "turn/completed":
		if threadID != state.threadID {
			return
		}
		encoded, _ := json.Marshal(params["turn"])
		var turn CodexTurn
		if json.Unmarshal(encoded, &turn) == nil {
			select {
			case state.completed <- turn:
			default:
			}
		}
	}
}

func (a *CodexAdapter) processCodexCollabItem(ctx context.Context, state *codexTurnState, item map[string]any) {
	if item["type"] != "collabAgentToolCall" {
		return
	}
	if fmt.Sprint(item["tool"]) == "spawnAgent" {
		for _, receiver := range anyStringSlice(item["receiverThreadIds"]) {
			state.itemsMu.Lock()
			_, exists := state.childThreads[receiver]
			if !exists {
				state.childThreads[receiver] = fmt.Sprint(item["id"]) + ":" + receiver
				state.childStatus[state.childThreads[receiver]] = "in_progress"
			}
			taskID := state.childThreads[receiver]
			state.itemsMu.Unlock()
			if exists {
				continue
			}
			if a.tasks != nil {
				if err := a.tasks.CreateTask(ctx, taskID, state.input.SessionID, agentTaskOrigin(state.input), codexTaskTitle(item["prompt"]), "in_progress"); err != nil {
					state.setTaskError(err)
					state.itemsMu.Lock()
					delete(state.childThreads, receiver)
					delete(state.childStatus, taskID)
					state.itemsMu.Unlock()
					continue
				}
			}
			a.mu.Lock()
			a.active[receiver] = state
			a.mu.Unlock()
			payload, _ := json.Marshal(map[string]any{"tool": "spawnAgent", "callId": taskID, "prompt": item["prompt"], "receiverThreadId": receiver})
			_, _ = state.input.Emit(ctx, NewEntry{Type: "tool_call", Payload: payload, ScopeLabel: state.input.ScopeLabel})
		}
	}
	agents, _ := item["agentsStates"].(map[string]any)
	for receiver, rawAgent := range agents {
		agent, _ := rawAgent.(map[string]any)
		state.itemsMu.Lock()
		taskID := state.childThreads[receiver]
		prior := state.childStatus[taskID]
		next := codexChildStatus(fmt.Sprint(agent["status"]))
		alreadyEmitted := state.childResults[taskID]
		state.itemsMu.Unlock()
		if taskID == "" || next == "" {
			continue
		}
		if next != prior {
			if err := transitionAgentTask(ctx, a.tasks, taskID, prior, next, agentTaskOrigin(state.input)); err != nil {
				state.setTaskError(err)
				continue
			}
			state.itemsMu.Lock()
			state.childStatus[taskID] = next
			state.itemsMu.Unlock()
		}
		terminal := (next == "completed" || next == "failed") && !alreadyEmitted
		if !terminal {
			continue
		}
		state.itemsMu.Lock()
		state.childResults[taskID] = true
		state.itemsMu.Unlock()
		message, _ := agent["message"].(string)
		payload, _ := json.Marshal(map[string]any{"tool": "spawnAgent", "callId": taskID, "result": firstNonEmpty(message, next), "isError": next == "failed"})
		_, _ = state.input.Emit(ctx, NewEntry{Type: "tool_result", Payload: payload, ScopeLabel: state.input.ScopeLabel})
	}
}

func (s *codexTurnState) setTaskError(err error) {
	if err == nil {
		return
	}
	s.taskErrMu.Lock()
	if s.taskErr == nil {
		s.taskErr = err
	}
	s.taskErrMu.Unlock()
}

func (s *codexTurnState) getTaskError() error {
	s.taskErrMu.Lock()
	defer s.taskErrMu.Unlock()
	return s.taskErr
}

func (a *CodexAdapter) failOpenCodexTasks(state *codexTurnState) error {
	if state == nil {
		return nil
	}
	state.itemsMu.Lock()
	open := map[string]string{}
	for taskID, status := range state.childStatus {
		if status == "pending" || status == "in_progress" {
			open[taskID] = status
		}
	}
	state.itemsMu.Unlock()
	if len(open) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for taskID, status := range open {
		if err := transitionAgentTask(ctx, a.tasks, taskID, status, "failed", agentTaskOrigin(state.input)); err != nil {
			state.setTaskError(err)
			return err
		}
		state.itemsMu.Lock()
		state.childStatus[taskID] = "failed"
		state.itemsMu.Unlock()
	}
	return nil
}

func codexChildStatus(value string) string {
	switch value {
	case "completed", "shutdown":
		return "completed"
	case "errored", "interrupted", "notFound":
		return "failed"
	case "running":
		return "in_progress"
	default:
		return ""
	}
}

func anyStringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func (a *CodexAdapter) request(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method != "item/tool/call" {
		return nil, fmt.Errorf("unsupported Codex request %s", method)
	}
	var params struct {
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		CallID    string          `json:"callId"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	a.mu.Lock()
	state := a.active[params.ThreadID]
	a.mu.Unlock()
	if state == nil {
		return nil, errors.New("inactive Codex thread")
	}
	if params.ThreadID != state.threadID && !codexChildToolAllowed(params.Tool) {
		return nil, fmt.Errorf("Codex child requested unavailable tool %s", params.Tool)
	}
	if _, ok := state.tools[params.Tool]; !ok && params.Tool != "finish_silently" && params.Tool != "stay_silent" {
		return nil, fmt.Errorf("Codex requested unavailable tool %s", params.Tool)
	}
	result := state.toolState.executeTool(ctx, ToolCall{ID: params.CallID, Name: params.Tool, Arguments: params.Arguments})
	return map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": toolResultText(result)}}, "success": !result.IsError}, nil
}

func codexChildToolAllowed(name string) bool {
	switch name {
	case "execute", "read", "write", "publish", "memory", "history", "background":
		return true
	default:
		return false
	}
}

func NewCodexProcessRuntime(ctx context.Context, models config.ModelsConfig, harness config.ModelHarnessConfig, notification CodexNotificationHandler, request CodexRequestHandler) (CodexRPC, string, func(), error) {
	provider, ok := models.Provider(harness.Provider)
	if !ok {
		return nil, "", nil, fmt.Errorf("codex provider %s is not configured", harness.Provider)
	}
	binary, err := findCodexBinary(harness.Runtime.BinaryPath)
	if err != nil {
		return nil, "", nil, err
	}
	jail, err := os.MkdirTemp("", "qm-codex-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(jail) }
	codexHome := filepath.Join(jail, "codex-home")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	auth, _ := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": provider.APIKey})
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), auth, 0600); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	pathParts := []string{filepath.Dir(binary)}
	if node, nodeErr := exec.LookPath("node"); nodeErr == nil {
		pathParts = append(pathParts, filepath.Dir(node))
	}
	pathParts = append(pathParts, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
	environment := []string{"HOME=" + jail, "CODEX_HOME=" + codexHome, "PATH=" + strings.Join(pathParts, ":"), "OPENAI_API_KEY=" + provider.APIKey, "OPENAI_BASE_URL=" + provider.BaseURL}
	processCtx := context.Background()
	rpc, err := newCodexRPCClient(processCtx, binary, jail, environment, notification, request)
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	startup := time.Duration(harness.Runtime.StartupTimeoutSeconds) * time.Second
	if startup <= 0 {
		startup = 30 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, startup)
	defer cancel()
	if err := rpc.Initialize(initCtx); err != nil {
		_ = rpc.Close(context.Background())
		cleanup()
		return nil, "", nil, err
	}
	return rpc, jail, cleanup, nil
}

func findCodexBinary(configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		absolute, err := filepath.Abs(configured)
		if err == nil {
			if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
				return absolute, nil
			}
		}
	}
	if binary, err := exec.LookPath("codex"); err == nil {
		return binary, nil
	}
	for _, relative := range []string{"qm/node_modules/.bin/codex", "node_modules/.bin/codex"} {
		for _, candidate := range findOpenCodeUpward(relative) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", errors.New("Codex binary not found; configure qm.models.harnesses[].runtime.binary_path")
}

var codexPermanentPattern = regexp.MustCompile(`(?i)\b(?:401|402|403)\b|unauthoriz|forbidden|invalid[_ -]?api[_ -]?key|incorrect api key|authentication (?:error|failed)|missing bearer|missing (?:api key|credentials)|not logged in|codex login|insufficient[_ -]?quota|exceeded your current quota|billing|credit(?: balance| limit)|out of credits|credits_depleted|must be verified|model[_ -]?not[_ -]?found|does not exist or you do not have access|unsupported[_ -]?model`)

func classifyCodexError(err error) error {
	if err != nil && codexPermanentPattern.MatchString(err.Error()) {
		return &NonRetryableError{Err: err}
	}
	return err
}

func codexReasoningEffort(value string) string {
	switch value {
	case "low", "medium", "high", "xhigh":
		return value
	default:
		return ""
	}
}

func codexReplayItems(raw json.RawMessage) []map[string]any {
	messages := piMessagesFromHistory(raw)
	result := []map[string]any{}
	for _, message := range messages {
		switch message.Role {
		case "user":
			result = append(result, map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": message.Content}}})
		case "assistant":
			if message.Content != "" {
				result = append(result, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": message.Content}}})
			}
			for _, call := range message.ToolCalls {
				result = append(result, map[string]any{"type": "function_call", "call_id": codexReplayCallID(call.ID), "name": call.Name, "arguments": string(call.Arguments)})
			}
		case "tool":
			result = append(result, map[string]any{"type": "function_call_output", "call_id": codexReplayCallID(message.ToolCallID), "output": message.Content})
		}
	}
	return result
}

func codexReplayCallID(value string) string {
	if len(value) <= 64 {
		return value
	}
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func stripCodexInputImages(items []map[string]any) []map[string]any {
	result := make([]map[string]any, len(items))
	for index, item := range items {
		result[index] = cloneAnyMap(item)
		if result[index]["type"] == "image" && strings.HasPrefix(fmt.Sprint(result[index]["url"]), "data:") {
			result[index]["url"] = "[image bytes omitted]"
		}
	}
	return result
}

func codexRedactThreadRequest(request map[string]any) map[string]any {
	result := cloneAnyMap(request)
	result["cwd"] = "[ephemeral control jail]"
	return result
}

func codexTurnText(turn CodexTurn) string {
	all, final, unphased := []string{}, []string{}, []string{}
	for _, item := range turn.Items {
		if item["type"] != "agentMessage" || strings.TrimSpace(fmt.Sprint(item["text"])) == "" {
			continue
		}
		text := fmt.Sprint(item["text"])
		all = append(all, text)
		if item["phase"] == "final_answer" {
			final = append(final, text)
		} else if item["phase"] == nil {
			unphased = append(unphased, text)
		}
	}
	selected := all
	if len(final) > 0 {
		selected = final
	} else if len(unphased) > 0 {
		selected = unphased
	}
	return strings.TrimSpace(strings.Join(selected, "\n"))
}

func codexTurnReasoning(turn CodexTurn) []string {
	result := []string{}
	for _, item := range turn.Items {
		if item["type"] != "reasoning" {
			continue
		}
		if summary, ok := item["summary"].([]any); ok {
			for _, value := range summary {
				if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
					result = append(result, text)
				}
			}
		}
	}
	return result
}

func codexInputTokenUpdate(params map[string]any, prior int64) int {
	usage, _ := params["tokenUsage"].(map[string]any)
	total, _ := usage["total"].(map[string]any)
	last, _ := usage["last"].(map[string]any)
	totalValue := int64(anyNumber(total["inputTokens"]))
	if totalValue <= prior {
		return 0
	}
	lastValue := int(anyNumber(last["inputTokens"]))
	if lastValue > 0 {
		return lastValue
	}
	return int(totalValue - prior)
}

func anyNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	default:
		return 0
	}
}

func startCodexSignals(ctx context.Context, input TurnInput, rpc CodexRPC, threadID string, turnID *string, state *codexTurnState) func() {
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
						state.stopped.Store(true)
						_ = rpc.Request(context.Background(), "turn/interrupt", map[string]any{"threadId": threadID, "turnId": *turnID}, nil)
					case "steer":
						payload, _ := json.Marshal(map[string]any{"text": signal.Text, "ts": signal.CreatedAt, "steered": true})
						_, _ = input.Emit(pollCtx, NewEntry{Type: "user", Payload: payload, ScopeLabel: input.ScopeLabel})
						_ = rpc.Request(pollCtx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": *turnID, "input": []any{map[string]any{"type": "text", "text": signal.Text, "text_elements": []any{}}}}, nil)
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
