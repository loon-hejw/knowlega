package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthenticateSourceRequest(t *testing.T) {
	body := []byte(`{"members":[]}`)
	req := httptest.NewRequest("POST", "http://qm.test/v1/directory?full=1", strings.NewReader(string(body)))
	now := time.Unix(1_700_000_000, 0)
	req.Header.Set("x-timestamp", "1700000000")
	req.Header.Set("x-signature", sourceSignature("source-secret", now.Unix(), req.Method, req.URL.RequestURI(), body))
	verifier := Verifier{SourceSecret: "source-secret", ReplayWindow: 5 * time.Minute, Now: func() time.Time { return now }}
	identity, err := verifier.Authenticate(context.Background(), req, body, "source")
	if err != nil {
		t.Fatal(err)
	}
	if !identity.Source {
		t.Fatal("expected source identity")
	}
}

func TestAuthenticateCapability(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{ActorID: "alice", ScopeID: "personal:alice", Audience: "control-plane", LiveAuthor: true, ExpiresAt: now.Add(time.Minute).UnixMilli()}
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, capabilityToken(t, "cap-secret", claims))
	verifier := Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}
	identity, err := verifier.Authenticate(context.Background(), req, nil, "either")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorID != "alice" || identity.ScopeID != "personal:alice" || !identity.LiveAuthor {
		t.Fatalf("unexpected identity %#v", identity)
	}
}

func TestAuthenticateCapabilityRejectsExpiredToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, capabilityToken(t, "cap-secret", Claims{ActorID: "alice", ScopeID: "personal:alice", ExpiresAt: now.Add(-time.Second).UnixMilli()}))
	_, err := (Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}).Authenticate(context.Background(), req, nil, "either")
	if err == nil {
		t.Fatal("expected expired capability to be rejected")
	}
}

func TestAuthenticateCapabilityRejectsNullObjectAndArrayClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	verifier := Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}
	for name, payload := range map[string]map[string]any{
		"memory": {
			"actorId": "alice", "scopeId": "personal:alice", "exp": now.Add(time.Minute).UnixMilli(), "memory": nil,
		},
		"keychain-members": {
			"actorId": "alice", "scopeId": "personal:alice", "exp": now.Add(time.Minute).UnixMilli(), "keychainMembers": nil,
		},
		"destinations": {
			"actorId": "alice", "scopeId": "personal:alice", "exp": now.Add(time.Minute).UnixMilli(), "destinations": nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
			req.Header.Set(CapabilityHeader, capabilityPayloadToken(t, "cap-secret", payload))
			if _, err := verifier.Authenticate(context.Background(), req, nil, "either"); err == nil {
				t.Fatalf("accepted explicit null %s claim", name)
			}
		})
	}
}

func TestAuthenticateCapabilityDestinationClaimMatchesNodeShapeValidation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	verifier := Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}
	// Node checks only that destinations is an array. A malformed item cannot
	// become an authority here because selecting it still requires its key.
	payload := map[string]any{
		"actorId": "alice", "scopeId": "personal:alice", "exp": now.Add(time.Minute).UnixMilli(),
		"destinations": []any{"not-a-destination", map[string]any{"key": "slack-c1", "type": "slack", "target": "C1"}},
		// Node does not shape-validate this optional, unused-by-retarget claim.
		"defaultDestinationKey": 42,
	}
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, capabilityPayloadToken(t, "cap-secret", payload))
	identity, err := verifier.Authenticate(context.Background(), req, nil, "either")
	if err != nil || len(identity.Destinations) != 2 || identity.Destinations[1].Key != "slack-c1" {
		t.Fatalf("destination claim identity=%#v err=%v", identity, err)
	}
}

func TestAuthenticateCapabilityCopiesCurrentDestinationAndScopeVersion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ActorID:      "alice",
		ScopeID:      "channel:C123456",
		ScopeVersion: "42",
		Audience:     "control-plane",
		Destination:  &Destination{Type: "slack", Target: "C123456", AudienceScopeID: "channel:C123456"},
		ExpiresAt:    now.Add(time.Minute).UnixMilli(),
	}
	req := httptest.NewRequest("POST", "http://qm.test/v1/surface-context", nil)
	req.Header.Set(CapabilityHeader, capabilityToken(t, "cap-secret", claims))
	identity, err := (Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}).Authenticate(context.Background(), req, nil, "either")
	if err != nil || identity.ScopeVersion != "42" || identity.Destination == nil || identity.Destination.Target != "C123456" {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
}

func TestAuthenticateCapabilityProjectsKeychainMembers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ActorID:         "alice",
		ScopeID:         "channel:C1",
		Audience:        "control-plane",
		KeychainMembers: []KeychainMember{{ID: "bob"}},
		ExpiresAt:       now.Add(time.Minute).UnixMilli(),
	}
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, capabilityToken(t, "cap-secret", claims))
	identity, err := (Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}).Authenticate(context.Background(), req, nil, "either")
	if err != nil || len(identity.KeychainMembers) != 1 || identity.KeychainMembers[0].ID != "bob" {
		t.Fatalf("keychain member capability identity=%#v err=%v", identity, err)
	}
}

func TestAuthenticateCapabilityRejectsNonHS256Header(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	payload, err := json.Marshal(Claims{ActorID: "alice", ScopeID: "personal:alice", ExpiresAt: now.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte("cap-secret"))
	_, _ = mac.Write([]byte(input))
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, input+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	if _, err := (Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}).Authenticate(context.Background(), req, nil, "either"); err == nil {
		t.Fatal("non-HS256 token was accepted")
	}
}

func TestVerifyLegacySignedTokens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{ActorID: "alice", ScopeID: "personal:alice", ExpiresAt: now.Add(time.Minute).UnixMilli()}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://qm.test/v1/projects", nil)
	req.Header.Set(CapabilityHeader, legacyToken("cap-secret", payload))
	identity, err := (Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}).Authenticate(context.Background(), req, nil, "either")
	if err != nil || identity.ActorID != "alice" {
		t.Fatalf("legacy capability identity=%#v err=%v", identity, err)
	}
	portalPayload, err := json.Marshal(PortalIdentity{PrincipalID: "alice", ExpiresAt: now.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	portal, err := VerifyPortalIdentity(legacyToken("portal-secret", portalPayload), "portal-secret", now)
	if err != nil || portal.PrincipalID != "alice" {
		t.Fatalf("legacy portal=%#v err=%v", portal, err)
	}
}

func TestVerifyBlobTransferCapability(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ActorID:   "alice",
		ScopeID:   "personal:alice",
		Audience:  "blob-transfer",
		Blob:      &BlobGrant{Dir: "read", ID: "0123456789abcdef0123456789abcdef"},
		ExpiresAt: now.Add(time.Minute).UnixMilli(),
	}
	verifier := Verifier{CapabilitySecret: "cap-secret", Now: func() time.Time { return now }}
	token := capabilityToken(t, "cap-secret", claims)
	verified, err := verifier.VerifyBlobTransferCapability(token, "read", claims.Blob.ID)
	if err != nil || verified.ActorID != "alice" {
		t.Fatalf("verified blob capability=%#v err=%v", verified, err)
	}
	if _, err := verifier.VerifyBlobTransferCapability(token, "read", "fedcba9876543210fedcba9876543210"); err == nil || HTTPStatus(err) != 403 {
		t.Fatalf("wrong blob id error=%v status=%d", err, HTTPStatus(err))
	}
	if _, err := verifier.VerifyBlobTransferCapability(token, "write", ""); err == nil || HTTPStatus(err) != 403 {
		t.Fatalf("read token granted write: %v", err)
	}
}

func sourceSignature(secret string, timestamp int64, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + "1700000000" + ":" + method + "\n" + path + "\n" + string(body)))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func capabilityToken(t *testing.T, secret string, claims Claims) string {
	t.Helper()
	return capabilityPayloadToken(t, secret, claims)
}

func capabilityPayloadToken(t *testing.T, secret string, payloadValue any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		t.Fatal(err)
	}
	encoded := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func legacyToken(secret string, payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
