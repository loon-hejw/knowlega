package agent

import (
	"context"
	"testing"
)

func TestPiSignalsKeepFollowUpsUntilTheAgentWouldStop(t *testing.T) {
	c := &piSignalController{steers: make(chan TurnSignal, 2), followUps: make(chan TurnSignal, 2)}
	c.steers <- TurnSignal{Kind: "steer", Text: "change current direction"}
	c.followUps <- TurnSignal{Kind: "followUp", Text: "next task"}
	input := TurnInput{Emit: func(context.Context, NewEntry) (SessionEntry, error) { return SessionEntry{}, nil }}
	var messages []PiMessage
	if n, err := c.applySteers(t.Context(), input, &messages); err != nil || n != 1 || len(c.followUps) != 1 {
		t.Fatalf("steers=%d err=%v", n, err)
	}
	if n, err := c.applyFollowUps(t.Context(), input, &messages); err != nil || n != 1 || messages[1].Content != "next task" {
		t.Fatalf("followups=%d messages=%+v err=%v", n, messages, err)
	}
}
