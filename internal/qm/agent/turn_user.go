package agent

import (
	"context"
	"encoding/json"
)

func turnUserEntry(ctx context.Context, input TurnInput) (SessionEntry, error) {
	if input.UserEntrySequence != nil {
		return SessionEntry{SessionID: input.SessionID, Sequence: *input.UserEntrySequence, Type: "user", ScopeLabel: input.ScopeLabel}, nil
	}
	payload, _ := json.Marshal(map[string]any{"text": input.Input, "attachments": rawJSONArray(input.Attachments)})
	if len(input.Attachments) == 0 || string(input.Attachments) == "null" || string(input.Attachments) == "[]" {
		payload, _ = json.Marshal(map[string]string{"text": input.Input})
	}
	return input.Emit(ctx, NewEntry{Type: "user", Payload: payload, ScopeLabel: input.ScopeLabel})
}
