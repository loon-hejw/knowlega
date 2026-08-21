package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	qmagent "github.com/loon-hejw/knowlega/internal/qm/agent"
	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type agentTurnRequest struct {
	Surface string `json:"surface"`
	Actor   struct {
		ExternalID  string `json:"externalId"`
		DisplayName string `json:"displayName"`
	} `json:"actor"`
	Conversation struct {
		Kind        string `json:"kind"`
		ThreadRef   string `json:"threadRef"`
		ChannelRef  string `json:"channelRef"`
		ChannelName string `json:"channelName"`
	} `json:"conversation"`
	Text            string          `json:"text"`
	DisplayText     string          `json:"displayText"`
	PriorTurns      json.RawMessage `json:"priorTurns"`
	Attachments     json.RawMessage `json:"attachments"`
	Model           string          `json:"model"`
	Harness         string          `json:"harness"`
	ThinkingLevel   string          `json:"thinkingLevel"`
	FastMode        bool            `json:"fastMode"`
	ReadOnly        bool            `json:"readOnly"`
	SurfaceTools    bool            `json:"surfaceTools"`
	TurnWallClockMS int             `json:"turnWallClockMs"`
	IdempotencyKey  string          `json:"idempotencyKey"`
	ProactiveOpener bool            `json:"proactiveOpener"`
	Approval        json.RawMessage `json:"approval"`
}

const proactiveOpenerPrompt = "The user just opened the app for the first time and hasn't typed anything yet. You already know who they are from their sign-in (see \"Who you're talking to\") — open the conversation yourself: greet them by name as their AI teammate, briefly say what you can do, and start onboarding by walking them through connecting their accounts. Don't ask their name or role, and don't research them in this opening turn — the hello is just a hello; you'll learn their role from connected tools and the people directory later, once their accounts are connecting."

func (h *HTTPServer) postAgentTurn(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var request agentTurnRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expected a TurnRequest"})
		return
	}
	request.Text = strings.TrimSpace(request.Text)
	request.Actor.ExternalID = strings.TrimSpace(request.Actor.ExternalID)
	request.Conversation.ThreadRef = strings.TrimSpace(request.Conversation.ThreadRef)
	if request.Actor.ExternalID == "" || request.Conversation.ThreadRef == "" || request.Text == "" && len(request.Attachments) == 0 && !request.ProactiveOpener {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expected a TurnRequest"})
		return
	}
	scopeID, sessionType, err := agentTurnScope(request)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
		return
	}
	if strings.HasPrefix(scopeID, "group:web-project-") {
		actorID := request.Actor.ExternalID
		if identity.ActorID != "" {
			if identity.ActorID != actorID {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden_actor"})
				return
			}
			actorID = identity.ActorID
		}
		allowed, membershipErr := validateProjectTurnMembership(r.Context(), scopeID, actorID, h.projectRepo.HasScopeMembership)
		if membershipErr != nil {
			h.fail(w, membershipErr)
			return
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden_scope"})
			return
		}
	}
	session, err := h.sessions.GetOrCreateByThread(r.Context(), request.Conversation.ThreadRef, sessionType, scopeID, request.Conversation.ChannelName, request.Surface)
	if err != nil {
		if writeAgentTurnSessionError(w, err) {
			return
		}
		h.fail(w, err)
		return
	}
	if err := h.sessions.AddParticipant(r.Context(), session.ID, request.Actor.ExternalID); err != nil {
		h.fail(w, err)
		return
	}
	approvedToolKeys := []string{}
	if len(request.Approval) > 0 && string(request.Approval) != "null" {
		var decision struct {
			RequestID string `json:"requestId"`
			Approved  bool   `json:"approved"`
			Scope     string `json:"scope"`
		}
		if json.Unmarshal(request.Approval, &decision) != nil || strings.TrimSpace(decision.RequestID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid approval decision"})
			return
		}
		key, refusal, resolveErr := h.agentApprovals.Resolve(r.Context(), decision.RequestID, session.ID, request.Actor.ExternalID, decision.Approved, decision.Scope)
		if resolveErr != nil {
			h.fail(w, resolveErr)
			return
		}
		if refusal != "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"status": "refused", "sessionId": session.ID, "reason": refusal})
			return
		}
		if key != "" {
			approvedToolKeys = append(approvedToolKeys, key)
		}
	}
	turnInput := request.Text
	if request.ProactiveOpener && turnInput == "" {
		turnInput = proactiveOpenerPrompt
	} else if turnInput == "" && hasAgentAttachments(request.Attachments) {
		turnInput = "The user shared attachment(s) without accompanying text. Inspect the files listed in the turn environment and respond to their contents."
	}
	history, err := h.agentTurnHistory(r, session.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	systemPrompt, err := h.agentTurnSystemPrompt(r, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	runID := uuid.NewString()
	storedRunID, deduped, err := h.runs.EnqueueWithID(r.Context(), runID, session.ID, raw, request.IdempotencyKey, 3)
	if err != nil {
		h.fail(w, err)
		return
	}
	if deduped {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "runId": storedRunID})
		return
	}
	userPayload := json.RawMessage(nil)
	if hasAgentAttachments(request.Attachments) {
		userPayload, err = json.Marshal(map[string]any{"text": turnInput, "attachments": request.Attachments})
	} else {
		userPayload, err = json.Marshal(map[string]string{"text": turnInput})
	}
	if err != nil {
		_ = h.runs.FailRuntime(r.Context(), storedRunID, err.Error(), true)
		h.fail(w, err)
		return
	}
	userEntry, err := h.sessions.AppendEntry(r.Context(), session.ID, data.NewSessionEntry{Type: "user", Payload: userPayload, ScopeLabel: scopeID})
	if err != nil {
		_ = h.runs.FailRuntime(r.Context(), storedRunID, err.Error(), true)
		h.fail(w, err)
		return
	}
	payload := qmagent.TurnTaskPayload{
		SessionID: session.ID, RunID: storedRunID, Input: turnInput, SystemPrompt: systemPrompt,
		ScopeLabel: scopeID, OrgScopeID: "org:" + h.config.QM.OrgID, WorkspaceKey: scopeID,
		Model: request.Model, Harness: request.Harness, ThinkingLevel: request.ThinkingLevel,
		FastMode: request.FastMode, ReadOnly: request.ReadOnly, ActorID: request.Actor.ExternalID,
		ApprovedToolKeys: approvedToolKeys, SurfaceTools: request.SurfaceTools,
		SurfaceName: request.Surface, TurnWallClockMS: request.TurnWallClockMS, History: history,
		PriorTurns: request.PriorTurns, Attachments: request.Attachments, UserEntrySequence: &userEntry.Sequence, TapeMode: "serve",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		_ = h.runs.FailRuntime(r.Context(), storedRunID, err.Error(), true)
		h.fail(w, err)
		return
	}
	_, _, err = h.runtimeTasks.Enqueue(r.Context(), data.EnqueueRuntimeTaskInput{
		ID: storedRunID, Kind: qmagent.TurnTaskKind, PayloadVersion: 1, Payload: encoded,
		ScopeID: scopeID, ActorID: request.Actor.ExternalID, IdempotencyKey: "run:" + storedRunID,
		SerialKey: "session:" + session.ID, MaxAttempts: 3,
	})
	if err != nil {
		_ = h.runs.FailRuntime(r.Context(), storedRunID, err.Error(), true)
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "runId": storedRunID})
}

func writeAgentTurnSessionError(w http.ResponseWriter, err error) bool {
	var mismatch *data.SessionScopeMismatchError
	if !errors.As(err, &mismatch) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": "scope_mismatch", "message": mismatch.Error(),
		"expectedScopeId": mismatch.ExpectedScopeID, "receivedScopeId": mismatch.ReceivedScopeID,
		"expectedType": mismatch.ExpectedType, "receivedType": mismatch.ReceivedType,
	})
	return true
}

func validateProjectTurnMembership(ctx context.Context, scopeID, actorID string, hasMembership func(context.Context, string, string) (bool, error)) (bool, error) {
	if !strings.HasPrefix(scopeID, "group:web-project-") {
		return true, nil
	}
	if actorID == "" || hasMembership == nil {
		return false, nil
	}
	return hasMembership(ctx, scopeID, actorID)
}

func hasAgentAttachments(raw json.RawMessage) bool {
	var values []json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &values) == nil && len(values) > 0
}

func (h *HTTPServer) getAgentRun(w http.ResponseWriter, r *http.Request) {
	id := runRecordID(r.URL.Path)
	run, err := h.runs.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	response := map[string]any{
		"status": run.Status, "startedAt": run.StartedAt, "finishedAt": run.FinishedAt,
	}
	if len(run.Result) > 0 && string(run.Result) != "null" {
		var result any
		if json.Unmarshal(run.Result, &result) == nil {
			response["result"] = result
		}
	}
	after := int64(-1)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < -1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_after"})
			return
		}
		after = parsed
	}
	events, err := h.runtimeTasks.ListEventsAfter(r.Context(), id, after)
	if err != nil {
		h.fail(w, err)
		return
	}
	var partial strings.Builder
	currentAttempt := 0
	partialReset := false
	activity := []map[string]any{}
	for _, event := range events {
		if event.Type == "text_block_start" {
			var block struct {
				Attempt int `json:"attempt"`
			}
			if json.Unmarshal(event.Payload, &block) == nil && block.Attempt > currentAttempt {
				currentAttempt = block.Attempt
			}
			partial.Reset()
			if after >= 0 {
				partialReset = true
			}
			continue
		}
		if event.Type == "delta" {
			var delta struct {
				Attempt int    `json:"attempt"`
				Chunk   string `json:"chunk"`
			}
			if json.Unmarshal(event.Payload, &delta) == nil {
				if delta.Attempt == 0 {
					delta.Attempt = 1
				}
				if delta.Attempt > currentAttempt {
					currentAttempt = delta.Attempt
					if after < 0 {
						partial.Reset()
					}
				}
				if after >= 0 || delta.Attempt == currentAttempt {
					partial.WriteString(delta.Chunk)
				}
			}
			continue
		}
		var envelope struct {
			Attempt int `json:"attempt"`
		}
		if json.Unmarshal(event.Payload, &envelope) == nil && envelope.Attempt > currentAttempt {
			if after < 0 {
				partial.Reset()
			}
			currentAttempt = envelope.Attempt
		}
		activity = append(activity, map[string]any{"seq": event.Sequence, "parentSeq": nil, "type": event.Type, "payload": json.RawMessage(event.Payload), "createdAt": event.CreatedAt})
	}
	if len(events) > 0 {
		response["eventCursor"] = events[len(events)-1].Sequence
	}
	if currentAttempt > 0 {
		response["attempt"] = currentAttempt
	}
	if partial.Len() > 0 {
		response["partial"] = partial.String()
		if after >= 0 {
			if partialReset {
				response["partialMode"] = "replace"
			} else {
				response["partialMode"] = "append"
			}
		}
	}
	if partialReset {
		response["partialReset"] = true
	}
	if len(activity) > 0 {
		response["activity"] = activity
	}
	if run.Status == "running" {
		task, taskErr := h.runtimeTasks.Get(r.Context(), id)
		if taskErr != nil {
			h.fail(w, taskErr)
			return
		}
		alive := task != nil && task.Status == "running" && task.LeaseExpiresAt != nil && *task.LeaseExpiresAt > time.Now().UnixMilli()
		response["alive"] = alive
		if !alive {
			response["stale"] = true
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func agentTurnScope(request agentTurnRequest) (string, string, error) {
	switch request.Conversation.Kind {
	case "dm":
		return "personal:" + request.Actor.ExternalID, "dm", nil
	case "channel":
		if strings.TrimSpace(request.Conversation.ChannelRef) == "" {
			return "", "", errors.New("channelRef required for a channel turn")
		}
		return "channel:" + strings.TrimSpace(request.Conversation.ChannelRef), "channel", nil
	case "group":
		if strings.TrimSpace(request.Conversation.ChannelRef) == "" {
			return "", "", errors.New("channelRef required for a group turn")
		}
		return "group:" + strings.TrimSpace(request.Conversation.ChannelRef), "group", nil
	default:
		return "", "", errors.New("conversation.kind must be dm, channel, or group")
	}
}

func (h *HTTPServer) agentTurnHistory(r *http.Request, sessionID string) (json.RawMessage, error) {
	entries, err := h.sessions.Entries(r.Context(), sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	result := make([]qmagent.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, qmagent.SessionEntry{
			SessionID: entry.SessionID, Sequence: entry.Sequence, ParentSequence: entry.ParentSequence,
			Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel, CreatedAt: entry.CreatedAt,
		})
	}
	return json.Marshal(result)
}

func (h *HTTPServer) agentTurnSystemPrompt(r *http.Request, scopeID string) (string, error) {
	sections := []string{
		"You are QM, a persistent assistant. Follow the user's language and work as a capable colleague.",
		"Use tools for durable workspace files, memory, conversation history, and the LLM-maintained Knowledge wiki. Never claim a tool action succeeded unless its result says so. Knowledge search is candidate recall; read cited documents before treating snippets as evidence.",
	}
	for _, id := range []string{"org:" + h.config.QM.OrgID, scopeID} {
		soul, err := h.souls.Get(r.Context(), id)
		if err != nil {
			return "", err
		}
		if soul != nil && strings.TrimSpace(soul.Content) != "" {
			sections = append(sections, fmt.Sprintf("Instructions for %s:\n%s", id, soul.Content))
		}
	}
	head, err := h.memory.Head(r.Context(), scopeID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(head.Content) != "" {
		content := head.Content
		if len(content) > 6000 {
			content = content[len(content)-6000:]
		}
		sections = append(sections, "What you remember:\n"+content)
	}
	return strings.Join(sections, "\n\n"), nil
}
