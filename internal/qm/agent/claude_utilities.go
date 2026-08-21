package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (a *ClaudeAdapter) OneShot(ctx context.Context, input UtilityInput) (string, error) {
	return a.claudeUtilityCall(ctx, input, utilityModel(input.Model, a.harness.DefaultModel))
}

func (a *ClaudeAdapter) Judge(ctx context.Context, input UtilityInput) (string, error) {
	model := utilityModel(a.harness.Runtime.JudgeModel, utilityModel(input.Model, a.harness.DefaultModel))
	return a.claudeUtilityCall(ctx, input, model)
}

func (a *ClaudeAdapter) claudeUtilityCall(ctx context.Context, input UtilityInput, model string) (string, error) {
	sequence := 0
	result, err := a.RunTurn(ctx, TurnInput{
		SessionID: fmt.Sprintf("oneshot-%d", time.Now().UnixNano()), Input: input.Prompt, SystemPrompt: input.SystemPrompt,
		ScopeLabel: firstNonEmpty(input.ScopeLabel, "org:oneshot"), OrgScopeID: "org:oneshot", Harness: "claude", Model: model, ReadOnly: true,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			sequence++
			return SessionEntry{SessionID: "oneshot", Sequence: sequence, Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel}, nil
		},
		RecordModelCall: input.RecordModelCall, RecordLLMRequest: input.RecordLLMRequest,
	})
	return result.Reply, err
}

func (a *ClaudeAdapter) ShouldRespond(ctx context.Context, input DetectInput) DetectResult {
	model := utilityModel(a.harness.Runtime.JudgeModel, utilityModel(input.Model, a.harness.DefaultModel))
	out, err := a.claudeUtilityCall(ctx, UtilityInput{Model: model, ScopeLabel: input.ScopeLabel, SystemPrompt: piDetectionPrompt(strings.TrimSpace(input.ReactionGuidance) != ""), Prompt: renderPiDetectionInput(input), RecordModelCall: input.RecordModelCall, RecordLLMRequest: input.RecordLLMRequest}, model)
	if err != nil {
		return DetectResult{Respond: false}
	}
	return parsePiDetectVerdict(out, strings.TrimSpace(input.ReactionGuidance) != "")
}

func (a *ClaudeAdapter) CompactHistory(ctx context.Context, input CompactInput) string {
	transcript := piHistoryTranscript(input.History)
	if transcript == "" {
		return ""
	}
	out, err := a.claudeUtilityCall(ctx, UtilityInput{Model: utilityModel(input.Model, a.harness.DefaultModel), ScopeLabel: input.ScopeLabel, SystemPrompt: piCompactionPrompt, Prompt: transcript, RecordModelCall: input.RecordModelCall, RecordLLMRequest: input.RecordLLMRequest}, utilityModel(input.Model, a.harness.DefaultModel))
	if err != nil || out == "" {
		return deterministicPiCompact(input.History)
	}
	return out
}

func (a *ClaudeAdapter) ContextTokenBudget(model string) (int, bool) {
	definition, ok := a.models.Model(utilityModel(model, a.harness.DefaultModel))
	if !ok || definition.ContextWindow <= 0 || definition.MaxTokens <= 0 || definition.MaxTokens >= definition.ContextWindow {
		return 0, false
	}
	return (definition.ContextWindow - definition.MaxTokens) / 2, true
}

func (a *ClaudeAdapter) ScreenSecurity(ctx context.Context, input UtilityInput) (*SecurityVerdict, error) {
	input.SystemPrompt = piSecurityScreenPrompt
	out, err := a.OneShot(ctx, input)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, err
	}
	return parsePiSecurityVerdict(out), nil
}

func (*ClaudeAdapter) PickAckEmoji(context.Context, UtilityInput, []string) (string, error) {
	return "", nil
}

func (a *ClaudeAdapter) GenerateTitle(ctx context.Context, input UtilityInput) (string, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return "", nil
	}
	input.SystemPrompt = piTitlePrompt
	input.Prompt = truncateRunes(input.Prompt, 4000)
	out, err := a.OneShot(ctx, input)
	return sanitizePiTitle(out), err
}

func (a *ClaudeAdapter) SummarizeApproval(ctx context.Context, input UtilityInput, command, reason, purpose string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil
	}
	input.SystemPrompt = "Explain this command in one plain-English sentence for an approver."
	input.Prompt = strings.Join(nonEmpty(command, reason, purpose), "\n")
	out, err := a.OneShot(ctx, input)
	return truncateRunes(strings.Trim(strings.TrimSpace(out), "\"'"), 300), err
}
