package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/secretbox"
)

func TestSurfaceContextGoOwnership(t *testing.T) {
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE context_request_tokens,context_requests,durable_map_versions,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "surface-source-secret"
	cfg.Auth.CapabilitySecret = "surface-capability-secret"
	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}}
	channels := []data.DirectoryChannel{
		{ChannelID: "C123456", Name: "general"},
		{ChannelID: "C654321", Name: "private-room", IsPrivate: true},
		{ChannelID: "C777777", Name: "private-seen", IsPrivate: true},
	}
	channelMembers := []data.DirectoryPair{{ChannelID: "C777777", PrincipalID: "alice"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members, Channels: &channels, ChannelMembers: &channelMembers}); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("0123456789abcdef0123456789abcdef", nil, "connector-secrets")
	if err != nil {
		t.Fatal(err)
	}
	repository := data.NewContextRequestRepository(pg)
	h := &HTTPServer{
		config:           cfg,
		directory:        directory,
		contextRequests:  repository,
		connectorSecrets: box,
		auth:             auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool},
		logger:           slog.Default(),
	}

	encryptedToken, err := box.Encrypt("viewer-token")
	if err != nil {
		t.Fatal(err)
	}
	pendingRequest, err := repository.Create(ctx, "slack", []byte(`{"viewer":"alice","count":1}`), encryptedToken)
	if err != nil {
		t.Fatal(err)
	}
	pendingHTTP := httptest.NewRequest(http.MethodGet, "/v1/surface-context/pending?source=slack", nil)
	signSourceRequest(t, pendingHTTP, cfg.Auth.SourceSigningSecret, nil)
	pendingResponse := httptest.NewRecorder()
	h.serveOwned(pendingResponse, pendingHTTP)
	if pendingResponse.Code != http.StatusOK || !bytes.Contains(pendingResponse.Body.Bytes(), []byte(`"viewerToken":"viewer-token"`)) {
		t.Fatalf("pending=%d %s", pendingResponse.Code, pendingResponse.Body.String())
	}
	if _, err := repository.Delete(ctx, pendingRequest.ID); err != nil {
		t.Fatal(err)
	}

	capability := testCapabilityTokenWithClaims(t, cfg.Auth.CapabilitySecret, auth.Claims{
		ActorID: "alice", ScopeID: "personal:alice", Audience: "control-plane",
		Destination: &auth.Destination{Type: "slack", Target: "D123456"}, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
	})
	match := strings.Repeat("x", 205)
	contextBody := []byte(`{"channel":"#general","count":250,"match":"` + match + `"}`)
	contextRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-context", bytes.NewReader(contextBody))
	contextRequest.Header.Set(auth.CapabilityHeader, capability)
	contextResponse := httptest.NewRecorder()
	contextDone := make(chan struct{})
	go func() {
		h.ServeHTTP(contextResponse, contextRequest)
		close(contextDone)
	}()
	claimed := waitForSurfaceRequest(t, repository, "slack")
	var query map[string]any
	if err := json.Unmarshal(claimed.Query, &query); err != nil || query["channelId"] != "C123456" || query["viewer"] != "alice" || query["count"] != float64(200) || len(query["match"].(string)) != 200 {
		t.Fatalf("query=%s parsed=%#v err=%v", claimed.Query, query, err)
	}
	if ok, err := repository.Fulfill(ctx, claimed.ID, []byte(`{"messages":[{"text":"hello"}],"hasMore":false}`), nil); err != nil || !ok {
		t.Fatalf("fulfill=%v err=%v", ok, err)
	}
	select {
	case <-contextDone:
	case <-time.After(3 * time.Second):
		t.Fatal("surface context handler did not finish")
	}
	if contextResponse.Code != http.StatusOK || !bytes.Contains(contextResponse.Body.Bytes(), []byte(`"channel":"#general"`)) || !bytes.Contains(contextResponse.Body.Bytes(), []byte(`"text":"hello"`)) {
		t.Fatalf("context=%d %s", contextResponse.Code, contextResponse.Body.String())
	}

	privateBody := []byte(`{"channel":"private-room"}`)
	privateRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-context", bytes.NewReader(privateBody))
	privateRequest.Header.Set(auth.CapabilityHeader, capability)
	privateResponse := httptest.NewRecorder()
	h.ServeHTTP(privateResponse, privateRequest)
	if privateResponse.Code != http.StatusForbidden || !bytes.Contains(privateResponse.Body.Bytes(), []byte(`"error":"not_visible"`)) {
		t.Fatalf("private=%d %s", privateResponse.Code, privateResponse.Body.String())
	}

	fileBody := []byte(`{"ts":"1723497600.123456","channel":"general"}`)
	fileRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-file", bytes.NewReader(fileBody))
	fileRequest.Header.Set(auth.CapabilityHeader, capability)
	fileResponse := httptest.NewRecorder()
	fileDone := make(chan struct{})
	go func() {
		h.ServeHTTP(fileResponse, fileRequest)
		close(fileDone)
	}()
	fileClaimed := waitForSurfaceRequest(t, repository, "slack")
	blobID := "0123456789abcdef0123456789abcdef"
	if ok, err := repository.Fulfill(ctx, fileClaimed.ID, []byte(`{"messages":[],"file":{"blobId":"`+blobID+`","name":"notes.txt","sizeBytes":12,"mimetype":"text/plain"}}`), nil); err != nil || !ok {
		t.Fatalf("file fulfill=%v err=%v", ok, err)
	}
	select {
	case <-fileDone:
	case <-time.After(3 * time.Second):
		t.Fatal("surface file handler did not finish")
	}
	var fileResult struct {
		Download struct {
			Path  string `json:"path"`
			Token string `json:"token"`
		} `json:"download"`
	}
	if err := json.Unmarshal(fileResponse.Body.Bytes(), &fileResult); err != nil || fileResponse.Code != http.StatusOK || fileResult.Download.Path != "/v1/blobs/"+blobID {
		t.Fatalf("file=%d %s parsed=%#v err=%v", fileResponse.Code, fileResponse.Body.String(), fileResult, err)
	}
	claims, err := h.auth.VerifyBlobTransferCapability(fileResult.Download.Token, "read", blobID)
	if err != nil || claims.ActorID != "alice" || claims.ScopeID != "personal:alice" {
		t.Fatalf("download claims=%#v err=%v", claims, err)
	}
}

func waitForSurfaceRequest(t *testing.T, repository *data.ContextRequestRepository, source string) data.PendingContextRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := repository.Pending(context.Background(), source, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("surface request was not created")
	return data.PendingContextRequest{}
}
