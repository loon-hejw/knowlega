package agent

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// AgentTaskStore is the durable, user-visible subagent task projection shared
// with the original Node harnesses. Provider child threads still execute inside
// their native harness; this store records their lifecycle for recovery and UI.
type AgentTaskStore interface {
	CreateTask(ctx context.Context, id, sessionID, originRunID, title, status string) error
	TransitionTask(ctx context.Context, id, expected, next, runID string) (bool, error)
}

func transitionAgentTask(ctx context.Context, store AgentTaskStore, id, expected, next, runID string) error {
	if store == nil || expected == next {
		return nil
	}
	updated, err := store.TransitionTask(ctx, id, expected, next, runID)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("task " + id + " was not " + expected + " while transitioning to " + next)
	}
	return nil
}

var codexNamedSubagentPattern = regexp.MustCompile(`(?i)\bYou are (?:the )?([^.!?]{1,80}? subagent)\b`)

func codexTaskTitle(value any) string {
	prompt, ok := value.(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return "subagent task"
	}
	normalized := strings.Join(strings.Fields(prompt), " ")
	title := normalized
	if match := codexNamedSubagentPattern.FindStringSubmatch(normalized); len(match) == 2 {
		title = match[1]
	}
	if utf8.RuneCountInString(title) <= 120 {
		return title
	}
	runes := []rune(title)
	return strings.TrimSpace(string(runes[:119])) + "…"
}

func agentTaskOrigin(input TurnInput) string {
	if strings.TrimSpace(input.RunID) != "" {
		return input.RunID
	}
	return input.SessionID
}
