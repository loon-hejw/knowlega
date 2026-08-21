package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type AgentPendingApprovalInput struct {
	ID, SessionID, ActorID, Command, Reason, Kind, Matched, Purpose, ApprovalKey string
}

type AgentApprovalRepository struct {
	pg         *Postgres
	userConfig *UserConfigRepository
}

func NewAgentApprovalRepository(pg *Postgres) *AgentApprovalRepository {
	return &AgentApprovalRepository{pg: pg, userConfig: NewUserConfigRepository(pg)}
}

func (r *AgentApprovalRepository) PutPending(ctx context.Context, input AgentPendingApprovalInput) error {
	if input.ID == "" || input.SessionID == "" || input.Command == "" {
		return errors.New("approval id, session id, and command are required")
	}
	document := map[string]any{
		"sessionId": input.SessionID, "command": input.Command, "reason": input.Reason,
		"kind": "approval", "blocksInput": true, "createdAt": time.Now().UnixMilli(),
		"grantModes": map[string]bool{"session": true, "always": true},
		"request":    map[string]any{"actor": map[string]string{"externalId": input.ActorID}},
	}
	if input.Matched != "" {
		document["matched"] = input.Matched
	}
	if input.Purpose != "" {
		document["purpose"] = input.Purpose
	}
	if input.ApprovalKey != "" {
		document["approvalKey"] = input.ApprovalKey
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	_, err = r.pg.Pool.Exec(ctx, `INSERT INTO approvals(id,json) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET json=EXCLUDED.json`, input.ID, string(encoded))
	return err
}

func (r *AgentApprovalRepository) Resolve(ctx context.Context, id, sessionID, actorID string, approved bool, scope string) (string, string, error) {
	approval, err := NewSessionRepository(r.pg).Approval(ctx, id)
	if err != nil {
		return "", "", err
	}
	if approval == nil || approval.SessionID != sessionID || approval.ActorID != "" && approval.ActorID != actorID {
		return "", "approval request is no longer available", nil
	}
	if _, err := r.pg.Pool.Exec(ctx, "DELETE FROM approvals WHERE id=$1", id); err != nil {
		return "", "", err
	}
	if !approved {
		return "", "approval denied for " + approval.Command, nil
	}
	key := approval.ApprovalKey
	if key == "" {
		key = approval.Command
	}
	if scope == "" || scope == "once" {
		return key, "", nil
	}
	if scope != "session" && scope != "always" {
		return "", "approval scope must be once, session, or always", nil
	}
	document := map[string]any{"actorId": actorID, "command": approval.Command, "scope": scope, "createdAt": time.Now().UnixMilli(), "approvalKey": key}
	if scope == "session" {
		document["sessionId"] = sessionID
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", "", err
	}
	grantSession := "*"
	if scope == "session" {
		grantSession = sessionID
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{actorID, scope, grantSession, key}, "\x00")))
	grantID := hex.EncodeToString(sum[:])[:24]
	_, err = r.pg.Pool.Exec(ctx, "INSERT INTO approval_grants(id,json) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET json=EXCLUDED.json", grantID, string(encoded))
	return key, "", err
}

func (r *AgentApprovalRepository) ToolApprovalPolicy(ctx context.Context, orgScopeID, scopeID, sessionID, actorID string) (bool, []string, error) {
	snapshot, err := r.userConfig.Snapshot(ctx, orgScopeID, scopeID)
	if err != nil {
		return false, nil, err
	}
	if snapshot.SecurityPosture != "strict" {
		return false, nil, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT json FROM approval_grants")
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return false, nil, err
		}
		var grant struct {
			ActorID     string `json:"actorId"`
			Command     string `json:"command"`
			Scope       string `json:"scope"`
			SessionID   string `json:"sessionId"`
			ApprovalKey string `json:"approvalKey"`
		}
		if json.Unmarshal(raw, &grant) != nil || !strings.EqualFold(grant.ActorID, actorID) || grant.Scope == "session" && grant.SessionID != sessionID {
			continue
		}
		key := grant.ApprovalKey
		if key == "" {
			key = grant.Command
		}
		keys = append(keys, key)
	}
	return true, keys, rows.Err()
}
