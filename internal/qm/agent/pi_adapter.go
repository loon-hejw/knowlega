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
	defaultPiSoftModelCalls   = 48
	defaultPiMaxModelCalls    = 96
	defaultPiMaxToolCalls     = 128
	duplicateActionLimit      = 3
	knowledgeSubmitStallLimit = 3
	knowledgeNoProgressLimit  = 12
	knowledgePreSubmitLimit   = 24
	// After enough navigation, give the model a tiny submit-only window so it
	// cannot spend the remaining budget on more searches without producing a
	// validation result.
	knowledgeSubmitOnlyLimit  = 1
	knowledgeBlockedToolLimit = 16
	piConversationCharLimit   = 240000
	piRecentFullMessageCount  = 24
	piOldMessageCharLimit     = 1400
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

type piKnowledgeValidation struct {
	blocked                  bool
	halt                     bool
	knowledgeUsed            bool
	submitComplete           bool
	answer                   string
	status                   string
	unresolvedRequirementIDs []string
	validationIssueCodes     []string
}

type piKnowledgeResultPayload struct {
	Status                   string   `json:"status"`
	Answer                   string   `json:"answer"`
	Code                     string   `json:"code"`
	UnresolvedRequirementIDs []string `json:"unresolved_requirement_ids"`
	ValidationIssues         []struct {
		Code string `json:"code"`
	} `json:"validation_issues"`
	Workspace struct {
		Status string `json:"status"`
	} `json:"workspace"`
	WorkspaceStatus string `json:"workspace_status"`
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
	knowledgeToolAvailable := false
	for _, definition := range definitions {
		if definition.Name == "knowledge" {
			knowledgeToolAvailable = true
			break
		}
	}
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
	terminatedTurn := false
	toolCalls := 0
	tokens := 0
	emptyEndingRetried := false
	lastActions := map[string]int{}
	softBudgetPrompted := false
	stalledPrompted := false
	fallbackAttempted := false
	loopStopReason := ""
	knowledgeSubmitStalls := 0
	knowledgeSubmitEvidence := -1
	knowledgeNoProgress := 0
	knowledgeCallsBeforeSubmit := 0
	knowledgeSubmitAttempted := false
	knowledgeFinalizationPrompted := false
	knowledgeSubmitOnly := false
	knowledgeSubmitOnlyAttempts := 0
	knowledgeValidation := piKnowledgeValidation{}
	knowledgeBlockedAtToolCall := -1
	for step := 0; step < modelLimit && result.ModelCalls < modelLimit && toolCalls < toolLimit; step++ {
		if knowledgeValidation.halt {
			loopStopReason = "project knowledge workspace is not ready for validation"
			break
		}
		if knowledgeValidation.blocked && knowledgeBlockedAtToolCall >= 0 && toolCalls-knowledgeBlockedAtToolCall >= knowledgeBlockedToolLimit {
			loopStopReason = "project knowledge validation is blocked after bounded evidence collection"
			break
		}
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
			messages = append(messages, PiMessage{Role: "user", Content: "[system] The execution budget is nearly exhausted. Stop broad searching, use only missing evidence actions, submit the current verification, and provide the best supported answer now."})
			softBudgetPrompted = true
			if input.OnProgress != nil {
				input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit"})
			}
		}
		suppressModelText := knowledgeValidation.blocked
		bufferedModelText := []string{}
		bufferedTextBlockStart := false
		streamedModelText := 0
		streamedTextBlockStart := false
		requestMessages := compactPiMessages(messages, piConversationCharLimit)
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
			if !knowledgeToolAvailable && !knowledgeValidation.withholdModelText() && input.OnDelta != nil {
				input.OnDelta(delta)
				streamedModelText++
			}
		}, func() {
			bufferedTextBlockStart = true
			if !knowledgeToolAvailable && !knowledgeValidation.withholdModelText() && input.OnTextBlockStart != nil {
				input.OnTextBlockStart()
				streamedTextBlockStart = true
			}
		})
		if err != nil {
			if signals != nil && signals.aborted.Load() {
				reply := partialReply
				if knowledgeValidation.withholdModelText() {
					reply = knowledgeValidation.summary()
				}
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
		completionHasKnowledge := false
		for _, call := range completion.ToolCalls {
			completionHasKnowledge = completionHasKnowledge || call.Name == "knowledge"
			if call.Name == "knowledge" {
				knowledgeValidation.noteCall(piKnowledgeAction(call))
			}
		}
		deferModelText := completionHasKnowledge
		emitBufferedModelContent := func() error {
			if knowledgeValidation.withholdModelText() {
				return nil
			}
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
		if !deferModelText {
			if err := emitBufferedModelContent(); err != nil {
				return TurnResult{}, err
			}
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
		if suppressModelText || knowledgeValidation.withholdModelText() || completionHasKnowledge {
			assistantContent = ""
		}
		assistantMessage := PiMessage{Role: "assistant", Content: assistantContent, ToolCalls: completion.ToolCalls}
		messages = append(messages, assistantMessage)
		if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(assistantMessage)}) {
			result.TapeWriteFailed = true
		}
		if len(completion.ToolCalls) == 0 {
			if steered, err := signals.applySteers(turnCtx, input, &messages); err != nil {
				return TurnResult{}, err
			} else if steered > 0 {
				if !knowledgeValidation.withholdModelText() && completion.Text != "" {
					payload, _ := json.Marshal(map[string]string{"text": completion.Text})
					if _, err := input.Emit(turnCtx, NewEntry{Type: "text", Payload: payload, ScopeLabel: input.ScopeLabel}); err != nil {
						return TurnResult{}, err
					}
				}
				continue
			}
			if knowledgeSubmitOnly && knowledgeValidation.needsSubmit() && !knowledgeValidation.blocked && !knowledgeValidation.halt {
				// Keep a bounded submit-only window even when the model emits
				// prose instead of a tool call. Candidate text remains withheld
				// until a complete submit is observed.
				knowledgeSubmitOnlyAttempts++
				if knowledgeSubmitOnlyAttempts >= knowledgeSubmitOnlyLimit || result.ModelCalls >= modelLimit || toolCalls >= toolLimit {
					loopStopReason = "project knowledge validation did not produce a submit call"
					break
				}
				messages = append(messages, PiMessage{Role: "user", Content: "[system] Submission mode is active. Do not answer or search/read. Call knowledge action=submit now with the full question, candidate, requirements, and exactly one check per requirement. The candidate is withheld until submit returns status=complete."})
				if input.OnProgress != nil {
					input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit", Stalled: true})
				}
				continue
			}
			if knowledgeValidation.needsSubmit() && !knowledgeValidation.halt && !knowledgeFinalizationPrompted && result.ModelCalls < modelLimit && toolCalls < toolLimit {
				knowledgeFinalizationPrompted = true
				knowledgeSubmitOnly = !knowledgeValidation.blocked
				knowledgeSubmitOnlyAttempts = 0
				messages = append(messages, PiMessage{Role: "user", Content: "[system] You attempted to finish before validating the project answer. Do not repeat the answer yet. Call knowledge action=submit now with the full question, answer, candidate, requirements, and exactly one check per requirement. If submit is incomplete, follow only its validation_issues."})
				if input.OnProgress != nil {
					input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit", Stalled: true})
				}
				continue
			}
			if strings.TrimSpace(completion.Text) == "" && !emptyEndingRetried && !input.SurfaceTools && (result.ModelCalls > 1 || !input.PollFire) {
				note := "[system] The turn ended with an empty message. If the work above is unfinished, continue it — without redoing steps that already succeeded; otherwise reply with your answer now."
				if input.PollFire {
					note = "[system] The turn ended with an empty message. If the work above produced something worth reporting (or is still mid-flight), reply with a brief status now; if there is genuinely nothing to report, call finish_silently."
				}
				emptyEndingRetried = true
				messages = append(messages, PiMessage{Role: "user", Content: note})
				continue
			}
			reply = completion.Text
			break
		}
		terminated := false
		for callIndex, call := range completion.ToolCalls {
			if knowledgeValidation.submitComplete {
				toolResult := piRuntimeSkipResult("post_validation_call_skipped", "[system] Project knowledge validation is already complete; this later action was skipped.", 1)
				text := toolResultText(toolResult)
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayloadForPi(call), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_result", Payload: toolResultEntryPayload(call, toolResult), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				messages = append(messages, PiMessage{Role: "tool", Content: text, ToolCallID: call.ID})
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(PiMessage{Role: "toolResult", Content: text, ToolCallID: call.ID})}) {
					result.TapeWriteFailed = true
				}
				continue
			}
			if knowledgeValidation.halt {
				loopStopReason = "project knowledge workspace is not ready for validation"
				toolResult := ToolResult{Content: []ToolContent{{Type: "text", Text: "[system] The project knowledge workspace is not ready; this action was skipped."}}, IsError: true}
				text := toolResultText(toolResult)
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayloadForPi(call), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_result", Payload: toolResultEntryPayload(call, toolResult), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				messages = append(messages, PiMessage{Role: "tool", Content: text, ToolCallID: call.ID})
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(PiMessage{Role: "toolResult", Content: text, ToolCallID: call.ID})}) {
					result.TapeWriteFailed = true
				}
				continue
			}
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
			actionKey := piToolActionKey(call)
			lastActions[actionKey]++
			if lastActions[actionKey] >= duplicateActionLimit {
				if !stalledPrompted {
					stalledPrompted = true
					if input.OnProgress != nil {
						input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "retrying", Strategy: "redirect", Stalled: true})
					}
				}
				toolResult := piRuntimeSkipResult("duplicate_action_skipped", "[system] This identical tool action was skipped after repeated use. Choose a different action that advances the answer.", lastActions[actionKey])
				text := toolResultText(toolResult)
				resultPayload := toolResultEntryPayload(call, toolResult)
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayloadForPi(call), ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_result", Payload: resultPayload, ScopeLabel: input.ScopeLabel}); err != nil {
					return TurnResult{}, err
				}
				if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(PiMessage{Role: "toolResult", Content: text, ToolCallID: call.ID})}) {
					result.TapeWriteFailed = true
				}
				messages = append(messages, PiMessage{Role: "tool", Content: text, ToolCallID: call.ID})
				if piKnowledgeAction(call) == "submit" {
					knowledgeSubmitStalls++
					if knowledgeSubmitStalls >= knowledgeSubmitStallLimit {
						loopStopReason = "knowledge validation did not advance after repeated incomplete submissions"
					}
				}
				continue
			}
			approval := input.ToolApprovalGate != nil && !toolApprovalExempt(call.Name) && !input.ToolApprovalGate(call.Name)
			callPayload, _ := json.Marshal(map[string]any{"tool": call.Name, "callId": call.ID, "arguments": rawJSONObject(call.Arguments)})
			if _, err := input.Emit(turnCtx, NewEntry{Type: "tool_call", Payload: callPayload, ScopeLabel: input.ScopeLabel}); err != nil {
				return TurnResult{}, err
			}
			var toolResult ToolResult
			knowledgeAction := piKnowledgeAction(call)
			if call.Name == "knowledge" {
				if knowledgeAction == "submit" {
					knowledgeSubmitAttempted = true
					knowledgeCallsBeforeSubmit = 0
					knowledgeFinalizationPrompted = false
					knowledgeSubmitOnly = false
					knowledgeSubmitOnlyAttempts = 0
				} else if !knowledgeSubmitAttempted {
					knowledgeCallsBeforeSubmit++
				}
			}
			priorEvidence := result.EvidenceCount
			if knowledgeSubmitOnly && knowledgeValidation.needsSubmit() && !knowledgeValidation.blocked && (call.Name != "knowledge" || knowledgeAction != "submit") {
				// Once convergence has been requested, navigation is no longer
				// useful. Feed a recoverable error back to the model so it can
				// issue the required submit without bypassing the evidence gate.
				toolResult = ToolResult{Content: []ToolContent{{Type: "text", Text: "[system] Submission mode is active; this action was skipped. Call knowledge action=submit now. Do not search, read, or use another tool."}}, IsError: true}
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
			if call.Name == "knowledge" {
				wasBlocked := knowledgeValidation.blocked
				knowledgeValidation.observe(knowledgeAction, toolResult)
				if knowledgeValidation.submitComplete && knowledgeValidation.answer == "" {
					knowledgeValidation.answer = piKnowledgeCallAnswer(call)
				}
				if !knowledgeValidation.blocked {
					knowledgeBlockedAtToolCall = -1
				} else if !wasBlocked {
					knowledgeBlockedAtToolCall = toolCalls
				} else if !toolResult.IsError && piKnowledgeRepairProgress(call, toolResult) {
					knowledgeBlockedAtToolCall = toolCalls
				}
			}
			result.EvidenceCount += evidenceCountFromDetails(toolResult.Details)
			if call.Name == "knowledge" {
				if !toolResult.IsError && (result.EvidenceCount > priorEvidence || piKnowledgeRepairProgress(call, toolResult)) {
					knowledgeNoProgress = 0
				} else {
					knowledgeNoProgress++
				}
				if knowledgeAction == "submit" && piKnowledgeSubmitIncomplete(toolResult) {
					if knowledgeSubmitEvidence == result.EvidenceCount {
						knowledgeSubmitStalls++
					} else {
						knowledgeSubmitEvidence = result.EvidenceCount
						knowledgeSubmitStalls = 1
					}
					if knowledgeSubmitStalls >= knowledgeSubmitStallLimit {
						loopStopReason = "knowledge validation did not advance after repeated incomplete submissions"
					}
				} else if knowledgeAction == "submit" {
					knowledgeSubmitStalls = 0
					knowledgeSubmitEvidence = result.EvidenceCount
				}
			}
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
			if knowledgeValidation.halt {
				loopStopReason = "project knowledge workspace is not ready for validation"
			}
		}
		knowledgeNeedsFinalization := !knowledgeValidation.submitComplete && (knowledgeNoProgress >= knowledgeNoProgressLimit || (!knowledgeSubmitAttempted && knowledgeCallsBeforeSubmit >= knowledgePreSubmitLimit))
		if loopStopReason == "" && knowledgeNeedsFinalization {
			if knowledgeSubmitOnly && knowledgeValidation.needsSubmit() && !knowledgeValidation.blocked && !knowledgeValidation.halt && !terminated && !result.PausedOnApproval && !result.Silent && result.ModelCalls < modelLimit && toolCalls < toolLimit {
				knowledgeSubmitOnlyAttempts++
				if knowledgeSubmitOnlyAttempts >= knowledgeSubmitOnlyLimit {
					loopStopReason = "project knowledge validation did not produce a submit call"
				} else {
					knowledgeNoProgress = 0
					knowledgeCallsBeforeSubmit = 0
					messages = append(messages, PiMessage{Role: "user", Content: "[system] Submission mode is active. Stop all navigation and call knowledge action=submit now. Include exactly one check for every requirement; do not provide a final answer until status=complete."})
					if input.OnProgress != nil {
						input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit", Stalled: true})
					}
				}
			} else if knowledgeValidation.needsSubmit() && !knowledgeValidation.halt && !knowledgeFinalizationPrompted && !terminated && !result.PausedOnApproval && !result.Silent && result.ModelCalls < modelLimit && toolCalls < toolLimit {
				knowledgeFinalizationPrompted = true
				knowledgeSubmitOnly = !knowledgeValidation.blocked
				knowledgeSubmitOnlyAttempts = 0
				knowledgeNoProgress = 0
				knowledgeCallsBeforeSubmit = 0
				messages = append(messages, PiMessage{Role: "user", Content: "[system] Enough knowledge navigation has been attempted without a validation submission. Stop broad searching. Choose the best candidate supported by the evidence already collected and call knowledge action=submit now with the full question, answer, candidate, requirements, and exactly one check per requirement. If submit is incomplete, follow only its validation_issues."})
				if input.OnProgress != nil {
					input.OnProgress(Progress{ToolCalls: toolCalls, ModelCalls: result.ModelCalls, EvidenceCount: result.EvidenceCount, ModelLimit: modelLimit, ToolLimit: toolLimit, Model: model, Step: step, Phase: "finalizing", Strategy: "submit", Stalled: true})
				}
			} else {
				loopStopReason = "project knowledge actions did not add new evidence"
			}
		}
		if deferModelText {
			if !knowledgeValidation.submitComplete && !knowledgeValidation.withholdModelText() {
				assistantMessage.Content = completion.Text
				messages[len(messages)-1] = assistantMessage
			}
			if !knowledgeValidation.submitComplete {
				if err := emitBufferedModelContent(); err != nil {
					return TurnResult{}, err
				}
			}
		}
		if knowledgeValidation.submitComplete {
			reply = knowledgeValidation.answer
			if strings.TrimSpace(reply) == "" {
				return TurnResult{}, &NonRetryableError{Err: errors.New("complete knowledge submission returned an empty answer")}
			}
			if input.OnTextBlockStart != nil {
				input.OnTextBlockStart()
			}
			if input.OnDelta != nil {
				input.OnDelta(reply)
			}
			finalMessage := PiMessage{Role: "assistant", Content: reply}
			messages = append(messages, finalMessage)
			if !writePiTape(turnCtx, input, TapeRecord{Kind: "message", Harness: "pi", ScopeLabel: input.ScopeLabel, Payload: piTapeMessage(finalMessage)}) {
				result.TapeWriteFailed = true
			}
			break
		}
		if terminated || result.PausedOnApproval || result.Silent {
			terminatedTurn = terminated
			reply = completion.Text
			if knowledgeValidation.withholdModelText() {
				reply = knowledgeValidation.summary()
				result.CompletionStatus = "incomplete"
				result.Status = "incomplete"
			}
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
		if knowledgeValidation.withholdModelText() {
			reply = knowledgeValidation.summary()
		} else if result.ModelCalls < modelLimit {
			result.ModelCalls++
			reply = a.finalizePiTurn(turnCtx, model, thinkingLevel, input, messages, result, loopStopReason)
		} else {
			reply = staticPiIncompleteSummary(result)
		}
		result.CompletionStatus = "incomplete"
		result.Status = "incomplete"
		result.Reason = loopStopReason
		result.NextAction = "follow validation_issues and run the requested repair action"
		result.Partial = reply
	}
	if (result.ModelCalls >= modelLimit || toolCalls >= toolLimit) && reply == "" && !result.Silent && !result.PausedOnApproval {
		result.ToolCalls = toolCalls
		if knowledgeValidation.withholdModelText() {
			reply = knowledgeValidation.summary()
		} else if result.ModelCalls < modelLimit {
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
	if !result.Silent && !result.PausedOnApproval && !terminatedTurn && strings.TrimSpace(reply) == "" {
		result.ToolCalls = toolCalls
		if knowledgeValidation.withholdModelText() {
			reply = knowledgeValidation.summary()
		} else if result.ModelCalls < modelLimit {
			result.ModelCalls++
			reply = a.finalizePiTurn(turnCtx, model, thinkingLevel, input, messages, result, "the model returned no final text")
		} else {
			reply = staticPiIncompleteSummary(result)
		}
		if strings.TrimSpace(reply) == "" {
			return TurnResult{}, &NonRetryableError{Err: errors.New("pi returned an empty final response")}
		}
		result.CompletionStatus = "incomplete"
		result.Reason = "the model returned no final text after tool execution"
		result.NextAction = "retry with another model"
		result.Partial = reply
	}
	if knowledgeValidation.withholdModelText() && !result.Silent {
		reply = knowledgeValidation.summary()
		result.CompletionStatus = "incomplete"
		result.Status = "incomplete"
		if result.Reason == "" {
			result.Reason = knowledgeValidation.reason()
		}
		result.NextAction = "follow validation_issues and submit again until status=complete"
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

func piToolActionKey(call ToolCall) string {
	arguments := rawJSONObject(call.Arguments)
	encoded, _ := json.Marshal(arguments)
	return call.Name + "\x00" + string(encoded)
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

func piKnowledgeEvidenceAction(action string) bool {
	switch action {
	case "read", "follow_links", "graph":
		return true
	default:
		return false
	}
}

func piKnowledgeCandidateSearch(call ToolCall) bool {
	if call.Name != "knowledge" {
		return false
	}
	var input struct {
		Action        string `json:"action"`
		Candidate     string `json:"candidate"`
		RequirementID string `json:"requirement_id"`
	}
	if json.Unmarshal(call.Arguments, &input) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(input.Action), "search") && strings.TrimSpace(input.Candidate) != "" && strings.TrimSpace(input.RequirementID) != ""
}

func piKnowledgeRepairProgress(call ToolCall, result ToolResult) bool {
	action := piKnowledgeAction(call)
	if piKnowledgeEvidenceAction(action) {
		return true
	}
	return piKnowledgeCandidateSearchReturnedResults(call, result)
}

// Candidate-specific searches are only repair progress when they actually
// return a result. A zero-result search is meaningful for a negative
// requirement, but treating every such search as progress lets a model cycle
// through paraphrases forever without reaching submit/blocked convergence.
func piKnowledgeCandidateSearchReturnedResults(call ToolCall, result ToolResult) bool {
	if !piKnowledgeCandidateSearch(call) || result.IsError || len(result.Details) == 0 || string(result.Details) == "null" {
		return false
	}
	var details struct {
		Action  string            `json:"action"`
		Sources []json.RawMessage `json:"sources"`
	}
	if json.Unmarshal(result.Details, &details) != nil {
		return false
	}
	if action := strings.TrimSpace(details.Action); action != "" && !strings.EqualFold(action, "search") {
		return false
	}
	return len(details.Sources) > 0
}

func piKnowledgeSubmitIncomplete(result ToolResult) bool {
	var value struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(toolResultText(result)), &value) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(value.Status), "incomplete")
}

func piRuntimeSkipResult(code, message string, attempts int) ToolResult {
	details, _ := json.Marshal(map[string]any{
		"kind":      "runtime",
		"errorCode": code,
		"attempts":  attempts,
		"retryable": false,
	})
	return ToolResult{Content: []ToolContent{{Type: "text", Text: message}}, Details: details, IsError: true}
}

func piKnowledgeCallAnswer(call ToolCall) string {
	if call.Name != "knowledge" {
		return ""
	}
	var input struct {
		Action string `json:"action"`
		Answer string `json:"answer"`
	}
	if json.Unmarshal(call.Arguments, &input) != nil || !strings.EqualFold(strings.TrimSpace(input.Action), "submit") {
		return ""
	}
	return strings.TrimSpace(input.Answer)
}

func (s *piKnowledgeValidation) noteCall(action string) {
	if s == nil {
		return
	}
	s.knowledgeUsed = true
	if action == "" {
		s.status = "incomplete"
	}
}

func (s piKnowledgeValidation) needsSubmit() bool {
	return s.knowledgeUsed && !s.submitComplete
}

func (s piKnowledgeValidation) withholdModelText() bool {
	return s.blocked || s.needsSubmit()
}

func (s *piKnowledgeValidation) observe(action string, result ToolResult) {
	s.noteCall(action)
	payloads := make([]piKnowledgeResultPayload, 0, 2)
	for _, raw := range []json.RawMessage{json.RawMessage(toolResultText(result)), result.Details} {
		var payload piKnowledgeResultPayload
		if len(raw) > 0 && json.Unmarshal(raw, &payload) == nil {
			payloads = append(payloads, payload)
		}
	}
	status := ""
	answer := ""
	code := ""
	workspaceStatus := ""
	var unresolved []string
	var issues []string
	for _, payload := range payloads {
		if value := strings.ToLower(strings.TrimSpace(payload.Status)); value != "" {
			status = value
		}
		if value := strings.TrimSpace(payload.Answer); value != "" {
			answer = value
		}
		if value := strings.ToLower(strings.TrimSpace(payload.Code)); value != "" {
			code = value
		}
		if value := strings.ToLower(strings.TrimSpace(payload.Workspace.Status)); value != "" {
			workspaceStatus = value
		}
		if value := strings.ToLower(strings.TrimSpace(payload.WorkspaceStatus)); value != "" {
			workspaceStatus = value
		}
		unresolved = appendUniqueStrings(unresolved, payload.UnresolvedRequirementIDs...)
		for _, issue := range payload.ValidationIssues {
			issues = appendUniqueStrings(issues, issue.Code)
		}
	}
	if action == "submit" && status == "complete" && !result.IsError {
		s.blocked = false
		s.halt = false
		s.submitComplete = true
		s.answer = answer
		s.status = ""
		s.unresolvedRequirementIDs = nil
		s.validationIssueCodes = nil
		return
	}
	if action == "submit" && status == "incomplete" {
		s.block("submit=incomplete", unresolved, issues, false)
		return
	}
	switch code {
	case "knowledge_unavailable":
		if action == "status" {
			s.block("unavailable", unresolved, issues, true)
		}
		return
	case "knowledge_empty":
		s.block("empty", unresolved, issues, true)
		return
	case "knowledge_failed":
		s.block("failed", unresolved, issues, true)
		return
	}
	if result.IsError {
		if action == "status" {
			s.block("failed", unresolved, issues, true)
		}
		return
	}
	for _, value := range []string{status, workspaceStatus} {
		switch value {
		case "empty":
			s.block("empty", unresolved, issues, true)
			return
		case "queued", "processing", "pending":
			s.block("pending", unresolved, issues, true)
			return
		case "failed":
			s.block("failed", unresolved, issues, true)
			return
		}
	}
}

func (s *piKnowledgeValidation) block(status string, unresolved, issues []string, halt bool) {
	s.blocked = true
	s.halt = s.halt || halt
	s.status = status
	if len(unresolved) > 0 {
		s.unresolvedRequirementIDs = append([]string(nil), unresolved...)
	}
	if len(issues) > 0 {
		s.validationIssueCodes = append([]string(nil), issues...)
	}
}

func (s piKnowledgeValidation) reason() string {
	if s.needsSubmit() && !s.blocked {
		return "project knowledge validation requires a complete submit"
	}
	return "project knowledge validation remains " + firstNonEmpty(s.status, "incomplete")
}

func (s piKnowledgeValidation) summary() string {
	lines := []string{
		"项目知识核验尚未完成，因此不能给出确定的候选答案。",
		"当前验证状态：" + firstNonEmpty(s.status, "incomplete") + "。",
	}
	if len(s.unresolvedRequirementIDs) > 0 {
		lines = append(lines, "未解决条件："+strings.Join(s.unresolvedRequirementIDs, "、")+"。")
	}
	if len(s.validationIssueCodes) > 0 {
		lines = append(lines, "验证问题："+strings.Join(s.validationIssueCodes, "、")+"。")
	} else if s.needsSubmit() {
		lines = append(lines, "验证问题：submit_required。")
	}
	lines = append(lines, "请按 validation_issues 补齐本轮证据并重新提交；只有 knowledge submit 返回 status=complete 后才能给出结论。")
	return strings.Join(lines, "\n")
}

func appendUniqueStrings(values []string, candidates ...string) []string {
	seen := make(map[string]bool, len(values)+len(candidates))
	for _, value := range values {
		seen[value] = true
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		values = append(values, candidate)
	}
	return values
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
	}
	return (characters + 3) / 4
}

func compactPiMessages(messages []PiMessage, limit int) []PiMessage {
	if limit <= 0 {
		return messages
	}
	result := append([]PiMessage(nil), messages...)
	total := piMessageChars(result)
	if total <= limit {
		return result
	}
	protected := max(0, len(result)-piRecentFullMessageCount)
	for index := 0; index < protected && total > limit; index++ {
		content := result[index].Content
		if len(content) <= piOldMessageCharLimit {
			continue
		}
		compacted := compactPiMessageContent(content, piOldMessageCharLimit)
		total -= len(content) - len(compacted)
		result[index].Content = compacted
	}
	if total <= limit {
		return result
	}
	// If a single recent evidence block is exceptionally large, trim the oldest
	// remaining content while preserving tool-call structure and recent turns.
	for index := protected; index < len(result) && total > limit; index++ {
		content := result[index].Content
		if len(content) == 0 {
			continue
		}
		remaining := max(256, len(content)-(total-limit))
		if remaining >= len(content) {
			continue
		}
		compacted := compactPiMessageContent(content, remaining)
		total -= len(content) - len(compacted)
		result[index].Content = compacted
	}
	if total > limit {
		keepRecent := min(4, len(result))
		for index := 0; index < len(result)-keepRecent && total > limit; index++ {
			content := result[index].Content
			if len(content) == 0 {
				continue
			}
			compacted := compactPiMessageContent(content, 64)
			total -= len(content) - len(compacted)
			result[index].Content = compacted
		}
	}
	return result
}

func piMessageChars(messages []PiMessage) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content)
		for _, call := range message.ToolCalls {
			total += len(call.Name) + len(call.ID) + len(call.Arguments)
		}
	}
	return total
}

func compactPiMessageContent(content string, limit int) string {
	if limit <= 0 || len(content) <= limit {
		return content
	}
	head := limit * 2 / 3
	tail := limit - head
	return content[:head] + "\n[… earlier tool output compacted …]\n" + content[len(content)-tail:]
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
