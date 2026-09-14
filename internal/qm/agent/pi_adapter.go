package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

const (
	defaultPiSoftModelCalls = 48
	defaultPiMaxModelCalls  = 96
	defaultPiMaxToolCalls   = 128
)

type PiMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Images     []Image    `json:"images,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
}

type PiCompletionRequest struct {
	Model               string
	SystemPrompt        string
	Messages            []PiMessage
	Tools               []ToolDefinition
	MaxOutputTokens     int
	DisableThinking     bool
	ThinkingLevel       string
	FastMode            bool
	AdaptiveThinking    bool
	SystemCacheBoundary *int
}

type PiUsage struct {
	Input, Output, CacheRead, CacheWrite int
}

type PiCompletion struct {
	Text, Thinking, Model string
	ToolCalls             []ToolCall
	Request               json.RawMessage
	Truncated             bool
	StopReason            string
	Transport             json.RawMessage
	Usage                 PiUsage
	TTFTMS, DurationMS    *int
}

type PiModelTransport interface {
	Complete(context.Context, PiCompletionRequest, func(string), func()) (PiCompletion, error)
	Close(context.Context) error
}

type PiAdapter struct {
	models    config.ModelsConfig
	harness   config.ModelHarnessConfig
	transport PiModelTransport
}

func NewPiAdapter(models config.ModelsConfig, transport PiModelTransport) (*PiAdapter, error) {
	harness, ok := models.Harness("pi")
	if !ok {
		return nil, errors.New("pi harness is not configured")
	}
	if transport == nil {
		created, err := NewPiHTTPTransport(models)
		if err != nil {
			return nil, err
		}
		transport = created
	}
	return &PiAdapter{models: models, harness: harness, transport: transport}, nil
}

func (a *PiAdapter) Profile() Profile { return Profiles()["pi"] }

func (a *PiAdapter) ResetSession(context.Context, string) error { return nil }

func (a *PiAdapter) Close(ctx context.Context) error {
	if a == nil || a.transport == nil {
		return nil
	}
	return a.transport.Close(ctx)
}

func (a *PiAdapter) RunTurn(ctx context.Context, input TurnInput) (TurnResult, error) {
	if a == nil || a.transport == nil {
		return TurnResult{}, &NonRetryableError{Err: errors.New("pi transport is not configured")}
	}
	if input.Emit == nil {
		return TurnResult{}, &NonRetryableError{Err: errors.New("pi session entry persistence is not configured")}
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = a.harness.DefaultModel
	}
	if _, ok := a.models.ProviderForModel(model); !ok || !contains(a.harness.ModelIDs, model) {
		return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("pi model %s is not approved", model)}
	}
	modelDefinition, _ := a.models.Model(model)
	provider, _ := a.models.ProviderForModel(model)
	thinkingLevel := strings.TrimSpace(input.ThinkingLevel)
	if thinkingLevel == "" {
		if provider.Protocol == "anthropic" {
			thinkingLevel = "low"
		} else {
			thinkingLevel = "auto"
		}
	}
	turnCtx := ctx
	cancel := func() {}
	wallClockMS := input.TurnWallClockMS
	if wallClockMS <= 0 && a.harness.Runtime.TurnWallClockSeconds > 0 {
		wallClockMS = a.harness.Runtime.TurnWallClockSeconds * 1000
	}
	if wallClockMS > 0 {
		turnCtx, cancel = context.WithTimeout(ctx, time.Duration(wallClockMS)*time.Millisecond)
	}
	defer cancel()
	compileStarted := time.Now()
	messages := piMessagesFromHistory(input.History)
	if len(input.TapeRows) > 0 {
		plan := PlanTapeSeed(input.TapeRows, "pi", input.TapeMode)
		if plan.Skip == "" && len(plan.Seed) > 0 {
			messages = plan.Seed
		}
	}
	if len(messages) == 0 {
		if prior := seedPriorTurnsText(input.PriorTurns); prior != "" {
			messages = append(messages, PiMessage{Role: "user", Content: prior})
		}
	}
	userEntry, err := turnUserEntry(turnCtx, input)
	if err != nil {
		return TurnResult{}, err
	}
	modelCtx, cancelModel := context.WithCancel(turnCtx)
	defer cancelModel()
	signals := startPiSignals(turnCtx, input, cancelModel)
	defer signals.stop()
	modelPrompt := strings.TrimSpace(strings.Join(nonEmpty(input.Input, input.Environment), "\n\n"))
	triggerMessage := PiMessage{Role: "user", Content: modelPrompt, Images: append([]Image(nil), input.Images...)}
	messages = append(messages, triggerMessage)
	tapeWriteFailed := !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(triggerMessage), BareText: input.Input, EntrySequence: &userEntry.Sequence})
	definitions := []ToolDefinition{}
	if input.Tools != nil {
		definitions, err = input.Tools.Definitions(turnCtx, ToolOptions{ReadOnly: input.ReadOnly, SurfaceTools: input.SurfaceTools, SurfaceName: input.SurfaceName})
		if err != nil {
			return TurnResult{}, err
		}
	}
	definitions = piLifecycleTools(definitions, input.SurfaceTools)
	compileMS := int(time.Since(compileStarted).Milliseconds())
	if input.RecordModelCall != nil {
		input.RecordModelCall(ModelCallRecord{Model: model, InputTokens: estimatePiTokens(input.SystemPrompt, messages), EntryCount: historyEntryCount(input.History)})
	}
	result := TurnResult{CompileMS: compileMS, TapeWriteFailed: tapeWriteFailed}
	softModelLimit := a.harness.Runtime.SoftModelCalls
	if softModelLimit <= 0 {
		softModelLimit = defaultPiSoftModelCalls
	}
	modelLimit := a.harness.Runtime.MaxModelCalls
	if modelLimit <= 0 {
		modelLimit = defaultPiMaxModelCalls
	}
	toolLimit := a.harness.Runtime.MaxToolCalls
	if toolLimit <= 0 {
		toolLimit = defaultPiMaxToolCalls
	}
	result.ModelLimit, result.ToolLimit = modelLimit, toolLimit
	var reply string
	var previousEnd time.Time
	toolCalls := 0
	tokens := 0
	softBudgetPrompted := false
	fallbackAttempted := false
	loopStopReason := ""
	for step := 0; step < modelLimit && result.ModelCalls < modelLimit && toolCalls < toolLimit; step++ {

		if _, err := signals.applySteers(turnCtx, input, &messages); err != nil {
			return TurnResult{}, err
		}
		if err := turnCtx.Err(); err != nil {
			if !fallbackAttempted {
				if fallback := a.nextFallbackModel(model); fallback != "" {
					fallbackAttempted = true
					if result.ModelCalls < modelLimit {
						result.ModelCalls++
						if input.OnProgress != nil {
							input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: fallback, Step: step, Phase: "fallback", Strategy: "synthesize"})
						}
						fallbackCtx, cancelFallback := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
						fallbackReply := a.fallbackPiSummary(fallbackCtx, fallback, thinkingLevel, input, messages, err.Error())
						cancelFallback()
						if strings.TrimSpace(fallbackReply) != "" {
							model = fallback
							result.FallbackModel = fallback
							result.CompletionStatus = "incomplete"
							result.Status = "incomplete"
							result.Reason = "the primary model hit its wall-clock limit"
							result.NextAction = "continue verification"
							reply = fallbackReply
							break
						}
					}
				}
			}
			result.CompletionStatus = "incomplete"
			result.Status = "incomplete"
			result.Reason = "the turn hit its wall-clock limit before all requested checks were completed"
			result.NextAction = "continue verification"
			reply = staticPiIncompleteSummary(result)
			break
		}
		started := time.Now()
		partialReply := ""
		result.ModelCalls++
		if input.OnProgress != nil {
			input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Tokens: tokens, Step: step, Phase: "thinking", Strategy: "reason"})
		}
		if result.ModelCalls >= softModelLimit && !softBudgetPrompted {
			messages = append(messages, PiMessage{Role: "user", Content: "[system] The execution budget is nearly exhausted. Finish the current task within the remaining budget; explain any unfinished work honestly."})
			softBudgetPrompted = true
			if input.OnProgress != nil {
				input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit"})
			}
		}
		bufferedModelText := []string{}
		bufferedTextBlockStart := false
		streamedModelText := 0
		streamedTextBlockStart := false
		requestMessages, compaction, contextErr := a.preparePiContext(modelCtx, model, input, messages, modelDefinition.ContextWindow)
		if contextErr != nil {
			return TurnResult{}, &NonRetryableError{Err: contextErr}
		}
		if compaction != nil {
			result.ModelCalls++
			result.CacheUsage = addPiCacheUsage(result.CacheUsage, compaction.Usage)
			messages = requestMessages
			payload, _ := json.Marshal(map[string]any{"kind": "compaction", "summary": compaction.Text})
			if !writePiTape(turnCtx, input, TapeRecord{Kind: "annotation", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: payload}) {
				result.TapeWriteFailed = true
			}
			if result.ModelCalls >= modelLimit {
				loopStopReason = "model budget reached during context compaction"
				break
			}
		}
		completion, err := a.transport.Complete(modelCtx, PiCompletionRequest{
			Model: model, SystemPrompt: input.SystemPrompt, Messages: requestMessages, Tools: definitions,
			MaxOutputTokens: a.models.Request.MaxOutputTokens, DisableThinking: a.models.Request.DisableThinking,
			ThinkingLevel: thinkingLevel, FastMode: input.FastMode && modelDefinition.FastMode, AdaptiveThinking: modelDefinition.AdaptiveThinking,
			SystemCacheBoundary: func() *int {
				if a.harness.Runtime.SystemCacheSplit {
					return input.SystemCacheBoundary
				}
				return nil
			}(),
		}, func(delta string) {
			partialReply += delta
			bufferedModelText = append(bufferedModelText, delta)
			if input.OnDelta != nil {
				input.OnDelta(delta)
				streamedModelText++
			}
		}, func() {
			bufferedTextBlockStart = true
			if input.OnTextBlockStart != nil {
				input.OnTextBlockStart()
				streamedTextBlockStart = true
			}
		})
		if err != nil {
			if signals != nil && signals.aborted.Load() {
				reply := partialReply

				if strings.TrimSpace(reply) == "" {
					reply = "(stopped)"
				}
				payload, _ := json.Marshal(map[string]any{"text": reply, "stopped": true})
				finalEntry, persistErr := input.Emit(turnCtx, NewEntry{Type: "assistant", Payload: payload, ScopeLabel: input.ScopeLabel})
				if persistErr != nil {
					return TurnResult{}, persistErr
				}
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(PiMessage{Role: "assistant", Content: reply})}) {
					result.TapeWriteFailed = true
				}
				checkpoint, _ := json.Marshal(map[string]bool{"subturnEnd": true})
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "annotation", ScopeLabel: input.ScopeLabel, Payload: checkpoint, EntrySequence: &finalEntry.Sequence}) {
					result.TapeWriteFailed = true
				}
				result.Reply = reply
				result.Stopped = true
				result.FinalEntrySequence = &finalEntry.Sequence
				return result, nil
			}
			if turnCtx.Err() != nil {
				if !fallbackAttempted {
					if fallback := a.nextFallbackModel(model); fallback != "" {
						fallbackAttempted = true
						if result.ModelCalls < modelLimit {
							result.ModelCalls++
							if input.OnProgress != nil {
								input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: fallback, Step: step, Phase: "fallback", Strategy: "synthesize"})
							}
							fallbackCtx, cancelFallback := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
							fallbackReply := a.fallbackPiSummary(fallbackCtx, fallback, thinkingLevel, input, messages, "the primary model hit its wall-clock limit")
							cancelFallback()
							if strings.TrimSpace(fallbackReply) != "" {
								model = fallback
								result.FallbackModel = fallback
								result.CompletionStatus = "incomplete"
								result.Status = "incomplete"
								result.Reason = "the primary model hit its wall-clock limit"
								result.NextAction = "continue verification"
								reply = fallbackReply
								break
							}
						}
					}
				}
				result.CompletionStatus = "incomplete"
				result.Status = "incomplete"
				result.Reason = "the turn hit its wall-clock limit before all requested checks were completed"
				result.NextAction = "continue verification"
				reply = staticPiIncompleteSummary(result)
				break
			}
			if !fallbackAttempted {
				if fallback := a.nextFallbackModel(model); fallback != "" {
					fallbackAttempted = true
					if result.ModelCalls < modelLimit {
						result.ModelCalls++
						if input.OnProgress != nil {
							input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: fallback, Step: step, Phase: "fallback", Strategy: "synthesize"})
						}
						fallbackCtx, cancelFallback := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
						fallbackReply := a.fallbackPiSummary(fallbackCtx, fallback, thinkingLevel, input, messages, err.Error())
						cancelFallback()
						if strings.TrimSpace(fallbackReply) != "" {
							model = fallback
							result.FallbackModel = fallback
							result.CompletionStatus = "incomplete"
							result.Status = "incomplete"
							result.Reason = "the primary model failed before completing the turn"
							result.NextAction = "continue verification"
							reply = fallbackReply
							break
						}
					}
				}
			}
			if isPermanentProviderError(err) {
				return TurnResult{}, &NonRetryableError{Err: err}
			}
			if isPiTimeoutError(err) {
				result.ToolCalls = toolCalls
				result.CompletionStatus = "incomplete"
				result.Status = "incomplete"
				result.Reason = "the model request timed out before all requested checks were completed"
				result.NextAction = "retry verification"
				reply = staticPiIncompleteSummary(result)
				break
			}
			return TurnResult{}, err
		}
		allowedToolCalls := len(completion.ToolCalls)
		toolBudgetReached := false
		remainingToolCalls := toolLimit - toolCalls
		if allowedToolCalls > remainingToolCalls {
			allowedToolCalls = max(0, remainingToolCalls)
			toolBudgetReached = true
		}
		toolCalls += allowedToolCalls
		tokens += completion.Usage.Input + completion.Usage.Output
		if input.OnProgress != nil {
			input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Tokens: tokens, Step: step, Phase: "working", Strategy: "tools"})
		}
		emitBufferedModelContent := func() error {

			if bufferedTextBlockStart && !streamedTextBlockStart && input.OnTextBlockStart != nil {
				input.OnTextBlockStart()
			}
			if input.OnDelta != nil {
				for index, delta := range bufferedModelText {
					if index < streamedModelText {
						continue
					}
					input.OnDelta(delta)
				}
			}
			if completion.Thinking != "" {
				payload, _ := json.Marshal(map[string]string{"thinking": completion.Thinking})
				if _, err := input.Emit(turnCtx, NewEntry{Type: "thinking", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
					return err
				}
			}
			if completion.Text != "" && len(completion.ToolCalls) > 0 {
				payload, _ := json.Marshal(map[string]string{"text": completion.Text})
				if _, err := input.Emit(turnCtx, NewEntry{Type: "text", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
					return err
				}
			}
			return nil
		}
		if err := emitBufferedModelContent(); err != nil {
			return TurnResult{}, err
		}
		stepGap := optionalMilliseconds(started.Sub(previousEnd), !previousEnd.IsZero())
		duration := completion.DurationMS
		if duration == nil {
			value := int(time.Since(started).Milliseconds())
			duration = &value
		}
		if input.RecordLLMRequest != nil {
			usage, _ := json.Marshal(map[string]any{
				"input": completion.Usage.Input, "output": completion.Usage.Output,
				"cacheRead": completion.Usage.CacheRead, "cacheWrite": completion.Usage.CacheWrite,
				"totalTokens": completion.Usage.Input + completion.Usage.Output,
			})
			_ = input.RecordLLMRequest(turnCtx, LLMRequestRecord{
				TurnSequence: &userEntry.Sequence, Step: step, Model: firstNonEmpty(completion.Model, model),
				Request: completion.Request, Truncated: completion.Truncated, Transport: completion.Transport, TTFTMS: completion.TTFTMS,
				DurationMS: duration, StepGapMS: stepGap, Usage: usage,
			})
		}
		previousEnd = time.Now()
		result.CacheUsage = addPiCacheUsage(result.CacheUsage, completion.Usage)
		assistantContent := completion.Text

		assistantMessage := PiMessage{Role: "assistant", Content: assistantContent, ToolCalls: completion.ToolCalls}
		messages = append(messages, assistantMessage)
		if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(assistantMessage)}) {
			result.TapeWriteFailed = true
		}
		if len(completion.ToolCalls) == 0 {
			if steered, err := signals.applySteers(turnCtx, input, &messages); err != nil {
				return TurnResult{}, err
			} else if steered > 0 {
				if completion.Text != "" {
					payload, _ := json.Marshal(map[string]string{"text": completion.Text})
					if _, err := input.Emit(turnCtx, NewEntry{Type: "text", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
						return TurnResult{}, err
					}
				}
				continue
			}

			if followed, err := signals.applyFollowUps(turnCtx, input, &messages); err != nil {
				return TurnResult{}, err
			} else if followed > 0 {
				continue
			}
			reply = completion.Text
			break
		}
		terminated := false
		parallelResults, err := parallelPiTools(turnCtx, input, definitions, completion.ToolCalls, allowedToolCalls, completion.StopReason == "length" || completion.StopReason == "max_tokens")
		if err != nil {
			return TurnResult{}, err
		}
		for callIndex, call := range completion.ToolCalls {

			if callIndex >= allowedToolCalls {
				toolResult := ToolResult{Content: []ToolContent{{Type: "text", Text: "[system] The tool budget was reached. This action was skipped; provide the best supported result now."}}, IsError: true}
				text := toolResultText(toolResult)
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayloadForPi(call), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_result", Payload: toolResultEntryPayload(call, toolResult), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(PiMessage{Role: "toolResult", Content: text, ToolCallID: call.ID})}) {
					result.TapeWriteFailed = true
				}
				messages = append(messages, PiMessage{Role: "tool", Content: text, ToolCallID: call.ID})
				continue
			}
			approval := input.ToolApprovalGate != nil && !toolApprovalExempt(call.Name) && !input.ToolApprovalGate(call.Name)
			callPayload, _ := json.Marshal(map[string]any{"tool": call.Name, "callId": call.ID, "arguments": rawJSONObject(call.Arguments)})
			if parallelResults == nil {
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayload, ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
			}
			var toolResult ToolResult
			if parallelResults != nil {
				toolResult = parallelResults[callIndex]
			} else if completion.StopReason == "length" || completion.StopReason == "max_tokens" {
				toolResult = toolError("Model output was truncated; no tool calls from this message were executed. Reissue the complete call.")
			} else if validationErr := validatePiToolCall(call, definitions); validationErr != nil {
				toolResult = toolError(validationErr.Error())
			} else if approval {
				const reason = "strict posture: this tool call requires human approval"
				result.PendingApprovals = append(result.PendingApprovals, PendingApproval{Command: call.Name, Reason: reason, Kind: "approval", ApprovalKey: "tool:" + call.Name})
				result.PausedOnApproval = true
				toolResult = ToolResult{Content: []ToolContent{{Type: "text", Text: "[blocked: needs human approval] " + reason}}, IsError: true, Terminate: true}
			} else if call.Name == "finish_silently" {
				toolResult = finishSilentlyResult(input.PollFire)
			} else if input.Tools == nil {
				toolResult = ToolResult{Content: []ToolContent{{Type: "text", Text: "[error] tool context is unavailable"}}, IsError: true, Terminate: true}
			} else {
				toolResult, err = input.Tools.Execute(turnCtx, call)
				if err != nil {
					toolResult = ToolResult{Content: []ToolContent{{Type: "text", Text: "[tool failed] " + err.Error()}}, IsError: true}
				}
			}
			text := toolResultText(toolResult)
			result.EvidenceCount += evidenceCountFromDetails(toolResult.Details)
			resultPayload := toolResultEntryPayload(call, toolResult)
			if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_result", Payload: resultPayload, ScopeLabel: input.ScopeLabel}); err != nil {
				return TurnResult{}, err
			}
			toolMessage := PiMessage{Role: "toolResult", Content: text, ToolCallID: call.ID}
			messages = append(messages, PiMessage{Role: "tool", Content: text, ToolCallID: call.ID})
			if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(toolMessage)}) {
				result.TapeWriteFailed = true
			}
			result.Silent = result.Silent || toolResult.Silent
			terminated = terminated || toolResult.Terminate

		}
		if terminated || result.PausedOnApproval || result.Silent {
			reply = completion.Text

			break
		}
		if loopStopReason != "" {
			break
		}
		if toolBudgetReached {
			break
		}
	}
	if loopStopReason != "" && reply == "" && !result.Silent && !result.PausedOnApproval {
		result.ToolCalls = toolCalls
		if result.ModelCalls < modelLimit {
			result.ModelCalls++
			reply = a.finalizePiTurn(turnCtx, model, thinkingLevel, input, messages, result, loopStopReason)
		} else {
			reply = staticPiIncompleteSummary(result)
		}
		result.CompletionStatus = "incomplete"
		result.Status = "incomplete"
		result.Reason = loopStopReason
		result.NextAction = "补充未确认条件的证据或修正候选"
		result.Partial = reply
	}
	if (result.ModelCalls >= modelLimit || toolCalls >= toolLimit) && reply == "" && !result.Silent && !result.PausedOnApproval {
		result.ToolCalls = toolCalls
		if result.ModelCalls < modelLimit {
			result.ModelCalls++
			reply = a.finalizePiTurn(turnCtx, model, thinkingLevel, input, messages, result, "execution budget reached")
		} else {
			reply = staticPiIncompleteSummary(result)
		}
		result.CompletionStatus = "incomplete"
		result.Reason = "execution budget reached before all requested checks were completed"
		result.NextAction = "continue verification"
		result.Partial = reply
	}

	if result.CompletionStatus == "" {
		result.CompletionStatus = "ok"
	}
	if result.CompletionStatus == "incomplete" && result.Status == "" {
		result.Status = "incomplete"
	}
	result.ToolCalls = toolCalls
	result.Reply = reply
	assistantPayload, _ := json.Marshal(map[string]string{"text": reply})
	emitCtx := turnCtx
	if emitCtx.Err() != nil {
		emitCtx = context.WithoutCancel(ctx)
	}
	finalEntry, err := input.Emit(emitCtx, NewEntry{Type: "assistant", Payload: assistantPayload, ScopeLabel: input.ScopeLabel})
	if err != nil {
		return TurnResult{}, err
	}
	checkpoint, _ := json.Marshal(map[string]bool{"subturnEnd": true})
	if !writePiTape(emitCtx, input, TapeRecord{Kind: "annotation", ScopeLabel: input.ScopeLabel, Payload: checkpoint, EntrySequence: &finalEntry.Sequence}) {
		result.TapeWriteFailed = true
	}
	result.FinalEntrySequence = &finalEntry.Sequence
	return result, nil
}

func piKnowledgeAction(call ToolCall) string {
	if call.Name != "knowledge" {
		return ""
	}
	var input struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(call.Arguments, &input) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(input.Action))
}

func callPayloadForPi(call ToolCall) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{"tool": call.Name, "callId": call.ID, "arguments": rawJSONObject(call.Arguments)})
	return payload
}

func evidenceCountFromDetails(details json.RawMessage) int {
	if len(details) == 0 || string(details) == "null" {
		return 0
	}
	var value struct {
		Sources []struct {
			Evidence bool `json:"evidence"`
		} `json:"sources"`
	}
	if json.Unmarshal(details, &value) != nil {
		return 0
	}
	count := 0
	for _, source := range value.Sources {
		if source.Evidence {
			count++
		}
	}
	return count
}

func (a *PiAdapter) finalizePiTurn(ctx context.Context, model, thinkingLevel string, input TurnInput, messages []PiMessage, result TurnResult, reason string) string {
	finalMessages := append([]PiMessage(nil), messages...)
	finalMessages = append(finalMessages, PiMessage{Role: "user", Content: fmt.Sprintf("[system] Produce the final user-facing result now. Do not call tools. State the supported conclusion, cite the evidence already collected, and list unresolved requirements explicitly. Mark it incomplete when evidence is missing. Reason: %s", reason)})
	completion, err := a.transport.Complete(ctx, PiCompletionRequest{Model: model, SystemPrompt: input.SystemPrompt, Messages: finalMessages, Tools: nil, MaxOutputTokens: a.models.Request.MaxOutputTokens, DisableThinking: a.models.Request.DisableThinking, ThinkingLevel: thinkingLevel}, nil, nil)
	if err == nil && strings.TrimSpace(completion.Text) != "" {
		return completion.Text
	}
	return staticPiIncompleteSummary(result)
}

func staticPiIncompleteSummary(result TurnResult) string {
	return fmt.Sprintf("已完成部分核验，但尚未形成完整结论。已调用 %d 次模型、%d 次工具并读取 %d 份证据。请继续核验未完成项。", result.ModelCalls, result.ToolCalls, result.EvidenceCount)
}

func (a *PiAdapter) nextFallbackModel(current string) string {
	candidates := append([]string(nil), a.harness.Runtime.FallbackModels...)
	if len(candidates) == 0 {
		candidates = append(candidates, a.harness.ModelIDs...)
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == current || !contains(a.harness.ModelIDs, candidate) {
			continue
		}
		if _, ok := a.models.ProviderForModel(candidate); ok {
			return candidate
		}
	}
	return ""
}

func (a *PiAdapter) fallbackPiSummary(ctx context.Context, model, thinkingLevel string, input TurnInput, messages []PiMessage, reason string) string {
	finalMessages := append([]PiMessage(nil), messages...)
	finalMessages = append(finalMessages, PiMessage{Role: "user", Content: fmt.Sprintf("[system] The primary model could not finish. Do not call tools. Based only on the evidence already present, produce a concise user-facing answer, clearly mark it incomplete, and list what remains unverified. Failure: %s", reason)})
	completion, err := a.transport.Complete(ctx, PiCompletionRequest{Model: model, SystemPrompt: input.SystemPrompt, Messages: finalMessages, Tools: nil, MaxOutputTokens: a.models.Request.MaxOutputTokens, DisableThinking: a.models.Request.DisableThinking, ThinkingLevel: thinkingLevel}, nil, nil)
	if err != nil {
		return ""
	}
	return completion.Text
}

func writePiTape(ctx context.Context, input TurnInput, record TapeRecord) bool {
	if input.Tape == nil {
		return true
	}
	return input.Tape(ctx, record) == nil
}

func piTapeMessage(message PiMessage) json.RawMessage {
	copy := message
	if copy.Role == "tool" {
		copy.Role = "toolResult"
	}
	copy.Images = append([]Image(nil), message.Images...)
	for index := range copy.Images {
		copy.Images[index].DataBase64 = ""
	}
	encoded, err := json.Marshal(copy)
	if err != nil {
		return json.RawMessage(`{"role":"unknown"}`)
	}
	return encoded
}

func piLifecycleTools(definitions []ToolDefinition, surfaceTools bool) []ToolDefinition {
	name := "finish_silently"
	description := "End a scheduled background turn immediately without sending anything when there is nothing worth reporting. On an attended turn this is a no-op."
	schema := json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}},"additionalProperties":false}`)
	if surfaceTools {
		name = "stay_silent"
		description = "Explicitly end an addressed surface turn without posting anything; provide a one-line audit reason."
		schema = json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"],"additionalProperties":false}`)
	}
	for _, definition := range definitions {
		if definition.Name == name {
			return definitions
		}
	}
	return append(definitions, ToolDefinition{Name: name, Description: description, InputSchema: schema})
}

func finishSilentlyResult(pollFire bool) ToolResult {
	if !pollFire {
		return ToolResult{Content: []ToolContent{{Type: "text", Text: "[no-op] finish_silently only applies to scheduled background fires; a person is waiting on this turn — just reply."}}}
	}
	return ToolResult{Content: []ToolContent{{Type: "text", Text: "Ending this turn silently — nothing will be delivered."}}, Terminate: true, Silent: true}
}

func piMessagesFromHistory(raw json.RawMessage) []PiMessage {
	var entries []SessionEntry
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	messages := make([]PiMessage, 0, len(entries))
	for _, entry := range entries {
		var payload map[string]json.RawMessage
		if json.Unmarshal(entry.Payload, &payload) != nil {
			continue
		}
		text := rawString(payload["text"])
		switch entry.Type {
		case "user":
			if text != "" {
				messages = append(messages, PiMessage{Role: "user", Content: text})
			}
		case "assistant", "text":
			if text != "" {
				messages = append(messages, PiMessage{Role: "assistant", Content: text})
			}
		case "tool_call":
			callID := rawString(payload["callId"])
			name := rawString(payload["tool"])
			arguments := payload["arguments"]
			if len(arguments) == 0 || !json.Valid(arguments) {
				arguments = piArgumentsFromToolEntry(payload)
			}
			if callID != "" && name != "" {
				messages = append(messages, PiMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: callID, Name: name, Arguments: arguments}}})
			}
		case "tool_result":
			callID := rawString(payload["callId"])
			result := rawString(payload["result"])
			if callID != "" {
				messages = append(messages, PiMessage{Role: "tool", ToolCallID: callID, Content: result})
			}
		}
	}
	return messages
}

func piArgumentsFromToolEntry(payload map[string]json.RawMessage) json.RawMessage {
	arguments := map[string]json.RawMessage{}
	for key, value := range payload {
		switch key {
		case "tool", "callId", "blocked", "denied", "reason", "approvalKey":
			continue
		}
		arguments[key] = value
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawJSONArray(raw json.RawMessage) any {
	var value []any
	if json.Unmarshal(raw, &value) != nil {
		return []any{}
	}
	return value
}

func rawJSONObject(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return map[string]any{}
	}
	return value
}

func toolResultText(result ToolResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if content.Type == "text" {
			parts = append(parts, content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toolApprovalExempt(name string) bool { return name == "finish_silently" || name == "stay_silent" }

func estimatePiTokens(system string, messages []PiMessage) int {
	characters := len(system)
	for _, message := range messages {
		characters += len(message.Content)
		for _, call := range message.ToolCalls {
			characters += len(call.ID) + len(call.Name) + len(call.Arguments)
		}
	}
	return (characters + 3) / 4
}

func historyEntryCount(raw json.RawMessage) int {
	var entries []json.RawMessage
	_ = json.Unmarshal(raw, &entries)
	return len(entries)
}

func nonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func optionalMilliseconds(duration time.Duration, present bool) *int {
	if !present {
		return nil
	}
	value := int(duration.Milliseconds())
	return &value
}

func addPiCacheUsage(existing *CacheUsage, usage PiUsage) *CacheUsage {
	if usage.Input == 0 && usage.Output == 0 && usage.CacheRead == 0 && usage.CacheWrite == 0 && existing == nil {
		return nil
	}
	if existing == nil {
		existing = &CacheUsage{}
	}
	existing.CacheRead += usage.CacheRead
	existing.CacheWrite += usage.CacheWrite
	existing.UncachedInput += max(0, usage.Input-usage.CacheRead-usage.CacheWrite)
	return existing
}

func piContextError(err error, wallClockMS int) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &NonRetryableError{Err: fmt.Errorf("the turn hit its %s wall-clock limit and was stopped", time.Duration(wallClockMS)*time.Millisecond)}
	}
	return err
}

type permanentProviderError interface{ Permanent() bool }

func isPermanentProviderError(err error) bool {
	var typed permanentProviderError
	return errors.As(err, &typed) && typed.Permanent()
}

func isPiTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "client.timeout") ||
		strings.Contains(message, "context deadline exceeded") ||
		strings.Contains(message, "context canceled")
}
