package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a *OpenCodeAdapter) OneShot(ctx context.Context, input UtilityInput) (string, error) {
	return a.openCodeUtilityCall(ctx, input, utilityModel(input.Model, a.harness.DefaultModel))
}

func (a *OpenCodeAdapter) Judge(ctx context.Context, input UtilityInput) (string, error) {
	return a.openCodeUtilityCall(ctx, input, utilityModel(input.Model, a.harness.DefaultModel))
}

func (a *OpenCodeAdapter) openCodeUtilityCall(ctx context.Context, input UtilityInput, model string) (string, error) {
	sequence := 0
	result, err := a.RunTurn(ctx, TurnInput{
		SessionID: fmt.Sprintf("oneshot-%d", time.Now().UnixNano()), Input: input.Prompt, SystemPrompt: input.SystemPrompt,
		ScopeLabel: firstNonEmpty(input.ScopeLabel, "org:oneshot"), OrgScopeID: "org:oneshot", Harness: "opencode", Model: model, ReadOnly: true,
		Emit: func(_ context.Context, entry NewEntry) (SessionEntry, error) {
			sequence++
			return SessionEntry{SessionID: "oneshot", Sequence: sequence, Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel}, nil
		},
		RecordModelCall: input.RecordModelCall, RecordLLMRequest: input.RecordLLMRequest,
	})
	return result.Reply, err
}

func (a *OpenCodeAdapter) ShouldRespond(context.Context, DetectInput) DetectResult {
	return DetectResult{Respond: false, Reason: "opencode does not expose shouldRespond"}
}

func (a *OpenCodeAdapter) CompactHistory(_ context.Context, input CompactInput) string {
	return deterministicPiCompact(input.History)
}

func (a *OpenCodeAdapter) ContextTokenBudget(model string) (int, bool) {
	definition, ok := a.models.Model(utilityModel(model, a.harness.DefaultModel))
	if !ok || definition.ContextWindow <= 0 || definition.MaxTokens <= 0 || definition.MaxTokens >= definition.ContextWindow {
		return 0, false
	}
	return (definition.ContextWindow - definition.MaxTokens) / 2, true
}

func (a *OpenCodeAdapter) ScreenSecurity(ctx context.Context, input UtilityInput) (*SecurityVerdict, error) {
	input.SystemPrompt = piSecurityScreenPrompt
	out, err := a.OneShot(ctx, input)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, err
	}
	return parsePiSecurityVerdict(out), nil
}

func (*OpenCodeAdapter) PickAckEmoji(context.Context, UtilityInput, []string) (string, error) {
	return "", nil
}

func (a *OpenCodeAdapter) GenerateTitle(ctx context.Context, input UtilityInput) (string, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return "", nil
	}
	input.SystemPrompt = piTitlePrompt
	input.Prompt = truncateRunes(input.Prompt, 4000)
	out, err := a.OneShot(ctx, input)
	return sanitizePiTitle(out), err
}

func (a *OpenCodeAdapter) SummarizeApproval(ctx context.Context, input UtilityInput, command, reason, purpose string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil
	}
	input.SystemPrompt = "Explain this command in one plain-English sentence for an approver."
	input.Prompt = strings.Join(nonEmpty(command, reason, purpose), "\n")
	out, err := a.OneShot(ctx, input)
	return truncateRunes(strings.Trim(strings.TrimSpace(out), "\"'"), 300), err
}

func openCodeMessagesJSON(messages []OpenCodeMessage) json.RawMessage {
	encoded, _ := json.Marshal(messages)
	return encoded
}
