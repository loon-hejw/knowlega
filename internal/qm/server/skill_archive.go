package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

const sharedSkillTriggerRefusal = "a skill in a shared scope can only be changed by a person, not an automated trigger"

// archiveOwnedSkill moves Node's deleteOwnedSkill durable half into Go. Skill
// review, publish, restore, and materialization still remain Node runtime
// responsibilities, but archiving is only an idempotent shared-map transition
// plus its audit event.
func (h *HTTPServer) archiveOwnedSkill(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	principalID := identity.ActorID
	if principalID == "" {
		var input struct {
			PrincipalID string `json:"principalId"`
		}
		if err := json.Unmarshal(raw, &input); err != nil || strings.TrimSpace(input.PrincipalID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
			return
		}
		principalID = input.PrincipalID
	}
	id := skillID(r.URL.Path)
	skill, err := h.skills.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if skill == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no such skill"})
		return
	}
	manageable, err := h.managesArtifactHome(r.Context(), skill.ScopeID, skill.CreatedBy, principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !manageable {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "that skill isn't yours to archive"})
		return
	}
	if identity.ActorID != "" && !identity.LiveActor && !identity.LiveAuthor && isSharedSkillScope(skill.ScopeID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": sharedSkillTriggerRefusal})
		return
	}
	archived, err := h.skills.Archive(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !archived {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no such skill"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "skill_archive", Resource: id, ScopeLabel: skill.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func isSharedSkillScope(scopeID string) bool {
	kind, _ := splitScopeID(scopeID)
	return kind == "channel" || kind == "group"
}
