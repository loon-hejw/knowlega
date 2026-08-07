package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hejw/qm-backend/internal/auth"
	"github.com/hejw/qm-backend/internal/data"
)

// proxyArtifactShare retains Node ownership for state transitions that are
// coupled to its skill review/promotion workflow or deployment transfer
// behavior. Go owns only the ordinary, durable ACL grant branch.
func (h *HTTPServer) proxyArtifactShare(r *http.Request, raw []byte) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/share" {
		return false
	}
	var input struct {
		Type    string `json:"type"`
		ToScope string `json:"toScope"`
		Move    bool   `json:"move"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	if input.Move || input.Type == "skill" && strings.TrimSpace(input.ToScope) == "org" {
		return true
	}
	// Team scope membership is currently evaluated by the Node project/runtime
	// layer. Do not widen Go's directory model by assuming group semantics.
	return strings.HasPrefix(strings.TrimSpace(input.ToScope), "team:")
}

type artifactShareHome struct {
	ID, OwnerScopeID, CreatedBy, GrantRef string
}

type artifactShareTarget struct {
	Scope, Label string
	Candidates   []map[string]string
}

// shareArtifact mirrors Node's /v1/share default branch. It deliberately
// creates an additional grant instead of moving the artifact's home.
func (h *HTTPServer) shareArtifact(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "sharing requires an agent capability token"})
		return
	}
	var input struct {
		Type       any `json:"type"`
		ID         any `json:"id"`
		ToScope    any `json:"toScope"`
		Permission any `json:"permission"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	artifactType, typeOK := input.Type.(string)
	if !typeOK || !isArtifactShareType(artifactType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "type must be one of: file, skill, deploy, cron"})
		return
	}
	id, idOK := input.ID.(string)
	if !idOK || strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "id required"})
		return
	}
	toScope, targetOK := input.ToScope.(string)
	if !targetOK || strings.TrimSpace(toScope) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": `toScope required ("org", a scope id, or a teammate's name)`})
		return
	}
	permission := "read"
	if input.Permission != nil {
		value, ok := input.Permission.(string)
		if !ok || value != "read" && value != "write" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": `permission must be "read" or "write"`})
			return
		}
		permission = value
	}

	home, err := h.artifactHome(r.Context(), artifactType, strings.TrimSpace(id))
	if err != nil {
		h.fail(w, err)
		return
	}
	if home == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": `no ` + artifactType + ` "` + strings.TrimSpace(id) + `"`})
		return
	}
	target, status, err := h.resolveArtifactShareTarget(r.Context(), toScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	switch status {
	case "invalid":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": target.Label})
		return
	case "none":
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "recipient_not_found", "message": target.Label})
		return
	case "ambiguous":
		writeJSON(w, http.StatusConflict, map[string]any{"error": "ambiguous_recipient", "message": "more than one teammate matches", "candidates": target.Candidates})
		return
	}
	manageable, err := h.managesArtifactHome(r.Context(), home.OwnerScopeID, home.CreatedBy, identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !manageable {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": `only the ` + artifactType + `'s owner (or a member of its shared home) can share or move it`})
		return
	}
	kind, _ := splitScopeID(target.Scope)
	if kind == "channel" || kind == "group" {
		member, err := h.canAccessResourceScope(r.Context(), identity.ActorID, target.Scope)
		if err != nil {
			h.fail(w, err)
			return
		}
		if !member {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": `you can't share into ` + target.Label + ` — you're not a member of that context`})
			return
		}
	}
	grant := data.Grant{OwnerScopeID: home.OwnerScopeID, Path: home.GrantRef, GranteeScopeID: target.Scope, Permission: permission, GrantedBy: identity.ActorID}
	if err := h.acl.Put(r.Context(), grant); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: grant.GrantedBy, Action: "grant", Resource: grant.Path, ScopeLabel: grant.GranteeScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verb": "share", "type": artifactType, "id": home.ID, "target": map[string]string{"scope": target.Scope, "label": target.Label}, "permission": permission})
}

func isArtifactShareType(value string) bool {
	return value == "file" || value == "skill" || value == "deploy" || value == "cron"
}

func (h *HTTPServer) artifactHome(ctx context.Context, artifactType, id string) (*artifactShareHome, error) {
	switch artifactType {
	case "skill":
		skill, err := h.skills.Get(ctx, id)
		if err != nil || skill == nil {
			return nil, err
		}
		return &artifactShareHome{ID: skill.ID, OwnerScopeID: skill.ScopeID, CreatedBy: skill.CreatedBy, GrantRef: "skill:" + skill.ID}, nil
	case "cron":
		cron, err := h.crons.Get(ctx, id)
		if err != nil || cron == nil {
			return nil, err
		}
		var metadata struct {
			OwnerScopeID string `json:"ownerScopeId"`
			CreatedBy    string `json:"createdBy"`
		}
		if err := json.Unmarshal(cron.JSON, &metadata); err != nil {
			return nil, err
		}
		return &artifactShareHome{ID: cron.ID, OwnerScopeID: metadata.OwnerScopeID, CreatedBy: metadata.CreatedBy, GrantRef: "cron:" + cron.ID}, nil
	case "file":
		file, err := h.files.Get(ctx, id)
		if err != nil || file == nil {
			return nil, err
		}
		return &artifactShareHome{ID: file.ID, OwnerScopeID: file.OwnerScopeID, CreatedBy: file.CreatedBy, GrantRef: file.Path}, nil
	case "deploy":
		deployment, err := h.deployments.Get(ctx, id)
		if err != nil || deployment == nil {
			return nil, err
		}
		return &artifactShareHome{ID: deployment.ID, OwnerScopeID: deployment.OwnerScopeID, CreatedBy: deployment.CreatedBy, GrantRef: "deployment:" + deployment.ID}, nil
	}
	return nil, nil
}

func (h *HTTPServer) resolveArtifactShareTarget(ctx context.Context, raw string) (artifactShareTarget, string, error) {
	value := strings.TrimSpace(raw)
	if value == "org" {
		return artifactShareTarget{Scope: "org:" + h.config.QM.OrgID, Label: "everyone in the org"}, "ok", nil
	}
	if kind, _ := splitScopeID(value); kind != "" {
		if kind == "team" {
			return artifactShareTarget{Label: `invalid scope "` + value + `" — use "org" or a scope id like personal:<id> or channel:<id>`}, "invalid", nil
		}
		return artifactShareTarget{Scope: value, Label: value}, "ok", nil
	}
	members, err := h.directory.List(ctx)
	if err != nil {
		return artifactShareTarget{}, "", err
	}
	matches := resolveDeploymentRecipient(members, value)
	if len(matches) == 0 {
		return artifactShareTarget{Label: `no teammate matches "` + value + `"`}, "none", nil
	}
	if len(matches) > 1 {
		candidates := make([]map[string]string, 0, len(matches))
		for _, member := range matches {
			candidates = append(candidates, map[string]string{"id": member.PrincipalID, "label": member.DisplayName})
		}
		return artifactShareTarget{Candidates: candidates}, "ambiguous", nil
	}
	return artifactShareTarget{Scope: "personal:" + matches[0].PrincipalID, Label: matches[0].DisplayName}, "ok", nil
}
