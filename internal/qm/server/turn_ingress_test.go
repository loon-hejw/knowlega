package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestAgentTurnScopeMatchesNodeConversationKinds(t *testing.T) {
	request := agentTurnRequest{}
	request.Actor.ExternalID = "alice"
	request.Conversation.Kind = "dm"
	if scope, kind, err := agentTurnScope(request); err != nil || scope != "personal:alice" || kind != "dm" {
		t.Fatalf("scope=%q kind=%q err=%v", scope, kind, err)
	}
	request.Conversation.Kind = "channel"
	request.Conversation.ChannelRef = "C1"
	if scope, kind, err := agentTurnScope(request); err != nil || scope != "channel:C1" || kind != "channel" {
		t.Fatalf("scope=%q kind=%q err=%v", scope, kind, err)
	}
	request.Conversation.Kind = "group"
	request.Conversation.ChannelRef = "project:42"
	if scope, kind, err := agentTurnScope(request); err != nil || scope != "group:project:42" || kind != "group" {
		t.Fatalf("scope=%q kind=%q err=%v", scope, kind, err)
	}
}

func TestValidateProjectTurnMembershipDoesNotTrustClientScope(t *testing.T) {
	called := false
	allowed, err := validateProjectTurnMembership(t.Context(), "group:web-project-secret", "attacker", func(_ context.Context, scopeID, actorID string) (bool, error) {
		called = true
		if scopeID != "group:web-project-secret" || actorID != "attacker" {
			t.Fatalf("scope=%q actor=%q", scopeID, actorID)
		}
		return false, nil
	})
	if err != nil || allowed || !called {
		t.Fatalf("allowed=%v called=%v err=%v", allowed, called, err)
	}
}

func TestWriteAgentTurnSessionErrorReturnsScopeMismatchConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	handled := writeAgentTurnSessionError(recorder, &data.SessionScopeMismatchError{
		ThreadRef: "web:alice:thread", ExpectedType: "group", ReceivedType: "group",
		ExpectedScopeID: "group:web-project-one", ReceivedScopeID: "group:web-project-two",
	})
	if !handled || recorder.Code != http.StatusConflict {
		t.Fatalf("handled=%v status=%d body=%s", handled, recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "scope_mismatch" || body["expectedScopeId"] != "group:web-project-one" || body["receivedScopeId"] != "group:web-project-two" {
		t.Fatalf("body=%v", body)
	}
}

func TestAgentTurnAttachmentAndProactiveDetection(t *testing.T) {
	if hasAgentAttachments(nil) || hasAgentAttachments(json.RawMessage(`[]`)) || !hasAgentAttachments(json.RawMessage(`[{"blobId":"abc"}]`)) {
		t.Fatal("attachment detection mismatch")
	}
	if !strings.Contains(proactiveOpenerPrompt, "opened the app for the first time") || !strings.Contains(proactiveOpenerPrompt, "Don't ask their name") {
		t.Fatalf("proactive prompt drifted: %q", proactiveOpenerPrompt)
	}
}
