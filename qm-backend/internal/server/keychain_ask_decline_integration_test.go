package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hejw/qm-backend/internal/auth"
	"github.com/hejw/qm-backend/internal/config"
	"github.com/hejw/qm-backend/internal/data"
)

func TestKeychainAskDeclineRoutePersistsForNodeResolutionSweep(t *testing.T) {
	databaseURL := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('qm-backend-integration-tests'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('qm-backend-integration-tests'))")
	})
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS keychain_asks(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE keychain_asks,durable_map_versions,audit_log RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UnixMilli()
	for _, askID := range []string{"ask-pending", "ask-triggered"} {
		payload, err := json.Marshal(map[string]any{
			"id": askID, "credentialId": "credential-1", "ownerId": "owner@example.com", "requesterId": "requester", "requesterScopeId": "channel:C1", "purpose": "deploy preview", "status": "pending", "createdAt": int64(1), "expiresAt": until,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_asks(id,json) VALUES($1,$2::jsonb)", askID, payload); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.QM.RouteMode = "go"
	cfg.Auth.CapabilitySecret = "keychain-ask-decline-secret"
	h := &HTTPServer{
		config:   cfg,
		keychain: data.NewKeychainStatusRepository(pg),
		audit:    data.NewAuditor(pg),
		auth:     auth.Verifier{CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool},
		logger:   slog.Default(),
	}
	if !h.owns(http.MethodPost, "/v1/keychain/asks/ask-pending/decline") {
		t.Fatal("keychain ask decline is not Go-owned")
	}
	ownerToken := testCapabilityToken(t, cfg.Auth.CapabilitySecret, "owner@example.com", "personal:owner@example.com")
	request := httptest.NewRequest(http.MethodPost, "/v1/keychain/asks/ask-pending/decline", bytes.NewReader([]byte(`{"note":"  no longer needed  "}`)))
	request.Header.Set(auth.CapabilityHeader, ownerToken)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"status":"declined"`)) || !bytes.Contains(response.Body.Bytes(), []byte(`"note":"no longer needed"`)) {
		t.Fatalf("decline response=%d %s", response.Code, response.Body.String())
	}
	var status, note string
	var notifiedAt *int64
	if err := pg.Pool.QueryRow(ctx, "SELECT json->>'status',json->>'note',(json->>'notifiedAt')::bigint FROM keychain_asks WHERE id='ask-pending'").Scan(&status, &note, &notifiedAt); err != nil || status != "declined" || note != "no longer needed" || notifiedAt != nil {
		t.Fatalf("stored ask status=%q note=%q notified=%v err=%v", status, note, notifiedAt, err)
	}
	var mapVersion, audits int64
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_asks'").Scan(&mapVersion); err != nil || mapVersion != 1 {
		t.Fatalf("ask map version=%d err=%v", mapVersion, err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='keychain.ask.decline' AND resource='ask-pending'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("ask decline audits=%d err=%v", audits, err)
	}
	repeat := httptest.NewRequest(http.MethodPost, "/v1/keychain/asks/ask-pending/decline", bytes.NewReader([]byte(`{}`)))
	repeat.Header.Set(auth.CapabilityHeader, ownerToken)
	repeatResponse := httptest.NewRecorder()
	h.ServeHTTP(repeatResponse, repeat)
	if repeatResponse.Code != http.StatusGone || !bytes.Contains(repeatResponse.Body.Bytes(), []byte("ask already declined")) {
		t.Fatalf("repeated decline=%d %s", repeatResponse.Code, repeatResponse.Body.String())
	}
	wrongOwner := httptest.NewRequest(http.MethodPost, "/v1/keychain/asks/ask-triggered/decline", bytes.NewReader([]byte(`{}`)))
	wrongOwner.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "other@example.com", "personal:other@example.com"))
	wrongOwnerResponse := httptest.NewRecorder()
	h.ServeHTTP(wrongOwnerResponse, wrongOwner)
	if wrongOwnerResponse.Code != http.StatusForbidden || !bytes.Contains(wrongOwnerResponse.Body.Bytes(), []byte("only the credential's owner")) {
		t.Fatalf("wrong owner decline=%d %s", wrongOwnerResponse.Code, wrongOwnerResponse.Body.String())
	}
	triggeredToken := testCapabilityTokenWithClaims(t, cfg.Auth.CapabilitySecret, auth.Claims{ActorID: "owner@example.com", ScopeID: "personal:owner@example.com", Audience: "control-plane", Triggered: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
	triggered := httptest.NewRequest(http.MethodPost, "/v1/keychain/asks/ask-triggered/decline", bytes.NewReader([]byte(`{}`)))
	triggered.Header.Set(auth.CapabilityHeader, triggeredToken)
	triggeredResponse := httptest.NewRecorder()
	h.ServeHTTP(triggeredResponse, triggered)
	if triggeredResponse.Code != http.StatusForbidden || !bytes.Contains(triggeredResponse.Body.Bytes(), []byte("consent can only be recorded")) {
		t.Fatalf("triggered decline=%d %s", triggeredResponse.Code, triggeredResponse.Body.String())
	}
}
