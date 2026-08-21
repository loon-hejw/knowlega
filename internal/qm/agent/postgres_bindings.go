package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type SessionPersistence interface {
	AppendEntry(context.Context, string, data.NewSessionEntry) (data.SessionEntry, error)
	Entries(context.Context, string, int, int) ([]data.SessionEntry, error)
	RecordLLMRequest(context.Context, string, data.NewLLMRequest) (data.LLMRequest, error)
	AppendTape(context.Context, string, data.NewTapeRecord) (data.TapeRecord, error)
	Tape(context.Context, string, int) ([]data.TapeRecord, error)
	TapeCoverage(context.Context, string) (int, error)
}

type PostgresBindingsResolver struct {
	sessions     SessionPersistence
	toolResolver ToolContextResolver
	signals      RunSignalPersistence
	approvals    ApprovalPersistence
	policy       ToolApprovalPolicy
	inbound      InboundMaterializer
	preparer     TurnInputPreparer
}

type ApprovalPersistence interface {
	PutPending(context.Context, data.AgentPendingApprovalInput) error
}

type ToolApprovalPolicy interface {
	ToolApprovalPolicy(context.Context, string, string, string, string) (bool, []string, error)
}

type ToolContextResolver interface {
	ResolveToolContext(context.Context, TurnTaskPayload) (ToolContext, error)
}

type RunSignalPersistence interface {
	TakePendingSignals(context.Context, string) ([]data.RunSignal, error)
}

func NewPostgresBindingsResolver(sessions SessionPersistence, toolResolver ToolContextResolver, signals ...RunSignalPersistence) *PostgresBindingsResolver {
	resolver := &PostgresBindingsResolver{sessions: sessions, toolResolver: toolResolver}
	if len(signals) == 1 {
		resolver.signals = signals[0]
	}
	return resolver
}

func (r *PostgresBindingsResolver) WithApprovals(approvals ApprovalPersistence, policy ToolApprovalPolicy) *PostgresBindingsResolver {
	r.approvals = approvals
	r.policy = policy
	return r
}

func (r *PostgresBindingsResolver) WithInboundMaterializer(inbound InboundMaterializer) *PostgresBindingsResolver {
	r.inbound = inbound
	return r
}

func (r *PostgresBindingsResolver) WithTurnInputPreparer(preparer TurnInputPreparer) *PostgresBindingsResolver {
	r.preparer = preparer
	return r
}

func (r *PostgresBindingsResolver) ResolveTurnBindings(ctx context.Context, payload TurnTaskPayload) (TurnBindings, error) {
	if r == nil || r.sessions == nil {
		return TurnBindings{}, errors.New("session persistence is not configured")
	}
	var tools ToolContext
	if r.toolResolver != nil {
		var err error
		tools, err = r.toolResolver.ResolveToolContext(ctx, payload)
		if err != nil {
			return TurnBindings{}, err
		}
	}
	var toolGate func(string) bool
	if r.policy != nil {
		strict, durable, policyErr := r.policy.ToolApprovalPolicy(ctx, payload.OrgScopeID, payload.ScopeLabel, payload.SessionID, payload.ActorID)
		if policyErr != nil {
			return TurnBindings{}, policyErr
		}
		if strict {
			uses := map[string]int{}
			for _, key := range payload.ApprovedToolKeys {
				uses[key]++
			}
			for _, key := range durable {
				uses[key] = -1
			}
			toolGate = func(tool string) bool {
				key := "tool:" + tool
				remaining, ok := uses[key]
				if !ok || remaining == 0 {
					return false
				}
				if remaining > 0 {
					uses[key]--
				}
				return true
			}
		}
	}
	storedTape, err := r.sessions.Tape(ctx, payload.SessionID, -1)
	if err != nil {
		return TurnBindings{}, err
	}
	priorMax, historyEntries := tapeHistoryMaximum(payload.History)
	coverage, err := r.sessions.TapeCoverage(ctx, payload.SessionID)
	if err != nil {
		return TurnBindings{}, err
	}
	tapeCovered := priorMax < 0 || coverage >= priorMax
	if !tapeCovered && payload.TapeMode == "serve" && sameTapeHarness(storedTape, "pi") {
		messages := piMessagesFromHistory(payload.History)
		if len(messages) > 0 {
			tapeMessages := make([]json.RawMessage, 0, len(messages))
			for _, message := range messages {
				tapeMessages = append(tapeMessages, piTapeMessage(message))
			}
			scopes := []string{}
			seenScopes := map[string]bool{}
			for _, entry := range historyEntries {
				if entry.ScopeLabel != "" && !seenScopes[entry.ScopeLabel] {
					seenScopes[entry.ScopeLabel] = true
					scopes = append(scopes, entry.ScopeLabel)
				}
			}
			encoded, encodeErr := json.Marshal(map[string]any{"event": "legacy_import", "messages": tapeMessages, "scopes": scopes})
			if encodeErr != nil {
				return TurnBindings{}, encodeErr
			}
			imported, appendErr := r.sessions.AppendTape(ctx, payload.SessionID, data.NewTapeRecord{
				Kind: "context_event", ScopeLabel: payload.ScopeLabel, Payload: encoded, CoversEntrySeq: &priorMax,
			})
			if appendErr != nil {
				return TurnBindings{}, appendErr
			}
			storedTape = append(storedTape, imported)
			coverage = priorMax
			tapeCovered = true
		}
	}
	tapeRows := make([]TapeRecord, 0, len(storedTape))
	for _, record := range storedTape {
		tapeRows = append(tapeRows, TapeRecord{
			Kind: record.Kind, Harness: record.Harness, ScopeLabel: record.ScopeLabel,
			Payload: record.Payload, BareText: stringValue(record.BareText), Timestamp: stringValue(record.TS),
			EntrySequence: record.EntrySequence, CoversEntrySequence: record.CoversEntrySeq,
			Sequence: record.Sequence, CreatedAt: record.CreatedAt,
		})
	}
	return TurnBindings{
		Tools:            tools,
		ToolApprovalGate: toolGate,
		Emit: func(ctx context.Context, entry NewEntry) (SessionEntry, error) {
			stored, err := r.sessions.AppendEntry(ctx, payload.SessionID, data.NewSessionEntry{Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel})
			if err != nil {
				return SessionEntry{}, err
			}
			return SessionEntry{
				SessionID: stored.SessionID, Sequence: stored.Sequence, ParentSequence: stored.ParentSequence,
				Type: stored.Type, Payload: stored.Payload, ScopeLabel: stored.ScopeLabel, CreatedAt: stored.CreatedAt,
			}, nil
		},
		FindToolResult: func(ctx context.Context, callID string) (ToolResult, bool, error) {
			entries, err := r.sessions.Entries(ctx, payload.SessionID, 500, 0)
			if err != nil {
				return ToolResult{}, false, err
			}
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Type != "tool_result" {
					continue
				}
				var stored struct {
					CallID  string          `json:"callId"`
					Result  string          `json:"result"`
					IsError bool            `json:"isError"`
					Details json.RawMessage `json:"details"`
				}
				if json.Unmarshal(entries[i].Payload, &stored) == nil && stored.CallID == callID {
					return ToolResult{Content: []ToolContent{{Type: "text", Text: stored.Result}}, Details: stored.Details, IsError: stored.IsError}, true, nil
				}
			}
			return ToolResult{}, false, nil
		},
		RecordLLMRequest: func(ctx context.Context, request LLMRequestRecord) error {
			_, err := r.sessions.RecordLLMRequest(ctx, payload.SessionID, data.NewLLMRequest{
				TurnSeq: request.TurnSequence, Step: request.Step, Model: request.Model, ScopeLabel: payload.ScopeLabel,
				Request: request.Request, Truncated: request.Truncated, TTFTMS: request.TTFTMS, DurationMS: request.DurationMS,
				StepGapMS: request.StepGapMS, ToolWallMS: request.ToolWallMS, GapPhases: request.GapPhases,
				Usage: request.Usage, Transport: request.Transport,
			})
			return err
		},
		RecordModelCall: func(ModelCallRecord) {},
		TakePendingSignals: func(ctx context.Context, runID string) ([]TurnSignal, error) {
			if r.signals == nil || runID == "" {
				return nil, nil
			}
			stored, err := r.signals.TakePendingSignals(ctx, runID)
			if err != nil {
				return nil, err
			}
			result := make([]TurnSignal, 0, len(stored))
			for _, signal := range stored {
				result = append(result, TurnSignal{Kind: signal.Kind, Text: signal.Text, CreatedAt: signal.CreatedAt, Payload: append(json.RawMessage(nil), signal.Payload...)})
			}
			return result, nil
		},
		Tape: func(ctx context.Context, record TapeRecord) error {
			var bareText, timestamp *string
			if record.BareText != "" {
				bareText = &record.BareText
			}
			if record.Timestamp != "" {
				timestamp = &record.Timestamp
			}
			_, err := r.sessions.AppendTape(ctx, payload.SessionID, data.NewTapeRecord{
				Kind: record.Kind, Harness: record.Harness, ScopeLabel: record.ScopeLabel, Payload: record.Payload,
				BareText: bareText, TS: timestamp, EntrySequence: record.EntrySequence, CoversEntrySeq: record.CoversEntrySequence,
			})
			return err
		},
		TapeRows: tapeRows,
		TapeMode: func() string {
			if payload.TapeMode == "serve" && tapeCovered {
				return "serve"
			}
			return "shadow"
		}(),
		TapeCovered: tapeCovered,
		PrepareInput: func(ctx context.Context, input *TurnInput) (*TurnResult, error) {
			if input == nil {
				return nil, nil
			}
			if r.inbound != nil && hasJSONArrayItems(payload.Attachments) {
				if payload.ReadOnly {
					input.Environment = joinEnvironment(input.Environment, readOnlyAttachmentEnvironment(payload.Attachments))
					input.Attachments = nil
				} else {
					materialized, err := r.inbound.Materialize(ctx, firstNonEmpty(payload.WorkspaceKey, payload.ScopeLabel), payload.Attachments)
					if err != nil {
						return nil, err
					}
					input.Attachments = materialized.Attachments
					input.Images = append(input.Images, materialized.Images...)
					input.Environment = joinEnvironment(input.Environment, materialized.Environment)
				}
			}
			if r.preparer != nil {
				return r.preparer.PrepareTurnInput(ctx, input)
			}
			return nil, nil
		},
		PersistApprovals: func(ctx context.Context, pending []PendingApproval) error {
			if r.approvals == nil {
				return nil
			}
			for _, approval := range pending {
				if err := r.approvals.PutPending(ctx, data.AgentPendingApprovalInput{ID: approval.RequestID, SessionID: payload.SessionID, ActorID: payload.ActorID, Command: approval.Command, Reason: approval.Reason, Kind: approval.Kind, Matched: approval.Matched, Purpose: approval.Purpose, ApprovalKey: approval.ApprovalKey}); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

func tapeHistoryMaximum(raw json.RawMessage) (int, []SessionEntry) {
	var entries []SessionEntry
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return -1, nil
	}
	maximum := -1
	for _, entry := range entries {
		if entry.Sequence > maximum {
			maximum = entry.Sequence
		}
	}
	return maximum, entries
}

func sameTapeHarness(rows []data.TapeRecord, harness string) bool {
	for _, row := range rows {
		if row.Kind == "message" && row.Harness != "" && row.Harness != harness {
			return false
		}
	}
	return true
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
