package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hejw/qm-backend/internal/auth"
	"github.com/hejw/qm-backend/internal/biz"
	"github.com/hejw/qm-backend/internal/config"
	"github.com/hejw/qm-backend/internal/data"
	"github.com/hejw/qm-backend/internal/knowledge"
)

func TestReadyzReflectsSharedDatabaseReadiness(t *testing.T) {
	ready := &HTTPServer{readiness: func(context.Context) error { return nil }}
	readyResponse := httptest.NewRecorder()
	ready.ServeHTTP(readyResponse, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readyResponse.Code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(readyResponse.Body.Bytes()), []byte(`{"ok":true}`)) {
		t.Fatalf("ready response=%d %s", readyResponse.Code, readyResponse.Body.String())
	}

	notReady := &HTTPServer{readiness: func(context.Context) error { return fmt.Errorf("database unavailable") }}
	notReadyResponse := httptest.NewRecorder()
	notReady.ServeHTTP(notReadyResponse, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReadyResponse.Code != http.StatusServiceUnavailable || !bytes.Equal(bytes.TrimSpace(notReadyResponse.Body.Bytes()), []byte(`{"ok":false}`)) {
		t.Fatalf("not-ready response=%d %s", notReadyResponse.Code, notReadyResponse.Body.String())
	}
}

func TestProjectRouteCompatibility(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE projects,durable_map_versions,deactivated_principals,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,admin_grants,audit_log RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.CapabilitySecret = "test-capability-secret"
	cfg.Auth.SourceSigningSecret = "test-project-source-secret"
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}, {PrincipalID: "bob", DisplayName: "Bob", Type: "internal"}, {PrincipalID: "guest", DisplayName: "Guest", Type: "guest"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	knowledgeEngine, err := knowledge.New(t.TempDir(), data.NewKnowledgeScopeRepository(pg), data.NewKnowledgeQueryRepository(pg))
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTPServer{config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, knowledge: knowledgeEngine, directory: directory, acl: data.NewACLRepository(pg), environments: data.NewEnvironmentRepository(pg, cfg.QM.OrgID), channelPolicy: data.NewChannelPolicyRepository(pg, cfg.QM.OrgID), audit: data.NewAuditor(pg), auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default()}
	body := []byte(`{"name":"Roadmap"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(body))
	req.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", res.Code, res.Body.String())
	}
	var created struct {
		Project biz.Project `json:"project"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Project.OwnerID != "alice" || created.Project.Name != "Roadmap" {
		t.Fatalf("unexpected project %#v", created.Project)
	}
	var createdView struct {
		Project struct {
			ScopeID string `json:"scopeId"`
			Members []struct {
				PrincipalID string `json:"principalId"`
				DisplayName string `json:"displayName"`
			} `json:"members"`
		} `json:"project"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &createdView); err != nil || createdView.Project.ScopeID != biz.ProjectScopeID(created.Project.ID) || len(createdView.Project.Members) != 1 || createdView.Project.Members[0].DisplayName != "Alice" {
		t.Fatalf("project view=%s parsed=%#v err=%v", res.Body.String(), createdView, err)
	}
	guestProjectBody := []byte(`{"principalId":"guest","name":"No access"}`)
	guestProject := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(guestProjectBody))
	signSourceRequest(t, guestProject, cfg.Auth.SourceSigningSecret, guestProjectBody)
	guestProjectResponse := httptest.NewRecorder()
	h.ServeHTTP(guestProjectResponse, guestProject)
	if guestProjectResponse.Code != http.StatusForbidden || !bytes.Contains(guestProjectResponse.Body.Bytes(), []byte(`"error":"forbidden"`)) {
		t.Fatalf("external project owner=%d %s", guestProjectResponse.Code, guestProjectResponse.Body.String())
	}
	if _, err := knowledgeEngine.GetStatus(ctx, knowledge.ScopeRef{OrgID: "acme", ExternalScopeID: biz.ProjectScopeID(created.Project.ID), Kind: "project"}); err != nil {
		t.Fatalf("project knowledge scope was not ensured: %v", err)
	}
	addMemberBody := []byte(`{"memberId":"bob"}`)
	addMember := httptest.NewRequest(http.MethodPost, "/v1/projects/"+created.Project.ID+"/members", bytes.NewReader(addMemberBody))
	addMember.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	addMemberResponse := httptest.NewRecorder()
	h.ServeHTTP(addMemberResponse, addMember)
	if addMemberResponse.Code != http.StatusOK || !bytes.Contains(addMemberResponse.Body.Bytes(), []byte(`"memberIds":["alice","bob"]`)) || !bytes.Contains(addMemberResponse.Body.Bytes(), []byte(`"displayName":"Bob"`)) {
		t.Fatalf("project member add=%d %s", addMemberResponse.Code, addMemberResponse.Body.String())
	}
	var rosterVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT (json->>'updatedAt')::bigint FROM projects WHERE id=$1", created.Project.ID).Scan(&rosterVersion); err != nil {
		t.Fatal(err)
	}
	guestBody := []byte(`{"memberId":"guest"}`)
	guestAdd := httptest.NewRequest(http.MethodPost, "/v1/projects/"+created.Project.ID+"/members", bytes.NewReader(guestBody))
	guestAdd.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	guestAddResponse := httptest.NewRecorder()
	h.ServeHTTP(guestAddResponse, guestAdd)
	if guestAddResponse.Code != http.StatusBadRequest || !bytes.Contains(guestAddResponse.Body.Bytes(), []byte(`"error":"invalid_member"`)) {
		t.Fatalf("guest project member=%d %s", guestAddResponse.Code, guestAddResponse.Body.String())
	}
	renameBody := []byte(`{"name":"Roadmap 2026"}`)
	rename := httptest.NewRequest(http.MethodPatch, "/v1/projects/"+created.Project.ID, bytes.NewReader(renameBody))
	rename.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	renameResponse := httptest.NewRecorder()
	h.ServeHTTP(renameResponse, rename)
	if renameResponse.Code != http.StatusOK || !bytes.Contains(renameResponse.Body.Bytes(), []byte(`"name":"Roadmap 2026"`)) {
		t.Fatalf("project rename=%d %s", renameResponse.Code, renameResponse.Body.String())
	}
	var renamedVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT (json->>'updatedAt')::bigint FROM projects WHERE id=$1", created.Project.ID).Scan(&renamedVersion); err != nil || renamedVersion != rosterVersion {
		t.Fatalf("project rename changed roster version: before=%d after=%d err=%v", rosterVersion, renamedVersion, err)
	}
	knowledgeStatus, err := knowledgeEngine.GetStatus(ctx, knowledge.ScopeRef{OrgID: "acme", ExternalScopeID: biz.ProjectScopeID(created.Project.ID), Kind: "project"})
	if err != nil || knowledgeStatus.Scope.ProjectName != "Roadmap 2026" {
		t.Fatalf("renamed project knowledge scope=%#v err=%v", knowledgeStatus, err)
	}
	bobRename := httptest.NewRequest(http.MethodPatch, "/v1/projects/"+created.Project.ID, bytes.NewReader(renameBody))
	bobRename.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "bob", "personal:bob"))
	bobRenameResponse := httptest.NewRecorder()
	h.ServeHTTP(bobRenameResponse, bobRename)
	if bobRenameResponse.Code != http.StatusNotFound || !bytes.Contains(bobRenameResponse.Body.Bytes(), []byte(`"error":"not_found"`)) {
		t.Fatalf("capability project rename denial=%d %s", bobRenameResponse.Code, bobRenameResponse.Body.String())
	}
	sourceRenameBody := []byte(`{"principalId":"bob","name":"Cannot rename"}`)
	sourceRename := httptest.NewRequest(http.MethodPatch, "/v1/projects/"+created.Project.ID, bytes.NewReader(sourceRenameBody))
	signSourceRequest(t, sourceRename, cfg.Auth.SourceSigningSecret, sourceRenameBody)
	sourceRenameResponse := httptest.NewRecorder()
	h.ServeHTTP(sourceRenameResponse, sourceRename)
	if sourceRenameResponse.Code != http.StatusForbidden || !bytes.Contains(sourceRenameResponse.Body.Bytes(), []byte(`"error":"forbidden"`)) {
		t.Fatalf("source project rename denial=%d %s", sourceRenameResponse.Code, sourceRenameResponse.Body.String())
	}
	removeMember := httptest.NewRequest(http.MethodDelete, "/v1/projects/"+created.Project.ID+"/members/bob", nil)
	removeMember.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	removeMemberResponse := httptest.NewRecorder()
	h.ServeHTTP(removeMemberResponse, removeMember)
	if removeMemberResponse.Code != http.StatusOK || bytes.Contains(removeMemberResponse.Body.Bytes(), []byte(`"bob"`)) {
		t.Fatalf("project member remove=%d %s", removeMemberResponse.Code, removeMemberResponse.Body.String())
	}
	listReq := httptest.NewRequest(http.MethodGet, "/v1/projects?principalId=bob", nil)
	listReq.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	listRes := httptest.NewRecorder()
	h.ServeHTTP(listRes, listReq)
	if listRes.Code != http.StatusNotFound {
		t.Fatalf("capability actor substitution changed: %d %s", listRes.Code, listRes.Body.String())
	}
}

func TestCapabilityAdminRestrictions(t *testing.T) {
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.Auth.CapabilitySecret = "test-capability-secret"
	h := &HTTPServer{config: cfg, auth: auth.Verifier{CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute}, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/runs?scope=org:acme", nil)
	req.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "admin", "personal:admin"))
	res := httptest.NewRecorder()
	h.serveOwned(res, req)
	if res.Code != http.StatusForbidden || !bytes.Contains(res.Body.Bytes(), []byte(`"error":"forbidden"`)) {
		t.Fatalf("admin capability restriction=%d %s", res.Code, res.Body.String())
	}
	live := httptest.NewRequest(http.MethodGet, "/v1/admin/runs?scope=org:acme", nil)
	if denied := h.capabilityAdminDenied(live, auth.Identity{ActorID: "admin", ScopeID: "personal:admin", LiveActor: true}); denied != "" {
		t.Fatalf("live personal admin request was denied: %s", denied)
	}
	shared := httptest.NewRequest(http.MethodGet, "/v1/admin/runs?scope=org:acme", nil)
	if denied := h.capabilityAdminDenied(shared, auth.Identity{ActorID: "admin", ScopeID: "channel:C1", LiveActor: true}); denied == "" {
		t.Fatal("shared-scope sensitive admin read was allowed")
	}
	shadow := httptest.NewRequest(http.MethodGet, "/v1/admin/deliveries/shadow?scope=org:acme", nil)
	if denied := h.capabilityAdminDenied(shadow, auth.Identity{ActorID: "admin", ScopeID: "channel:C1", LiveActor: true}); denied == "" {
		t.Fatal("shared-scope shadow delivery read was allowed")
	}
	files := httptest.NewRequest(http.MethodGet, "/v1/admin/files?scope=org:acme", nil)
	if denied := h.capabilityAdminDenied(files, auth.Identity{ActorID: "admin", ScopeID: "channel:C1", LiveActor: true}); denied == "" {
		t.Fatal("shared-scope admin file read was allowed")
	}
	keychain := httptest.NewRequest(http.MethodGet, "/v1/admin/keychain", nil)
	if denied := h.capabilityAdminDenied(keychain, auth.Identity{ActorID: "admin", ScopeID: "channel:C1", LiveActor: true}); denied == "" {
		t.Fatal("shared-scope admin keychain read was allowed")
	}
}

func TestSourceSkillCatalogCompatibility(t *testing.T) {
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE skills,sessions,projects,durable_map_versions,deactivated_principals,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,admin_grants,audit_log RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "skill-source-secret"
	cfg.Auth.CapabilitySecret = "skill-capability-secret"
	cfg.Auth.PortalIdentitySecret = "skill-portal-secret"
	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}}
	channels := []data.DirectoryChannel{{ChannelID: "C1", Name: "private", IsPrivate: true}, {ChannelID: "C2", Name: "unseen", IsPrivate: true}}
	channelMembers := []data.DirectoryPair{{ChannelID: "C1", PrincipalID: "alice"}}
	groupMembers := []data.DirectoryPair{{GroupID: "G1", PrincipalID: "alice"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members, Channels: &channels, ChannelMembers: &channelMembers, GroupMembers: &groupMembers}); err != nil {
		t.Fatal(err)
	}
	insertSkill := func(id, scopeID, name, status, createdBy string, pack bool) {
		t.Helper()
		record := map[string]any{
			"id": id, "scopeId": scopeID, "status": status, "createdBy": createdBy, "version": 1,
			"manifest": map[string]any{"name": name, "description": name + " description", "body": "body", "requiredCapabilities": []string{}, "files": []any{}},
		}
		if pack {
			record["pack"] = map[string]string{"packId": "pack-1", "commit": "abc", "upstreamName": name}
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Pool.Exec(ctx, "INSERT INTO skills(id,json) VALUES($1,$2::jsonb)", id, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	insertSkill("01-org-build", "org:acme", "build", "published", "admin", false)
	insertSkill("02-channel-build", "channel:C1", "build", "published", "alice", false)
	insertSkill("03-personal-build", "personal:alice", "build", "published", "alice", false)
	insertSkill("04-group-deploy", "group:G1", "deploy", "published", "alice", true)
	insertSkill("05-org-review", "org:acme", "review", "published", "admin", false)
	insertSkill("06-archived", "personal:alice", "old", "archived", "alice", false)
	insertSkill("07-unseen", "channel:C2", "unseen", "published", "alice", false)
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	h := &HTTPServer{
		config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, directory: directory,
		sessions: data.NewSessionRepository(pg), skills: data.NewSkillRepository(pg),
		auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default(),
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills?principalId=alice&includeShadowed=1", nil)
	signSourceRequest(t, req, cfg.Auth.SourceSigningSecret, nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("skill catalog=%d %s", res.Code, res.Body.String())
	}
	// Node signs source GETs but deliberately does not consume their replay
	// record, so a polling client may reuse the same signed read request.
	repeat := httptest.NewRequest(http.MethodGet, "/v1/skills?principalId=alice&includeShadowed=1", nil)
	repeat.Header.Set("x-timestamp", req.Header.Get("x-timestamp"))
	repeat.Header.Set("x-signature", req.Header.Get("x-signature"))
	repeatResponse := httptest.NewRecorder()
	h.ServeHTTP(repeatResponse, repeat)
	if repeatResponse.Code != http.StatusOK {
		t.Fatalf("repeated source skill catalog=%d %s", repeatResponse.Code, repeatResponse.Body.String())
	}
	var response struct {
		Skills []struct {
			ID       string `json:"id"`
			Scope    string `json:"scope"`
			Shadowed bool   `json:"shadowed"`
			Editable bool   `json:"editable"`
			Source   string `json:"source"`
			Pack     *struct {
				PackID string `json:"packId"`
			} `json:"pack"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got, want := len(response.Skills), 6; got != want {
		t.Fatalf("skill catalog count=%d want=%d: %s", got, want, res.Body.String())
	}
	gotIDs := make([]string, 0, len(response.Skills))
	for _, skill := range response.Skills {
		gotIDs = append(gotIDs, skill.ID)
	}
	if got, want := strings.Join(gotIDs, ","), "03-personal-build,02-channel-build,01-org-build,04-group-deploy,05-org-review,06-archived"; got != want {
		t.Fatalf("skill order=%q want=%q", got, want)
	}
	if !response.Skills[0].Shadowed || response.Skills[0].Scope != "personal" || !response.Skills[0].Editable {
		t.Fatalf("personal resolution=%#v", response.Skills[0])
	}
	if response.Skills[1].Shadowed || response.Skills[2].Shadowed || response.Skills[3].Source != "pack" || response.Skills[3].Pack == nil || response.Skills[3].Pack.PackID != "pack-1" {
		t.Fatalf("shadow/pack projection=%#v", response.Skills)
	}
	for _, skill := range response.Skills {
		if skill.ID == "07-unseen" {
			t.Fatalf("unreachable skill leaked: %s", res.Body.String())
		}
	}
	detail := httptest.NewRequest(http.MethodGet, "/v1/skills/03-personal-build", nil)
	detail.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	detailResponse := httptest.NewRecorder()
	h.ServeHTTP(detailResponse, detail)
	if detailResponse.Code != http.StatusOK || !bytes.Contains(detailResponse.Body.Bytes(), []byte(`"body":"body"`)) || !bytes.Contains(detailResponse.Body.Bytes(), []byte(`"editable":true`)) {
		t.Fatalf("capability skill detail=%d %s", detailResponse.Code, detailResponse.Body.String())
	}
	portalDetail := httptest.NewRequest(http.MethodGet, "/v1/skills/04-group-deploy", nil)
	portalDetail.Header.Set("x-portal-identity", testPortalIdentityToken(t, cfg.Auth.PortalIdentitySecret, "alice"))
	signSourceRequest(t, portalDetail, cfg.Auth.SourceSigningSecret, nil)
	portalDetailResponse := httptest.NewRecorder()
	h.ServeHTTP(portalDetailResponse, portalDetail)
	if portalDetailResponse.Code != http.StatusOK || !bytes.Contains(portalDetailResponse.Body.Bytes(), []byte(`"scope":"group"`)) {
		t.Fatalf("portal skill detail=%d %s", portalDetailResponse.Code, portalDetailResponse.Body.String())
	}
	unauthenticatedDetail := httptest.NewRequest(http.MethodGet, "/v1/skills/03-personal-build?principalId=alice", nil)
	signSourceRequest(t, unauthenticatedDetail, cfg.Auth.SourceSigningSecret, nil)
	unauthenticatedDetailResponse := httptest.NewRecorder()
	h.ServeHTTP(unauthenticatedDetailResponse, unauthenticatedDetail)
	if unauthenticatedDetailResponse.Code != http.StatusUnauthorized || !bytes.Contains(unauthenticatedDetailResponse.Body.Bytes(), []byte(`"error":"capability_required"`)) {
		t.Fatalf("unsigned identity skill detail=%d %s", unauthenticatedDetailResponse.Code, unauthenticatedDetailResponse.Body.String())
	}
	writeBody := []byte(`{"members":[]}`)
	write := httptest.NewRequest(http.MethodPost, "/v1/directory", bytes.NewReader(writeBody))
	signSourceRequest(t, write, cfg.Auth.SourceSigningSecret, writeBody)
	if _, err := h.auth.Authenticate(ctx, write, writeBody, "source"); err != nil {
		t.Fatalf("first source mutation authentication: %v", err)
	}
	repeatedWrite := httptest.NewRequest(http.MethodPost, "/v1/directory", bytes.NewReader(writeBody))
	repeatedWrite.Header.Set("x-timestamp", write.Header.Get("x-timestamp"))
	repeatedWrite.Header.Set("x-signature", write.Header.Get("x-signature"))
	if _, err := h.auth.Authenticate(ctx, repeatedWrite, writeBody, "source"); err == nil || auth.HTTPStatus(err) != http.StatusUnauthorized {
		t.Fatalf("repeated source mutation err=%v status=%d", err, auth.HTTPStatus(err))
	}
}

func TestLocalBlobTransferCompatibility(t *testing.T) {
	transferRoot := t.TempDir()
	cfg := config.Default()
	cfg.QM.RouteMode = "go"
	cfg.QM.FileStore.Mode = "local"
	cfg.QM.FileStore.LocalDir = t.TempDir()
	cfg.QM.FileStore.TransferLocalDir = transferRoot
	cfg.Auth.SourceSigningSecret = "blob-source-secret"
	cfg.Auth.CapabilitySecret = "blob-capability-secret"
	h := &HTTPServer{config: cfg, auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute}, logger: slog.Default()}

	body := []byte("opaque transfer bytes")
	digest := sha256.Sum256(body)
	put := httptest.NewRequest(http.MethodPost, "/v1/blobs", bytes.NewReader(body))
	put.Header.Set("x-content-sha256", hex.EncodeToString(digest[:]))
	signBlobSourceRequest(t, put, cfg.Auth.SourceSigningSecret)
	putResult := httptest.NewRecorder()
	h.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK {
		t.Fatalf("local source blob upload=%d %s", putResult.Code, putResult.Body.String())
	}
	var uploaded struct {
		BlobID    string `json:"blobId"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(putResult.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if !blobTransferIDPattern.MatchString(uploaded.BlobID) || uploaded.SizeBytes != int64(len(body)) {
		t.Fatalf("unexpected uploaded blob %#v", uploaded)
	}
	if _, err := os.Stat(filepath.Join(transferRoot, uploaded.BlobID)); err != nil {
		t.Fatalf("staged blob missing: %v", err)
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/blobs/"+uploaded.BlobID, nil)
	signBlobSourceRequest(t, get, cfg.Auth.SourceSigningSecret)
	getResult := httptest.NewRecorder()
	h.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK || !bytes.Equal(getResult.Body.Bytes(), body) || getResult.Header().Get("content-type") != "application/octet-stream" {
		t.Fatalf("local source blob read=%d type=%q body=%q", getResult.Code, getResult.Header().Get("content-type"), getResult.Body.Bytes())
	}

	capability, err := auth.MintCapability(auth.Claims{ActorID: "alice", ScopeID: "personal:alice", Audience: "blob-transfer", Blob: &auth.BlobGrant{Dir: "read", ID: uploaded.BlobID}, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	capGet := httptest.NewRequest(http.MethodGet, "/v1/blobs/"+uploaded.BlobID, nil)
	capGet.Header.Set(auth.CapabilityHeader, capability)
	capGetResult := httptest.NewRecorder()
	h.ServeHTTP(capGetResult, capGet)
	if capGetResult.Code != http.StatusOK || !bytes.Equal(capGetResult.Body.Bytes(), body) {
		t.Fatalf("local capability blob read=%d %s", capGetResult.Code, capGetResult.Body.String())
	}

	badDigest := httptest.NewRequest(http.MethodPost, "/v1/blobs", strings.NewReader("different"))
	badDigest.Header.Set("x-content-sha256", strings.Repeat("0", 64))
	signBlobSourceRequest(t, badDigest, cfg.Auth.SourceSigningSecret)
	badDigestResult := httptest.NewRecorder()
	h.ServeHTTP(badDigestResult, badDigest)
	if badDigestResult.Code != http.StatusBadRequest || !bytes.Contains(badDigestResult.Body.Bytes(), []byte(`"error":"hash_mismatch"`)) {
		t.Fatalf("blob digest mismatch=%d %s", badDigestResult.Code, badDigestResult.Body.String())
	}
}

func TestRunDeliveryStateRouteOwnership(t *testing.T) {
	h := &HTTPServer{}
	h.config.QM.SlackEnvironmentState = "unknown"
	if !h.owns(http.MethodPost, "/v1/runs/run-1/delivery-state") {
		t.Fatal("delivery-state route is not Go-owned")
	}
	if h.owns(http.MethodPost, "/v1/runs/run-1/delivery-state/extra") {
		t.Fatal("delivery-state route accepted an invalid suffix")
	}
	if !h.owns(http.MethodPost, "/v1/runs/run-1/signal") {
		t.Fatal("run signal route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/approvals/pending") || !h.owns(http.MethodGet, "/v1/approvals/approval-1") {
		t.Fatal("approval read routes are not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/admin/impersonate") || !h.owns(http.MethodPost, "/v1/admin/impersonate/stop") {
		t.Fatal("admin impersonation routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/files") {
		t.Fatal("admin file list route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/memory") || !h.owns(http.MethodPut, "/v1/admin/memory") {
		t.Fatal("admin memory routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/memory/scopes") {
		t.Fatal("admin memory scope list route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/deployments") {
		t.Fatal("admin deployment list route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/crons") {
		t.Fatal("admin cron list route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/scopes") {
		t.Fatal("admin scope overview route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/resources") {
		t.Fatal("admin resources manifest route is not Go-owned")
	}
	if !h.owns(http.MethodDelete, "/v1/skills/skill-1") {
		t.Fatal("owned skill archive route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/share") {
		t.Fatal("ordinary artifact share route is not Go-owned")
	}
	if h.proxyArtifactShare(httptest.NewRequest(http.MethodPost, "/v1/share", nil), []byte(`{"type":"file","id":"f","toScope":"personal:U2"}`)) {
		t.Fatal("ordinary artifact share must be served by Go")
	}
	if !h.proxyArtifactShare(httptest.NewRequest(http.MethodPost, "/v1/share", nil), []byte(`{"type":"skill","id":"s","toScope":"org"}`)) || !h.proxyArtifactShare(httptest.NewRequest(http.MethodPost, "/v1/share", nil), []byte(`{"type":"deploy","id":"d","toScope":"personal:U2","move":true}`)) {
		t.Fatal("skill promotion and moves must retain Node ownership")
	}
	if !h.owns(http.MethodGet, "/v1/admin/skills") || !h.owns(http.MethodGet, "/v1/admin/skills/skill-1") || !h.owns(http.MethodDelete, "/v1/admin/skills/skill-1") {
		t.Fatal("admin skill read routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/skill-packs") {
		t.Fatal("admin skill pack list route is not Go-owned")
	}
	if !h.owns(http.MethodDelete, "/v1/admin/skill-packs/pack-1") {
		t.Fatal("admin skill pack delete route is not Go-owned")
	}
	if !h.owns(http.MethodPatch, "/v1/admin/skill-packs/pack-1") {
		t.Fatal("admin skill pack patch route is not Go-owned")
	}
	if !h.owns(http.MethodPut, "/v1/admin/crons/cron-1/destination") {
		t.Fatal("admin cron destination route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/crons/cron-1/disable") {
		t.Fatal("cron disable route is not Go-owned")
	}
	if !h.owns(http.MethodDelete, "/v1/crons/cron-1") {
		t.Fatal("cron delete route is not Go-owned")
	}
	if !h.owns(http.MethodPatch, "/v1/crons/cron-1") {
		t.Fatal("cron patch route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/crons/cron-1/destination") {
		t.Fatal("capability cron destination route is not Go-owned")
	}
	capabilityCronDisable := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-1/disable", nil)
	capabilityCronDisable.Header.Set(auth.CapabilityHeader, "test")
	if h.proxyCronRoute(capabilityCronDisable) {
		t.Fatal("capability cron disable must be served by Go")
	}
	capabilityCronDelete := httptest.NewRequest(http.MethodDelete, "/v1/crons/cron-1", nil)
	capabilityCronDelete.Header.Set(auth.CapabilityHeader, "test")
	if h.proxyCronRoute(capabilityCronDelete) {
		t.Fatal("capability cron delete must be served by Go")
	}
	capabilityCronPatch := httptest.NewRequest(http.MethodPatch, "/v1/crons/cron-1", bytes.NewReader([]byte(`{"title":"patch"}`)))
	capabilityCronPatch.Header.Set(auth.CapabilityHeader, "test")
	if h.proxyCronRoute(capabilityCronPatch) {
		t.Fatal("capability owner cron patch must be considered by Go")
	}
	capabilityCronDestination := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-1/destination", bytes.NewReader([]byte(`{"destinationKey":"slack-c1"}`)))
	capabilityCronDestination.Header.Set(auth.CapabilityHeader, "test")
	if h.proxyCronRoute(capabilityCronDestination) {
		t.Fatal("capability cron destination mutation must be served by Go")
	}
	if !h.owns(http.MethodGet, "/v1/admin/keychain") {
		t.Fatal("admin keychain metadata route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/custom-providers") {
		t.Fatal("admin custom provider status route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/slack-installation") {
		t.Fatal("admin Slack installation status route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/sandbox-routes") {
		t.Fatal("admin sandbox route read is not Go-owned")
	}
	unknownSlack := httptest.NewRequest(http.MethodGet, "/v1/admin/slack-installation", nil)
	if !h.proxySlackInstallationRoute(unknownSlack) {
		t.Fatal("unknown Slack environment state must retain Node ownership")
	}
	h.config.QM.SlackEnvironmentState = "absent"
	if h.proxySlackInstallationRoute(unknownSlack) {
		t.Fatal("declared Slack environment state did not permit Go ownership")
	}
	sandboxRoutes := httptest.NewRequest(http.MethodGet, "/v1/admin/sandbox-routes", nil)
	if !h.proxySandboxRoutes(sandboxRoutes) {
		t.Fatal("undeclared sandbox topology must retain Node ownership")
	}
	h.config.QM.SandboxDefaultBackend = "local"
	h.config.QM.SandboxBackends = []string{"local", "sprites"}
	if h.proxySandboxRoutes(sandboxRoutes) {
		t.Fatal("declared sandbox topology did not permit Go ownership")
	}
	if !h.owns(http.MethodGet, "/v1/deployments") || !h.owns(http.MethodGet, "/v1/deployments/deployment-1") {
		t.Fatal("anonymous deployment read routes are not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/deployments/deployment-1/name") || !h.owns(http.MethodPost, "/v1/deployments/deployment-1/display-name") || !h.owns(http.MethodPost, "/v1/deployments/deployment-1/share") {
		t.Fatal("durable deployment mutations are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/memory") || !h.owns(http.MethodPut, "/v1/memory") || !h.owns(http.MethodGet, "/v1/memory/history") || !h.owns(http.MethodPost, "/v1/memory/restore") {
		t.Fatal("source memory routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/apis") {
		t.Fatal("agent API discovery route is not Go-owned")
	}
	for _, path := range []string{"/v1/keychain/credentials", "/v1/keychain/overview", "/v1/keychain/grants", "/v1/keychain/asks"} {
		if !h.owns(http.MethodGet, path) {
			t.Fatalf("keychain metadata read %s is not Go-owned", path)
		}
	}
	if !h.owns(http.MethodDelete, "/v1/keychain/credentials/credential-1") {
		t.Fatal("keychain credential delete is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/keychain/grants/grant-1/revoke") {
		t.Fatal("keychain grant revoke is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/keychain/grants") {
		t.Fatal("keychain direct grant is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/connectors/oauth/revoke") {
		t.Fatal("OAuth connector revoke is not Go-owned")
	}
	for _, path := range []string{"/v1/connectors/oauth/status", "/v1/connectors/catalog"} {
		if !h.owns(http.MethodGet, path) {
			t.Fatalf("OAuth metadata route %s is not Go-owned", path)
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if !h.proxyOAuthCatalogRoute(request) {
			t.Fatalf("undeclared OAuth catalog ownership must proxy %s", path)
		}
	}
	h.config.QM.OAuthCatalogEnabled = true
	if h.proxyOAuthCatalogRoute(httptest.NewRequest(http.MethodGet, "/v1/connectors/catalog", nil)) || h.proxyOAuthCatalogRoute(httptest.NewRequest(http.MethodGet, "/v1/connectors/oauth/status", nil)) {
		t.Fatal("declared OAuth catalog did not permit Go ownership")
	}
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/connectors/token"},
		{http.MethodPost, "/v1/keychain/drops"},
		{http.MethodPost, "/v1/deployments/deployment-1/archive"},
		{http.MethodPost, "/v1/deployments/deployment-1/restore"},
		{http.MethodGet, "/v1/admin/scopes/personal:U1"},
		{http.MethodPut, "/v1/admin/scopes/personal:U1/soul"},
	} {
		if h.owns(route.method, route.path) {
			t.Fatalf("runtime-backed Node route was unexpectedly claimed by Go: %s %s", route.method, route.path)
		}
	}
	if !h.owns(http.MethodGet, "/v1/soul") || !h.owns(http.MethodPost, "/v1/soul") {
		t.Fatal("soul routes are not Go-owned")
	}
	capabilityMemory := httptest.NewRequest(http.MethodGet, "/v1/memory/history", nil)
	capabilityMemory.Header.Set(auth.CapabilityHeader, "capability")
	if h.proxyCapabilityMemoryRoute(capabilityMemory) {
		t.Fatal("capability memory history must use the Go shared-revision projection")
	}
	viewerDeployment := httptest.NewRequest(http.MethodGet, "/v1/deployments/deployment-1", nil)
	viewerDeployment.Header.Set(auth.CapabilityHeader, "capability")
	if !h.proxyViewerDeploymentRoute(viewerDeployment) {
		t.Fatal("identified deployment read must stay on Node")
	}
	anonymousDeployment := httptest.NewRequest(http.MethodGet, "/v1/deployments/deployment-1", nil)
	if h.proxyViewerDeploymentRoute(anonymousDeployment) {
		t.Fatal("anonymous deployment metadata read unexpectedly proxies")
	}
	if !h.owns(http.MethodPost, "/v1/turns/run-1/metrics") {
		t.Fatal("turn metrics route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/surface-context/context-1/result") {
		t.Fatal("surface context result route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/deliveries") {
		t.Fatal("pending deliveries route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/deliveries/delivery-1/ack") {
		t.Fatal("delivery ack route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/metrics") {
		t.Fatal("admin metrics route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/sessions") {
		t.Fatal("admin sessions route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/sessions/session-1/llm") {
		t.Fatal("admin session LLM route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/sessions/session-1") {
		t.Fatal("admin session detail route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/users") {
		t.Fatal("admin users route is not Go-owned")
	}
	if !h.owns(http.MethodPut, "/v1/admin/users/user-1/onboarding") {
		t.Fatal("admin user onboarding route is not Go-owned")
	}
	if h.owns(http.MethodPut, "/v1/admin/users/user-1/onboarding/extra") {
		t.Fatal("admin user onboarding route accepted an invalid suffix")
	}
	if !h.owns(http.MethodPost, "/v1/admin/users/user-1/reset") {
		t.Fatal("admin user reset route is not Go-owned")
	}
	if h.owns(http.MethodPost, "/v1/admin/users/user-1/reset/extra") {
		t.Fatal("admin user reset route accepted an invalid suffix")
	}
	if !h.owns(http.MethodGet, "/v1/admin/users/user-1") {
		t.Fatal("admin user detail route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/sessions/session-1") || !h.owns(http.MethodGet, "/v1/sessions/session-1/entries/0") {
		t.Fatal("viewer session read routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/sessions/session-1/entries/nope") {
		t.Fatal("invalid viewer entry route must stay Go-owned to return a compatibility error")
	}
	if !h.owns(http.MethodGet, "/v1/conversations/session-1") || !h.owns(http.MethodGet, "/v1/conversations") {
		t.Fatal("agent conversation read ownership is incorrect")
	}
	if !h.owns(http.MethodPost, "/v1/sessions/session-1") || !h.owns(http.MethodPost, "/v1/conversations/session-1") {
		t.Fatal("participant session view write routes are not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/contexts") {
		t.Fatal("contexts route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/scope-resources") {
		t.Fatal("scope resources route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/files") {
		t.Fatal("capability file metadata list route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/files/file-1/content") {
		t.Fatal("capability local file content route is not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/files/upload") {
		t.Fatal("local staged file upload route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/admin/files/read") || !h.owns(http.MethodGet, "/v1/admin/files/download") || !h.owns(http.MethodPost, "/v1/admin/files/upload") {
		t.Fatal("local administrator file byte routes are not Go-owned")
	}
	if !h.owns(http.MethodPost, "/v1/blobs") || !h.owns(http.MethodGet, "/v1/blobs/0123456789abcdef0123456789abcdef") {
		t.Fatal("local raw Blob routes are not Go-owned")
	}
	portalFileList := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	if !h.proxyFileListRoute(portalFileList) {
		t.Fatal("portal file list must remain on Node while portal identity is not migrated")
	}
	teamFileList := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	if !h.proxyFileListScope(auth.Identity{ScopeID: "team:T1"}, teamFileList) {
		t.Fatal("team capability file list must remain on Node until team membership is migrated")
	}
	contentWithoutStore := httptest.NewRequest(http.MethodGet, "/v1/files/file-1/content", nil)
	contentWithoutStore.Header.Set(auth.CapabilityHeader, "capability")
	if !h.proxyFileContentRoute(contentWithoutStore) {
		t.Fatal("file content must remain on Node without an explicitly shared local store")
	}
	h.config.QM.FileStore.Mode = "local"
	h.config.QM.FileStore.LocalDir = "/docstore"
	contentWithStore := httptest.NewRequest(http.MethodGet, "/v1/files/file-1/content", nil)
	contentWithStore.Header.Set(auth.CapabilityHeader, "capability")
	if h.proxyFileContentRoute(contentWithStore) {
		t.Fatal("configured local capability file content route did not cut over to Go")
	}
	uploadWithoutTransfer := httptest.NewRequest(http.MethodPost, "/v1/files/upload", nil)
	if !h.proxyFileUploadRoute(uploadWithoutTransfer) {
		t.Fatal("file upload must remain on Node without an explicitly shared transfer store")
	}
	h.config.QM.FileStore.TransferLocalDir = "/transfer"
	uploadWithTransfer := httptest.NewRequest(http.MethodPost, "/v1/files/upload", nil)
	if h.proxyFileUploadRoute(uploadWithTransfer) {
		t.Fatal("configured local staged file upload route did not cut over to Go")
	}
	adminReadWithoutStore := httptest.NewRequest(http.MethodGet, "/v1/admin/files/read?id=file-1", nil)
	h.config.QM.FileStore.Mode, h.config.QM.FileStore.LocalDir, h.config.QM.FileStore.TransferLocalDir = "", "", ""
	if !h.proxyAdminFileRoute(adminReadWithoutStore) {
		t.Fatal("administrator file read must remain on Node without shared local storage")
	}
	h.config.QM.FileStore.Mode, h.config.QM.FileStore.LocalDir, h.config.QM.FileStore.TransferLocalDir = "local", "/docstore", "/transfer"
	adminReadWithStore := httptest.NewRequest(http.MethodGet, "/v1/admin/files/read?id=file-1", nil)
	if h.proxyAdminFileRoute(adminReadWithStore) {
		t.Fatal("configured local administrator file read did not cut over to Go")
	}
	localBlob := httptest.NewRequest(http.MethodPost, "/v1/blobs", nil)
	if !h.localBlobRoute(localBlob) {
		t.Fatal("configured local raw Blob route did not cut over to Go")
	}
	if !h.owns(http.MethodPost, "/v1/grants") || !h.owns(http.MethodPost, "/v1/grants/revoke") {
		t.Fatal("grant routes are not Go-owned")
	}
	teamScopeResources := httptest.NewRequest(http.MethodGet, "/v1/scope-resources?principalId=U1&scope=team:T1", nil)
	if !h.proxyScopeResourcesRoute(teamScopeResources) {
		t.Fatal("team scope resources must stay on Node until team membership is migrated")
	}
	if !h.owns(http.MethodGet, "/v1/sessions/session-1/background") {
		t.Fatal("session background route is not Go-owned")
	}
	if !h.owns(http.MethodGet, "/v1/sessions/session-1/approvals") {
		t.Fatal("session approvals route is not Go-owned")
	}
}

func TestEnvironmentRouteCompatibility(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE environment_attachments,environments,audit_log"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.CapabilitySecret = "test-capability-secret"
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	h := &HTTPServer{config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, directory: data.NewDirectoryRepository(pg, cfg.QM.OrgID), acl: data.NewACLRepository(pg), environments: data.NewEnvironmentRepository(pg, cfg.QM.OrgID), channelPolicy: data.NewChannelPolicyRepository(pg, cfg.QM.OrgID), audit: data.NewAuditor(pg), auth: auth.Verifier{CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default()}
	create := httptest.NewRequest(http.MethodPost, "/v1/environments", bytes.NewBufferString(`{"name":"Shared Lab"}`))
	create.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	created := httptest.NewRecorder()
	h.serveOwned(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	attach := httptest.NewRequest(http.MethodPost, "/v1/environments/attach", bytes.NewBufferString(`{"name":"Shared Lab"}`))
	attach.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "bob", "personal:bob"))
	blocked := httptest.NewRecorder()
	h.serveOwned(blocked, attach)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("owner mediation=%d %s", blocked.Code, blocked.Body.String())
	}
	attach.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	attached := httptest.NewRecorder()
	h.serveOwned(attached, attach)
	if attached.Code != http.StatusOK {
		t.Fatalf("attach=%d %s", attached.Code, attached.Body.String())
	}
	list := httptest.NewRequest(http.MethodGet, "/v1/environments", nil)
	list.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "alice", "personal:alice"))
	listed := httptest.NewRecorder()
	h.serveOwned(listed, list)
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(`"attachedScopes":["personal:alice"]`)) {
		t.Fatalf("list=%d %s", listed.Code, listed.Body.String())
	}
}

func TestSurfaceCachePolicyRouteCompatibility(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE channel_policy_history,channel_policy,audit_log RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "surface-source-secret"
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	h := &HTTPServer{config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, directory: data.NewDirectoryRepository(pg, cfg.QM.OrgID), acl: data.NewACLRepository(pg), environments: data.NewEnvironmentRepository(pg, cfg.QM.OrgID), channelPolicy: data.NewChannelPolicyRepository(pg, cfg.QM.OrgID), audit: data.NewAuditor(pg), auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default()}
	body := []byte(`{"container":"C123","orders":"Summarize decisions","setBy":"U1"}`)
	set := httptest.NewRequest(http.MethodPost, "/v1/surface-cache/policy", bytes.NewReader(body))
	signSourceRequest(t, set, cfg.Auth.SourceSigningSecret, body)
	setResponse := httptest.NewRecorder()
	h.serveOwned(setResponse, set)
	if setResponse.Code != http.StatusOK || !bytes.Contains(setResponse.Body.Bytes(), []byte(`"orders":"Summarize decisions"`)) {
		t.Fatalf("set=%d %s", setResponse.Code, setResponse.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/surface-cache/policy?container=C123", nil)
	signSourceRequest(t, get, cfg.Auth.SourceSigningSecret, nil)
	getResponse := httptest.NewRecorder()
	h.serveOwned(getResponse, get)
	if getResponse.Code != http.StatusOK || !bytes.Contains(getResponse.Body.Bytes(), []byte(`"bots":{}`)) {
		t.Fatalf("get=%d %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestContextPolicyRouteCompatibility(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE channel_policy_history,channel_policy,audit_log,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "context-policy-source-secret"
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "U1", DisplayName: "Alice", Type: "internal"}}
	channels := []data.DirectoryChannel{{ChannelID: "C1", Name: "eng"}}
	channelMembers := []data.DirectoryPair{{ChannelID: "C1", PrincipalID: "U1"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members, Channels: &channels, ChannelMembers: &channelMembers}); err != nil {
		t.Fatal(err)
	}
	h := &HTTPServer{config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, directory: directory, acl: data.NewACLRepository(pg), environments: data.NewEnvironmentRepository(pg, cfg.QM.OrgID), channelPolicy: data.NewChannelPolicyRepository(pg, cfg.QM.OrgID), audit: data.NewAuditor(pg), auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default()}
	body := []byte(`{"principalId":"U1","scope":"channel:C1","orders":"Only escalate incidents","bots":{"status-bot":{"mode":"rollup","rollupHours":6}},"ambientEnabled":true}`)
	put := httptest.NewRequest(http.MethodPut, "/v1/contexts/policy", bytes.NewReader(body))
	signSourceRequest(t, put, cfg.Auth.SourceSigningSecret, body)
	putResponse := httptest.NewRecorder()
	h.serveOwned(putResponse, put)
	if putResponse.Code != http.StatusOK || !bytes.Contains(putResponse.Body.Bytes(), []byte(`"rollupHours":6`)) {
		t.Fatalf("put=%d %s", putResponse.Code, putResponse.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/contexts/policy?principalId=U1&scope=channel:C1", nil)
	signSourceRequest(t, get, cfg.Auth.SourceSigningSecret, nil)
	getResponse := httptest.NewRecorder()
	h.serveOwned(getResponse, get)
	if getResponse.Code != http.StatusOK || !bytes.Contains(getResponse.Body.Bytes(), []byte(`"ambientEnabled":true`)) {
		t.Fatalf("get=%d %s", getResponse.Code, getResponse.Body.String())
	}
	conflictBody := []byte(`{"principalId":"U1","scope":"channel:C1","orders":"stale","bots":{},"baseUpdatedAt":0}`)
	conflict := httptest.NewRequest(http.MethodPut, "/v1/contexts/policy", bytes.NewReader(conflictBody))
	signSourceRequest(t, conflict, cfg.Auth.SourceSigningSecret, conflictBody)
	conflictResponse := httptest.NewRecorder()
	h.serveOwned(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", conflictResponse.Code, conflictResponse.Body.String())
	}
}

func TestAdminDirectoryAndSlackMirrorReadRoutes(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE deliveries,turn_metrics,run_activity,run_signals,runs,session_llm_requests,session_entries,participants,sessions,projects,memory_revisions,error_events,credential_usage,egress_events,ambient_judgments,ack_emoji_picks,channel_messages,channel_files,channel_state,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,admin_grants,audit_log RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS session_leases(session_id TEXT PRIMARY KEY,token TEXT NOT NULL,expires_at BIGINT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS session_tape(session_id TEXT NOT NULL,seq INT NOT NULL,kind TEXT NOT NULL,payload TEXT NOT NULL,scope_label TEXT NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(session_id,seq))"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM session_leases"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM session_tape"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "admin-read-source-secret"
	cfg.Auth.CapabilitySecret = "admin-read-capability-secret"
	cfg.Auth.PortalIdentitySecret = "admin-read-portal-secret"
	fileStoreRoot := t.TempDir()
	transferStoreRoot := t.TempDir()
	cfg.QM.FileStore.Mode = "local"
	cfg.QM.FileStore.LocalDir = fileStoreRoot
	cfg.QM.FileStore.TransferLocalDir = transferStoreRoot
	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "U1", DisplayName: "Alice", Type: "internal", SlackID: "U1"}}
	channels := []data.DirectoryChannel{{ChannelID: "C1", Name: "eng"}}
	channelMembers := []data.DirectoryPair{{ChannelID: "C1", PrincipalID: "U1"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members, Channels: &channels, ChannelMembers: &channelMembers}); err != nil {
		t.Fatal(err)
	}
	if err := data.NewACLRepository(pg).PutAdminGrant(ctx, data.AdminGrant{PrincipalID: "admin", ScopeID: "org:acme", Role: "org_admin", GrantedBy: "bootstrap", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO channel_state(org_id,container,last_ts,oldest_ts,name,kind,members,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)", "acme", "C1", "2.0", "1.0", "eng", "channel", `["U1"]`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO channel_messages(org_id,container,ts,author_id,text,mentions,created_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)", "acme", "C1", "2.0", "U1", "deployment incident resolved", `{}`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var judgmentID, pickID int64
	if err := pg.Pool.QueryRow(ctx, "INSERT INTO ambient_judgments(org_id,surface,container,decision,reason,prompt,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id", "acme", "slack", "C1", "act", "incident needs a response", "full ambient prompt", time.Now().UnixMilli()).Scan(&judgmentID); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "INSERT INTO ack_emoji_picks(org_id,surface,channel,ts,outcome,picked,candidates,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id", "acme", "slack", "C1", "2.0", "picked", "eyes", "eyes,white_check_mark", time.Now().UnixMilli()).Scan(&pickID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO credential_usage(ts,slug,host,status,scope_label,principal_id) VALUES($1,$2,$3,$4,$5,$6)", time.Now().UnixMilli(), "openai", "api.openai.com", "ok", "project:p1", "U1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO egress_events(ts,source,host,allowed,scope_label,verdict) VALUES($1,$2,$3,$4,$5,$6)", time.Now().UnixMilli()+1, "firewall", "blocked.example", false, "project:p1", "denied"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO error_events(ts,scope_label,category,code,message,session_id) VALUES($1,$2,$3,$4,$5,$6)", time.Now().UnixMilli(), "project:p1", "runner", "lease_expired", "worker lease expired", "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sessions(id,type,scope_id,thread_ref,created_at) VALUES($1,$2,$3,$4,$5)", "session-1", "direct", "project:p1", "thread-1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sessions(id,type,scope_id,thread_ref,created_at,last_activity,messages,turns) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", "session-cron", "direct", "project:p1", "agent:main:cron:cron-2", time.Now().UnixMilli()-1, time.Now().UnixMilli(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sessions(id,type,scope_id,thread_ref,created_at,channel_name) VALUES($1,$2,$3,$4,$5,$6)", "session-channel", "channel", "channel:C1", "thread-channel", time.Now().UnixMilli(), "eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO participants(session_id,principal_id,valid_from) VALUES($1,$2,$3)", "session-1", "U1", int64(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO participants(session_id,principal_id,valid_from) VALUES($1,$2,$3)", "session-channel", "U1", int64(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sessions(id,type,scope_id,thread_ref,created_at) VALUES($1,$2,$3,$4,$5)", "session-reset", "direct", "personal:U1", "thread-reset", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO participants(session_id,principal_id,valid_from) VALUES($1,$2,$3)", "session-reset", "U1", int64(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_entries(session_id,seq,type,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-1", 1, "user", `{"text":"Need deployment help"}`, "project:p1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_entries(session_id,seq,type,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-1", 2, "assistant", `{"text":"I can help"}`, "project:p1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_entries(session_id,seq,type,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-1", 3, "soul", `{"text":"private system context"}`, "project:p1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_entries(session_id,seq,type,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-1", 4, "user", `{"text":"What is the rollback?"}`, "project:p1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_llm_requests(id,session_id,turn_seq,step,model,scope_label,request,truncated,created_at,ttft_ms,usage_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", "llm-1", "session-1", 1, 0, "gpt-test", "project:p1", `{"messages":[]}`, false, time.Now().UnixMilli(), 12, `{"input":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_entries(session_id,seq,type,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-reset", 1, "user", `{"text":"reset me"}`, "personal:U1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_llm_requests(id,session_id,turn_seq,step,model,scope_label,request,truncated,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", "llm-reset", "session-reset", 1, 0, "gpt-test", "personal:U1", `{"messages":[]}`, false, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_leases(session_id,token,expires_at) VALUES($1,$2,$3)", "session-reset", "lease-reset", time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO session_tape(session_id,seq,kind,payload,scope_label,created_at) VALUES($1,$2,$3,$4,$5,$6)", "session-reset", 1, "user", `{"text":"reset me"}`, "personal:U1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO runs(id,session_id,status,request,attempts,max_attempts,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)", "run-1", "thread-1", "pending", `{}`, 1, 3, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO turn_metrics(ts,scope_label,session_id,run_id,status,total_ms,cache_read,cache_write,uncached_input) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", time.Now().UnixMilli(), "project:p1", "session-1", "run-1", "done", 10, 5, 2000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deliveries(id,idempotency_key,destination,text,created_at) VALUES($1,$2,$3,$4,$5)", "delivery-1", "run:run-1", `{"type":"slack","target":"C1"}`, "pending reply", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deliveries(id,idempotency_key,destination,text,created_at) VALUES($1,$2,$3,$4,$5)", "delivery-claim", "claim:delivery-claim", `{"type":"slack","target":"C2"}`, "claim me", time.Now().UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deliveries(id,idempotency_key,destination,text,created_at) VALUES($1,$2,$3,$4,$5)", "delivery-principal", "principal:delivery-principal", `{"type":"principal","target":"U2"}`, "direct reply", time.Now().UnixMilli()+2); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deliveries(id,idempotency_key,destination,text,provenance,created_at,shadow) VALUES($1,$2,$3,$4,$5::jsonb,$6,TRUE)", "shadow-1", "cron:cron-1:slot-1", `{"type":"slack","target":"C1"}`, "shadow reply", `{"trigger":"cron","surface":"slack","fireKey":"cron:cron-1:slot-1","sourceScopeId":"project:p1","sourceThreadRef":"thread-1"}`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-1", []byte(`{"id":"cron-1","title":"Deploy reminder","ownerScopeId":"personal:U1","owner":"U1","createdBy":"U1","runAs":"scopeShared","enabled":true,"archived":false,"schedule":{"everyMs":60000},"destination":{"type":"principal","target":"U1"},"fireLog":[{"fireKey":"old"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-scope", []byte(`{"id":"cron-scope","title":"Scope reminder","ownerScopeId":"channel:C1","owner":"U1","createdBy":"U1","enabled":true,"archived":false,"schedule":{"everyMs":60000}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-cap-delete", []byte(`{"id":"cron-cap-delete","title":"Temporary reminder","ownerScopeId":"personal:U1","owner":"U1","createdBy":"U1","runAs":"scopeShared","enabled":true,"archived":false,"schedule":{"everyMs":60000}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-cap-patch", []byte(`{"id":"cron-cap-patch","title":"Original owner reminder","ownerScopeId":"personal:U1","owner":"U1","createdBy":"U1","enabled":true,"archived":false,"schedule":{"everyMs":60000}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-cap-retarget", []byte(`{"id":"cron-cap-retarget","title":"Retargetable reminder","ownerScopeId":"personal:U1","owner":"U1","createdBy":"U1","enabled":true,"archived":false,"schedule":{"everyMs":60000},"destination":{"type":"principal","target":"U1"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS process_sessions(process_id TEXT PRIMARY KEY,scope_id TEXT NOT NULL,kind TEXT NOT NULL,command TEXT NOT NULL,started_at BIGINT NOT NULL,expires_at BIGINT NOT NULL,status TEXT NOT NULL,session_ref TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM process_sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO process_sessions(process_id,scope_id,kind,command,started_at,expires_at,status,session_ref) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", "proc-1", "project:p1", "background", "tail -f deploy.log", time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), "running", "thread-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS monitors(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM monitors"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO monitors(id,json) VALUES($1,$2::jsonb)", "watch-1", fmt.Sprintf(`{"id":"watch-1","processId":"proc-1","command":"tail -f deploy.log","threadRef":"thread-1","enabled":true,"createdAt":%d,"expiresAt":%d,"pattern":"ERROR"}`, time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS approvals(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM approvals"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO approvals(id,json) VALUES($1,$2::jsonb)", "approval-1", `{"sessionId":"session-1","command":"deploy --prod","reason":"production confirmation","blocksInput":true,"kind":"approval","grantModes":["once"],"request":{"actor":{"externalId":"U1"}}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS file_artifacts(id TEXT PRIMARY KEY,kind TEXT NOT NULL DEFAULT 'file',owner_scope_id TEXT NOT NULL,path TEXT NOT NULL,name TEXT NOT NULL,mimetype TEXT NOT NULL,size_bytes BIGINT NOT NULL,blob_key TEXT,sha256 TEXT,direction TEXT NOT NULL,created_by TEXT NOT NULL,created_in_scope TEXT,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,enabled BOOLEAN NOT NULL DEFAULT TRUE,source TEXT NOT NULL DEFAULT 'live')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM file_artifacts"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO file_artifacts(id,owner_scope_id,path,name,mimetype,size_bytes,blob_key,direction,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", "file-1", "project:p1", "artifacts/file-1/report.txt", "report.txt", "text/plain", 42, "blob/file-1", "in", "U1", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO file_artifacts(id,owner_scope_id,path,name,mimetype,size_bytes,blob_key,direction,created_by,created_in_scope,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", "file-scope", "personal:U1", "artifacts/file-scope/plan.txt", "plan.txt", "text/plain", 11, "blob/file-scope", "in", "U1", "channel:C1", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO file_artifacts(id,owner_scope_id,path,name,mimetype,size_bytes,blob_key,direction,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", "file-personal", "personal:U1", "artifacts/file-personal/notes.txt", "notes.txt", "text/plain", 9, "blob/file-personal", "in", "U1", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	localBlobSHA := strings.Repeat("a", 64)
	localBlobBytes := []byte("Go reads the same Node docstore bytes.")
	if err := os.MkdirAll(filepath.Join(fileStoreRoot, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileStoreRoot, "files", localBlobSHA), localBlobBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO file_artifacts(id,owner_scope_id,path,name,mimetype,size_bytes,blob_key,direction,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", "file-local", "personal:U1", "artifacts/file-local/go-read.txt", "go read.txt", "text/plain", len(localBlobBytes), "files/"+localBlobSHA, "in", "U1", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO file_artifacts(id,owner_scope_id,path,name,mimetype,size_bytes,blob_key,direction,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", "file-shared", "personal:U2", "artifacts/file-shared/shared.txt", "shared.txt", "text/plain", 7, nil, "in", "U2", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := data.NewACLRepository(pg).Put(ctx, data.Grant{OwnerScopeID: "personal:U2", Path: "artifacts/file-shared/shared.txt", GranteeScopeID: "personal:U1", Permission: "read", GrantedBy: "U2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS deployments(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM deployments"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deployments(id,json) VALUES($1,$2::jsonb)", "deployment-1", `{"id":"deployment-1","ownerScopeId":"project:p1","createdBy":"U1","name":"incident-dashboard","displayName":"Incident dashboard","currentVersion":2,"appliedVersion":1,"status":"running","lastAccessAt":1234,"endpoint":{"host":"local","port":8080,"publicUrl":"https://deploy.example/dashboard?access=secret&x=1"},"versions":[{"version":1,"createdAt":1000,"commit":"abc"},{"version":2,"createdAt":2000,"parentCommit":"abc"}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO deployments(id,json) VALUES($1,$2::jsonb)", "deployment-scope", `{"id":"deployment-scope","ownerScopeId":"personal:U1","createdInScope":"channel:C1","createdBy":"U1","name":"scope-dashboard","currentVersion":1,"status":"running","versions":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM skill_packs"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM skills"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO skill_packs(id,json) VALUES($1,$2::jsonb)", "pack-1", `{"id":"pack-1","url":"https://github.com/acme/deploy-skills.git"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO skills(id,json) VALUES($1,$2::jsonb)", "skill-1", `{"id":"skill-1","scopeId":"project:p1","manifest":{"name":"deploy-check","description":"Check deployment status","requiredCapabilities":["read:deploy"],"body":"# Deploy check","files":[{"path":"scripts/check.sh","content":"echo safe","executable":true}]},"signature":"test","status":"published","createdBy":"U1","version":3,"grantedCapabilities":["read:deploy"],"approvals":["reviewed"],"createdAt":1000,"updatedAt":2000,"lastUsedAt":3000,"pack":{"packId":"pack-1","commit":"abc","upstreamName":"deploy-check"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO skills(id,json) VALUES($1,$2::jsonb)", "skill-scope", `{"id":"skill-scope","scopeId":"channel:C1","manifest":{"name":"scope-check","description":"Check the current scope","body":"# Scope check"},"status":"published","createdBy":"U1","version":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM soul_history"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM soul_configs"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO soul_configs(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "org:acme", `{"scopeId":"org:acme","content":"Org policy","version":4}`, "project:p1", `{"scopeId":"project:p1","content":"Project instruction","version":2}`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"security_postures", "command_policies", "egress_policies", "base_model_configs", "connector_clients"} {
		if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+"(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS context_requests(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM context_requests"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO context_requests(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "context-1", `{"id":"context-1","source":"slack","createdAt":1,"status":"pending","query":{"channelId":"C1","count":1}}`, "context-2", `{"id":"context-2","source":"slack","createdAt":1,"status":"pending","query":{"channelId":"C1","count":1}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO security_postures(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "org:acme", `{"scopeId":"org:acme","posture":"auto"}`, "personal:U1", `{"scopeId":"personal:U1","posture":"strict"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO command_policies(id,json) VALUES($1,$2::jsonb)", "personal:U1", `{"scopeId":"personal:U1","policy":{"mode":"denylist","rules":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO egress_policies(id,json) VALUES($1,$2::jsonb)", "personal:U1", `{"scopeId":"personal:U1","policy":{"allowedHosts":["api.example.com"],"deniedHosts":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO base_model_configs(id,json) VALUES($1,$2::jsonb)", "personal:U1", `{"scopeId":"personal:U1","modelId":"gpt-test"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO connector_clients(id,json) VALUES($1,$2::jsonb)", "personal:U1:github", `{"scopeId":"personal:U1","provider":"github","clientId":"client-id","secretEnc":"encrypted-secret","enabled":true,"updatedBy":"admin","updatedAt":1234}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS custom_model_providers(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM custom_model_providers"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO custom_model_providers(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "example-ai", `{"id":"example-ai","name":"Example AI","protocol":"openai","baseUrl":"https://models.example.test/v1","models":[{"id":"example-1","name":"Example One","contextWindow":64000}],"apiKeyEnc":"must-not-leak","disabled":false,"updatedAt":123,"updatedBy":"admin"}`, "disabled-ai", `{"id":"disabled-ai","name":"Disabled AI","protocol":"anthropic","baseUrl":"https://disabled.example.test","models":[{"id":"disabled-1"}],"apiKeyEnc":"disabled-key","disabled":true,"updatedAt":124,"updatedBy":"admin"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS slack_installation(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM slack_installation"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO slack_installation(id,json) VALUES($1,$2::jsonb)", "acme", `{"orgId":"acme","disabled":false,"botTokenEnc":"must-not-leak","appTokenEnc":"also-secret","teamId":"T1","teamName":"Acme","updatedAt":456,"updatedBy":"admin","version":"456:revision"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS sandbox_routing(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM sandbox_routing"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sandbox_routing(id,json) VALUES($1,$2::jsonb)", "personal:U1", `{"backend":"sprites","migratedAt":"2026-08-06T00:00:00.000Z","migrationSha":"abc123","capabilitiesLost":["stageOut"],"reason":"capacity"}`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"keychain_credentials", "keychain_grants", "keychain_asks"} {
		if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+"(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM durable_map_versions WHERE tbl IN ('keychain_credentials','keychain_grants','keychain_asks')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_credentials(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb),($5,$6::jsonb)", "credential-u1", `{"id":"credential-u1","ownerId":"U1","service":"github","kind":"env","envKey":"GITHUB_TOKEN","secretEnc":"must-not-leak","fingerprint":"sha","createdAt":1,"updatedAt":2}`, "credential-connector", `{"id":"credential-connector","ownerId":"U1","service":"slack.com","kind":"env","managed":"connector","secretEnc":"connector-secret","createdAt":1,"updatedAt":2}`, "credential-broker", `{"id":"credential-broker","ownerId":"org:acme","service":"deploy","kind":"broker","secretEnc":"broker-secret","createdAt":1,"updatedAt":2}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_grants(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "grant-active", `{"id":"grant-active","credentialId":"credential-u1","ownerId":"U1","audienceScopeId":"personal:U2","mode":"standing","purpose":"deploy","status":"active","createdAt":1,"usedBy":"U2"}`, "grant-expired", fmt.Sprintf(`{"id":"grant-expired","credentialId":"credential-u1","ownerId":"U1","audienceScopeId":"personal:U2","mode":"once","purpose":"old","status":"active","createdAt":1,"expiresAt":%d}`, time.Now().Add(-time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_asks(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "ask-pending", fmt.Sprintf(`{"id":"ask-pending","credentialId":"credential-u1","ownerId":"U1","requesterId":"U2","requesterScopeId":"personal:U2","purpose":"deploy","status":"pending","createdAt":2,"expiresAt":%d}`, time.Now().Add(time.Hour).UnixMilli()), "ask-expired", fmt.Sprintf(`{"id":"ask-expired","credentialId":"credential-u1","ownerId":"U1","requesterId":"U2","requesterScopeId":"personal:U2","purpose":"old","status":"pending","createdAt":1,"expiresAt":%d}`, time.Now().Add(-time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_asks(id,json) VALUES($1,$2::jsonb)", "ask-adoptable", fmt.Sprintf(`{"id":"ask-adoptable","credentialId":"credential-u1","ownerId":"U1","requesterId":"U3","requesterScopeId":"personal:U1","purpose":"ship the preview","status":"pending","createdAt":3,"expiresAt":%d}`, time.Now().Add(time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO credential_usage(ts,slug,host,status,scope_label,principal_id) VALUES($1,$2,$3,$4,$5,$6)", time.Now().UnixMilli(), "keychain:github:credential-u1", "api.github.com", "materialized", "personal:U1", "U1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO projects(id,json) VALUES($1,$2::jsonb)", "approval-project", `{"id":"approval-project","orgId":"acme","name":"Approval project","ownerId":"U1","memberIds":["U1"],"createdAt":1000,"updatedAt":1234}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO sessions(id,type,scope_id,thread_ref,created_at) VALUES($1,$2,$3,$4,$5)", "session-project-approval", "direct", "group:web-project-approval-project", "thread-project-approval", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO participants(session_id,principal_id,valid_from) VALUES($1,$2,$3)", "session-project-approval", "U1", int64(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO approvals(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb)", "approval-project-current", `{"sessionId":"session-project-approval","command":"deploy current","createdAt":2000,"blocksInput":true,"request":{"actor":{"externalId":"U1"},"scopeVersion":"1234"}}`, "approval-project-stale", `{"sessionId":"session-project-approval","command":"deploy stale","createdAt":2000,"blocksInput":true,"request":{"actor":{"externalId":"U1"},"scopeVersion":"1233"}}`); err != nil {
		t.Fatal(err)
	}
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	cfg.QM.SlackEnvironmentState = "absent"
	cfg.QM.SandboxDefaultBackend = "local"
	cfg.QM.SandboxBackends = []string{"local", "sprites"}
	cfg.QM.OAuthCatalogEnabled = true
	cfg.QM.OAuthConfiguredProviders = []string{"slack"}
	h := &HTTPServer{config: cfg, projects: biz.NewProjectUsecase(repo), projectRepo: repo, directory: directory, acl: data.NewACLRepository(pg), environments: data.NewEnvironmentRepository(pg, cfg.QM.OrgID), channelPolicy: data.NewChannelPolicyRepository(pg, cfg.QM.OrgID), surfaceCache: data.NewSurfaceCacheRepository(pg, cfg.QM.OrgID), ambientJudgments: data.NewAmbientJudgmentRepository(pg, cfg.QM.OrgID), ackEmojiPicks: data.NewAckEmojiPickRepository(pg, cfg.QM.OrgID), egress: data.NewEgressRepository(pg), errors: data.NewErrorEventRepository(pg), runs: data.NewRunRepository(pg), deliveries: data.NewDeliveryRepository(pg), sessions: data.NewSessionRepository(pg), replay: data.NewReplayRepository(pg), metrics: data.NewMetricsRepository(pg), retention: data.NewRetentionRepository(pg), crons: data.NewCronRepository(pg), files: data.NewFileArtifactRepository(pg), deployments: data.NewDeploymentRepository(pg), memory: data.NewMemoryRepository(pg), skills: data.NewSkillRepository(pg), souls: data.NewSoulRepository(pg), userConfig: data.NewUserConfigRepository(pg), contextRequests: data.NewContextRequestRepository(pg), keychain: data.NewKeychainStatusRepository(pg), customProviders: data.NewCustomProviderRepository(pg), slackInstallation: data.NewSlackInstallationRepository(pg), sandboxRoutes: data.NewSandboxRouteRepository(pg), audit: data.NewAuditor(pg), auth: auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool}, logger: slog.Default()}
	shareCapability := testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1")
	for _, test := range []struct {
		name, body, owner, ref, grantee, permission string
	}{
		{name: "file", body: `{"type":"file","id":"file-scope","toScope":"personal:U2"}`, owner: "personal:U1", ref: "artifacts/file-scope/plan.txt", grantee: "personal:U2", permission: "read"},
		{name: "cron channel", body: `{"type":"cron","id":"cron-1","toScope":"channel:C1","permission":"write"}`, owner: "personal:U1", ref: "cron:cron-1", grantee: "channel:C1", permission: "write"},
		{name: "deployment recipient", body: `{"type":"deploy","id":"deployment-scope","toScope":"Alice"}`, owner: "personal:U1", ref: "deployment:deployment-scope", grantee: "personal:U1", permission: "read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/share", bytes.NewBufferString(test.body))
			request.Header.Set(auth.CapabilityHeader, shareCapability)
			response := httptest.NewRecorder()
			h.serveOwned(response, request)
			if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"verb":"share"`)) || !bytes.Contains(response.Body.Bytes(), []byte(`"permission":"`+test.permission+`"`)) {
				t.Fatalf("share=%d %s", response.Code, response.Body.String())
			}
			grants, err := h.acl.List(ctx, test.owner, test.ref)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, grant := range grants {
				if grant.GranteeScopeID == test.grantee && grant.Permission == test.permission && grant.GrantedBy == "U1" {
					found = true
				}
			}
			if !found {
				t.Fatalf("share grants=%#v", grants)
			}
		})
	}
	shareDenied := httptest.NewRequest(http.MethodPost, "/v1/share", bytes.NewBufferString(`{"type":"file","id":"file-shared","toScope":"personal:U3"}`))
	shareDenied.Header.Set(auth.CapabilityHeader, shareCapability)
	shareDeniedResponse := httptest.NewRecorder()
	h.serveOwned(shareDeniedResponse, shareDenied)
	if shareDeniedResponse.Code != http.StatusForbidden || !bytes.Contains(shareDeniedResponse.Body.Bytes(), []byte(`only the file's owner`)) {
		t.Fatalf("transitive share=%d %s", shareDeniedResponse.Code, shareDeniedResponse.Body.String())
	}
	var artifactShareAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='grant' AND principal_id='U1'").Scan(&artifactShareAudits); err != nil || artifactShareAudits != 3 {
		t.Fatalf("artifact share audits=%d err=%v", artifactShareAudits, err)
	}
	oauthCatalogRequest := httptest.NewRequest(http.MethodGet, "/v1/connectors/catalog", nil)
	signSourceRequest(t, oauthCatalogRequest, cfg.Auth.SourceSigningSecret, nil)
	oauthCatalogResponse := httptest.NewRecorder()
	h.ServeHTTP(oauthCatalogResponse, oauthCatalogRequest)
	if oauthCatalogResponse.Code != http.StatusOK || !bytes.Contains(oauthCatalogResponse.Body.Bytes(), []byte(`"provider":"slack"`)) || !bytes.Contains(oauthCatalogResponse.Body.Bytes(), []byte(`"configured":true`)) || !bytes.Contains(oauthCatalogResponse.Body.Bytes(), []byte(`"provider":"github"`)) || bytes.Contains(oauthCatalogResponse.Body.Bytes(), []byte("CLIENT_SECRET")) {
		t.Fatalf("OAuth catalog=%d %s", oauthCatalogResponse.Code, oauthCatalogResponse.Body.String())
	}
	oauthStatusRequest := httptest.NewRequest(http.MethodGet, "/v1/connectors/oauth/status?principalId=U1", nil)
	signSourceRequest(t, oauthStatusRequest, cfg.Auth.SourceSigningSecret, nil)
	oauthStatusResponse := httptest.NewRecorder()
	h.ServeHTTP(oauthStatusResponse, oauthStatusRequest)
	if oauthStatusResponse.Code != http.StatusOK || !bytes.Contains(oauthStatusResponse.Body.Bytes(), []byte(`"slack"`)) || !bytes.Contains(oauthStatusResponse.Body.Bytes(), []byte(`"connected":true`)) || !bytes.Contains(oauthStatusResponse.Body.Bytes(), []byte(`"available":true`)) || bytes.Contains(oauthStatusResponse.Body.Bytes(), []byte("connector-secret")) {
		t.Fatalf("OAuth status=%d %s", oauthStatusResponse.Code, oauthStatusResponse.Body.String())
	}
	sessionCapRequest := httptest.NewRequest(http.MethodPost, "/v1/session-cap", nil)
	sessionCapRequest.Header.Set("x-portal-identity", testPortalIdentityToken(t, cfg.Auth.PortalIdentitySecret, "U1"))
	signSourceRequest(t, sessionCapRequest, cfg.Auth.SourceSigningSecret, nil)
	sessionCapResponse := httptest.NewRecorder()
	h.serveOwned(sessionCapResponse, sessionCapRequest)
	if sessionCapResponse.Code != http.StatusOK {
		t.Fatalf("session cap=%d %s", sessionCapResponse.Code, sessionCapResponse.Body.String())
	}
	var capBody struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(sessionCapResponse.Body.Bytes(), &capBody); err != nil || capBody.Token == "" {
		t.Fatalf("session cap body=%s err=%v", sessionCapResponse.Body.String(), err)
	}
	capReq := httptest.NewRequest(http.MethodGet, "/v1/conversations/session-1", nil)
	capReq.Header.Set(auth.CapabilityHeader, capBody.Token)
	if _, err := h.auth.Authenticate(ctx, capReq, nil, "either"); err != nil {
		t.Fatalf("session cap token is not accepted by Go verifier: %v", err)
	}
	keychainCredentialsRequest := httptest.NewRequest(http.MethodGet, "/v1/keychain/credentials", nil)
	keychainCredentialsRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	keychainCredentialsResponse := httptest.NewRecorder()
	h.serveOwned(keychainCredentialsResponse, keychainCredentialsRequest)
	if keychainCredentialsResponse.Code != http.StatusOK || !bytes.Contains(keychainCredentialsResponse.Body.Bytes(), []byte(`"id":"credential-u1"`)) || bytes.Contains(keychainCredentialsResponse.Body.Bytes(), []byte("must-not-leak")) || bytes.Contains(keychainCredentialsResponse.Body.Bytes(), []byte(`"id":"credential-connector"`)) {
		t.Fatalf("keychain credentials=%d %s", keychainCredentialsResponse.Code, keychainCredentialsResponse.Body.String())
	}
	keychainOverviewRequest := httptest.NewRequest(http.MethodGet, "/v1/keychain/overview", nil)
	keychainOverviewRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	keychainOverviewResponse := httptest.NewRecorder()
	h.serveOwned(keychainOverviewResponse, keychainOverviewRequest)
	if keychainOverviewResponse.Code != http.StatusOK || !bytes.Contains(keychainOverviewResponse.Body.Bytes(), []byte(`"id":"credential-u1"`)) || !bytes.Contains(keychainOverviewResponse.Body.Bytes(), []byte(`"credentialId":"credential-connector"`)) || !bytes.Contains(keychainOverviewResponse.Body.Bytes(), []byte(`"credentialId":"credential-u1"`)) || !bytes.Contains(keychainOverviewResponse.Body.Bytes(), []byte(`"scopeNames":{"personal:U1":"Alice"}`)) || bytes.Contains(keychainOverviewResponse.Body.Bytes(), []byte("must-not-leak")) {
		t.Fatalf("keychain overview=%d %s", keychainOverviewResponse.Code, keychainOverviewResponse.Body.String())
	}
	keychainGrantsRequest := httptest.NewRequest(http.MethodGet, "/v1/keychain/grants", nil)
	keychainGrantsRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	keychainGrantsResponse := httptest.NewRecorder()
	h.serveOwned(keychainGrantsResponse, keychainGrantsRequest)
	if keychainGrantsResponse.Code != http.StatusOK || !bytes.Contains(keychainGrantsResponse.Body.Bytes(), []byte(`"id":"grant-active"`)) {
		t.Fatalf("keychain grants=%d %s", keychainGrantsResponse.Code, keychainGrantsResponse.Body.String())
	}
	keychainAsksRequest := httptest.NewRequest(http.MethodGet, "/v1/keychain/asks", nil)
	keychainAsksRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	keychainAsksResponse := httptest.NewRecorder()
	h.serveOwned(keychainAsksResponse, keychainAsksRequest)
	if keychainAsksResponse.Code != http.StatusOK || !bytes.Contains(keychainAsksResponse.Body.Bytes(), []byte(`"id":"ask-pending"`)) {
		t.Fatalf("keychain asks=%d %s", keychainAsksResponse.Code, keychainAsksResponse.Body.String())
	}
	directGrantBody := []byte(`{"credential":"credential-u1","mode":"standing","purpose":"ship the preview","expiresAt":1900000000}`)
	directGrantRequest := httptest.NewRequest(http.MethodPost, "/v1/keychain/grants", bytes.NewReader(directGrantBody))
	directGrantRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	directGrantResponse := httptest.NewRecorder()
	h.ServeHTTP(directGrantResponse, directGrantRequest)
	if directGrantResponse.Code != http.StatusOK || !bytes.Contains(directGrantResponse.Body.Bytes(), []byte(`"credentialId":"credential-u1"`)) || !bytes.Contains(directGrantResponse.Body.Bytes(), []byte(`"expiresAt":1900000000000`)) || !bytes.Contains(directGrantResponse.Body.Bytes(), []byte(`"command":"curl -fsS`)) {
		t.Fatalf("keychain direct grant=%d %s", directGrantResponse.Code, directGrantResponse.Body.String())
	}
	var directGrant struct {
		Grant struct {
			ID string `json:"id"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(directGrantResponse.Body.Bytes(), &directGrant); err != nil || directGrant.Grant.ID == "" {
		t.Fatalf("direct grant payload=%s err=%v", directGrantResponse.Body.String(), err)
	}
	var adoptedAskStatus, adoptedAskGrantID string
	if err := pg.Pool.QueryRow(ctx, "SELECT json->>'status',json->>'grantId' FROM keychain_asks WHERE id='ask-adoptable'").Scan(&adoptedAskStatus, &adoptedAskGrantID); err != nil || adoptedAskStatus != "approved" || adoptedAskGrantID != directGrant.Grant.ID {
		t.Fatalf("adopted ask status=%q grant=%q err=%v", adoptedAskStatus, adoptedAskGrantID, err)
	}
	triggeredToken, err := auth.MintCapability(auth.Claims{ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", Triggered: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	triggeredGrantRequest := httptest.NewRequest(http.MethodPost, "/v1/keychain/grants", bytes.NewReader(directGrantBody))
	triggeredGrantRequest.Header.Set(auth.CapabilityHeader, triggeredToken)
	triggeredGrantResponse := httptest.NewRecorder()
	h.ServeHTTP(triggeredGrantResponse, triggeredGrantRequest)
	if triggeredGrantResponse.Code != http.StatusForbidden || !bytes.Contains(triggeredGrantResponse.Body.Bytes(), []byte(`consent can only be recorded`)) {
		t.Fatalf("triggered keychain grant=%d %s", triggeredGrantResponse.Code, triggeredGrantResponse.Body.String())
	}
	sourceKeychainRequest := httptest.NewRequest(http.MethodGet, "/v1/keychain/credentials", nil)
	signSourceRequest(t, sourceKeychainRequest, cfg.Auth.SourceSigningSecret, nil)
	sourceKeychainResponse := httptest.NewRecorder()
	h.serveOwned(sourceKeychainResponse, sourceKeychainRequest)
	if sourceKeychainResponse.Code != http.StatusUnauthorized || !bytes.Contains(sourceKeychainResponse.Body.Bytes(), []byte(`keychain routes require an agent capability token`)) {
		t.Fatalf("source keychain read=%d %s", sourceKeychainResponse.Code, sourceKeychainResponse.Body.String())
	}
	apiDiscoveryRequest := httptest.NewRequest(http.MethodGet, "/v1/apis", nil)
	apiDiscoveryRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	apiDiscoveryResponse := httptest.NewRecorder()
	h.serveOwned(apiDiscoveryResponse, apiDiscoveryRequest)
	if apiDiscoveryResponse.Code != http.StatusOK || !bytes.Contains(apiDiscoveryResponse.Body.Bytes(), []byte(`"path":"/v1/skills"`)) || !bytes.Contains(apiDiscoveryResponse.Body.Bytes(), []byte(`"path":"/v1/admin/whoami"`)) || bytes.Contains(apiDiscoveryResponse.Body.Bytes(), []byte(`"path":"/v1/admin/scopes"`)) {
		t.Fatalf("regular API discovery=%d %s", apiDiscoveryResponse.Code, apiDiscoveryResponse.Body.String())
	}
	adminCapability, err := auth.MintCapability(auth.Claims{ActorID: "admin", ScopeID: "personal:admin", Audience: "control-plane", LiveActor: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	adminAPIDiscoveryRequest := httptest.NewRequest(http.MethodGet, "/v1/apis", nil)
	adminAPIDiscoveryRequest.Header.Set(auth.CapabilityHeader, adminCapability)
	adminAPIDiscoveryResponse := httptest.NewRecorder()
	h.serveOwned(adminAPIDiscoveryResponse, adminAPIDiscoveryRequest)
	if adminAPIDiscoveryResponse.Code != http.StatusOK || !bytes.Contains(adminAPIDiscoveryResponse.Body.Bytes(), []byte(`"path":"/v1/admin/scopes"`)) || bytes.Count(adminAPIDiscoveryResponse.Body.Bytes(), []byte(`"path":"/v1/admin/whoami"`)) != 1 {
		t.Fatalf("admin API discovery=%d %s", adminAPIDiscoveryResponse.Code, adminAPIDiscoveryResponse.Body.String())
	}
	var apiDiscoveryAuditCount int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log WHERE action='apis.list'").Scan(&apiDiscoveryAuditCount); err != nil || apiDiscoveryAuditCount != 2 {
		t.Fatalf("API discovery audit count=%d err=%v", apiDiscoveryAuditCount, err)
	}
	soulRequest := httptest.NewRequest(http.MethodGet, "/v1/soul?scopeId=project:p1", nil)
	signSourceRequest(t, soulRequest, cfg.Auth.SourceSigningSecret, nil)
	soulResponse := httptest.NewRecorder()
	h.serveOwned(soulResponse, soulRequest)
	if soulResponse.Code != http.StatusOK || !bytes.Contains(soulResponse.Body.Bytes(), []byte(`"soul":"Project instruction"`)) || !bytes.Contains(soulResponse.Body.Bytes(), []byte(`"soulVersion":2`)) || !bytes.Contains(soulResponse.Body.Bytes(), []byte(`"orgSoul":"Org policy"`)) || !bytes.Contains(soulResponse.Body.Bytes(), []byte(`Lower-scope instructions`)) {
		t.Fatalf("soul=%d %s", soulResponse.Code, soulResponse.Body.String())
	}
	soulPostBody := []byte(`{"scopeId":"personal:U1","content":"Personal instruction","actorId":"U1"}`)
	soulPost := httptest.NewRequest(http.MethodPost, "/v1/soul", bytes.NewReader(soulPostBody))
	signSourceRequest(t, soulPost, cfg.Auth.SourceSigningSecret, soulPostBody)
	soulPostResponse := httptest.NewRecorder()
	h.serveOwned(soulPostResponse, soulPost)
	if soulPostResponse.Code != http.StatusOK || !bytes.Contains(soulPostResponse.Body.Bytes(), []byte(`"version":1`)) {
		t.Fatalf("soul post=%d %s", soulPostResponse.Code, soulPostResponse.Body.String())
	}
	var savedSoul struct {
		Content string              `json:"content"`
		Version int                 `json:"version"`
		History []data.SoulRevision `json:"history"`
	}
	var savedSoulRaw []byte
	if err := pg.Pool.QueryRow(ctx, "SELECT json FROM soul_configs WHERE id=$1", "personal:U1").Scan(&savedSoulRaw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(savedSoulRaw, &savedSoul); err != nil || savedSoul.Content != "Personal instruction" || savedSoul.Version != 1 || len(savedSoul.History) != 1 || savedSoul.History[0].UpdatedBy != "U1" {
		t.Fatalf("saved soul=%s parsed=%#v err=%v", savedSoulRaw, savedSoul, err)
	}
	var soulMapVersion int
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='soul_configs'").Scan(&soulMapVersion); err != nil || soulMapVersion < 1 {
		t.Fatalf("soul durable version=%d err=%v", soulMapVersion, err)
	}
	soulDeniedBody := []byte(`{"scopeId":"personal:U2","content":"nope","actorId":"U1"}`)
	soulDenied := httptest.NewRequest(http.MethodPost, "/v1/soul?case=denied", bytes.NewReader(soulDeniedBody))
	signSourceRequest(t, soulDenied, cfg.Auth.SourceSigningSecret, soulDeniedBody)
	soulDeniedResponse := httptest.NewRecorder()
	h.serveOwned(soulDeniedResponse, soulDenied)
	if soulDeniedResponse.Code != http.StatusForbidden || !bytes.Contains(soulDeniedResponse.Body.Bytes(), []byte(`"error":"soul_update_denied"`)) {
		t.Fatalf("soul denied=%d %s", soulDeniedResponse.Code, soulDeniedResponse.Body.String())
	}
	scopeResourcesRequest := httptest.NewRequest(http.MethodGet, "/v1/scope-resources?principalId=U1&scope=channel:C1", nil)
	signSourceRequest(t, scopeResourcesRequest, cfg.Auth.SourceSigningSecret, nil)
	scopeResourcesResponse := httptest.NewRecorder()
	h.serveOwned(scopeResourcesResponse, scopeResourcesRequest)
	if scopeResourcesResponse.Code != http.StatusOK || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"id":"file-scope"`)) || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"id":"cron-scope"`)) || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"id":"deployment-scope"`)) || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"permission":"write"`)) || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"id":"skill-scope"`)) || !bytes.Contains(scopeResourcesResponse.Body.Bytes(), []byte(`"manageable":false`)) {
		t.Fatalf("scope resources=%d %s", scopeResourcesResponse.Code, scopeResourcesResponse.Body.String())
	}
	scopeResourcesDenied := httptest.NewRequest(http.MethodGet, "/v1/scope-resources?principalId=U1&scope=personal:U2", nil)
	signSourceRequest(t, scopeResourcesDenied, cfg.Auth.SourceSigningSecret, nil)
	scopeResourcesDeniedResponse := httptest.NewRecorder()
	h.serveOwned(scopeResourcesDeniedResponse, scopeResourcesDenied)
	if scopeResourcesDeniedResponse.Code != http.StatusNotFound || !bytes.Contains(scopeResourcesDeniedResponse.Body.Bytes(), []byte(`"not a context you can see"`)) {
		t.Fatalf("scope resources denied=%d %s", scopeResourcesDeniedResponse.Code, scopeResourcesDeniedResponse.Body.String())
	}
	fileListRequest := httptest.NewRequest(http.MethodGet, "/v1/files?limit=1", nil)
	fileListRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	fileListResponse := httptest.NewRecorder()
	h.serveOwned(fileListResponse, fileListRequest)
	if fileListResponse.Code != http.StatusOK || !bytes.Contains(fileListResponse.Body.Bytes(), []byte(`"owned":[`)) || !bytes.Contains(fileListResponse.Body.Bytes(), []byte(`"shared":[{"createdAt":`)) || !bytes.Contains(fileListResponse.Body.Bytes(), []byte(`"id":"file-shared"`)) || !bytes.Contains(fileListResponse.Body.Bytes(), []byte(`"openable":false`)) || !bytes.Contains(fileListResponse.Body.Bytes(), []byte(`"nextCursor":`)) {
		t.Fatalf("capability file list=%d %s", fileListResponse.Code, fileListResponse.Body.String())
	}
	fractionalFileListRequest := httptest.NewRequest(http.MethodGet, "/v1/files?limit=0.5", nil)
	fractionalFileListRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	fractionalFileListResponse := httptest.NewRecorder()
	h.serveOwned(fractionalFileListResponse, fractionalFileListRequest)
	if fractionalFileListResponse.Code != http.StatusOK || !bytes.Contains(fractionalFileListResponse.Body.Bytes(), []byte(`"owned":[]`)) || bytes.Contains(fractionalFileListResponse.Body.Bytes(), []byte(`"nextCursor":`)) {
		t.Fatalf("fractional capability file list=%d %s", fractionalFileListResponse.Code, fractionalFileListResponse.Body.String())
	}
	fileContentRequest := httptest.NewRequest(http.MethodGet, "/v1/files/file-local/content", nil)
	fileContentRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	fileContentResponse := httptest.NewRecorder()
	h.serveOwned(fileContentResponse, fileContentRequest)
	if fileContentResponse.Code != http.StatusOK || !bytes.Equal(fileContentResponse.Body.Bytes(), localBlobBytes) || fileContentResponse.Header().Get("content-type") != "text/plain" || fileContentResponse.Header().Get("content-disposition") != "inline; filename*=UTF-8''go%20read.txt" {
		t.Fatalf("capability file content=%d headers=%v body=%q", fileContentResponse.Code, fileContentResponse.Header(), fileContentResponse.Body.Bytes())
	}
	stagedUploadID := strings.Repeat("b", 32)
	stagedUploadBytes := []byte("staged Node blob becomes a Go durable artifact")
	if err := os.WriteFile(filepath.Join(transferStoreRoot, stagedUploadID), stagedUploadBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	uploadBody := []byte(`{"principalId":"U1","blobId":"` + stagedUploadID + `","name":"nested\\report.md","scopeId":"channel:C1"}`)
	uploadRequest := httptest.NewRequest(http.MethodPost, "/v1/files/upload", bytes.NewReader(uploadBody))
	signSourceRequest(t, uploadRequest, cfg.Auth.SourceSigningSecret, uploadBody)
	uploadResponse := httptest.NewRecorder()
	h.serveOwned(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusOK || !bytes.Contains(uploadResponse.Body.Bytes(), []byte(`"name":"report.md"`)) || !bytes.Contains(uploadResponse.Body.Bytes(), []byte(`"createdInScope":"channel:C1"`)) {
		t.Fatalf("local staged upload=%d %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var uploaded struct {
		File struct {
			ID string `json:"id"`
		} `json:"file"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &uploaded); err != nil || uploaded.File.ID == "" {
		t.Fatalf("uploaded response=%s parsed=%#v err=%v", uploadResponse.Body.String(), uploaded, err)
	}
	var uploadedPath, uploadedBlobKey string
	if err := pg.Pool.QueryRow(ctx, "SELECT path,blob_key FROM file_artifacts WHERE id=$1", uploaded.File.ID).Scan(&uploadedPath, &uploadedBlobKey); err != nil {
		t.Fatal(err)
	}
	writtenUpload, err := os.ReadFile(filepath.Join(fileStoreRoot, "files", strings.TrimPrefix(uploadedBlobKey, "files/")))
	if err != nil || !bytes.Equal(writtenUpload, stagedUploadBytes) {
		t.Fatalf("durable upload bytes=%q err=%v", writtenUpload, err)
	}
	if _, err := os.Stat(filepath.Join(transferStoreRoot, stagedUploadID)); !os.IsNotExist(err) {
		t.Fatalf("staged upload was not removed: %v", err)
	}
	uploadGrants, err := h.acl.List(ctx, "personal:U1", uploadedPath)
	if err != nil || len(uploadGrants) != 1 || uploadGrants[0].GranteeScopeID != "channel:C1" || uploadGrants[0].Permission != "read" {
		t.Fatalf("uploaded file ACL=%#v err=%v", uploadGrants, err)
	}
	uploadedContentRequest := httptest.NewRequest(http.MethodGet, "/v1/files/"+uploaded.File.ID+"/content", nil)
	uploadedContentRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	uploadedContentResponse := httptest.NewRecorder()
	h.serveOwned(uploadedContentResponse, uploadedContentRequest)
	if uploadedContentResponse.Code != http.StatusOK || !bytes.Equal(uploadedContentResponse.Body.Bytes(), stagedUploadBytes) || uploadedContentResponse.Header().Get("content-type") != "text/markdown" {
		t.Fatalf("uploaded Go file content=%d headers=%v body=%q", uploadedContentResponse.Code, uploadedContentResponse.Header(), uploadedContentResponse.Body.Bytes())
	}
	adminBlobID := strings.Repeat("c", 32)
	adminBlobBytes := []byte("administrator staged file\n")
	if err := os.WriteFile(filepath.Join(transferStoreRoot, adminBlobID), adminBlobBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	adminUploadBody := []byte(`{"blobId":"` + adminBlobID + `","name":"admin notes.txt","mimetype":"text/plain"}`)
	adminUploadRequest := httptest.NewRequest(http.MethodPost, "/v1/admin/files/upload?scope=personal:U1", bytes.NewReader(adminUploadBody))
	adminUploadRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminUploadRequest, cfg.Auth.SourceSigningSecret, adminUploadBody)
	adminUploadResponse := httptest.NewRecorder()
	h.serveOwned(adminUploadResponse, adminUploadRequest)
	if adminUploadResponse.Code != http.StatusOK || !bytes.Contains(adminUploadResponse.Body.Bytes(), []byte(`"openable":true`)) {
		t.Fatalf("admin local upload=%d %s", adminUploadResponse.Code, adminUploadResponse.Body.String())
	}
	var adminUploaded struct {
		File struct {
			ID string `json:"id"`
		} `json:"file"`
	}
	if err := json.Unmarshal(adminUploadResponse.Body.Bytes(), &adminUploaded); err != nil || adminUploaded.File.ID == "" {
		t.Fatalf("admin uploaded response=%s parsed=%#v err=%v", adminUploadResponse.Body.String(), adminUploaded, err)
	}
	if _, err := os.Stat(filepath.Join(transferStoreRoot, adminBlobID)); !os.IsNotExist(err) {
		t.Fatalf("admin staged blob was not removed: %v", err)
	}
	adminReadRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/files/read?id="+adminUploaded.File.ID, nil)
	adminReadRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminReadRequest, cfg.Auth.SourceSigningSecret, nil)
	adminReadResponse := httptest.NewRecorder()
	h.serveOwned(adminReadResponse, adminReadRequest)
	if adminReadResponse.Code != http.StatusOK || !bytes.Contains(adminReadResponse.Body.Bytes(), []byte(`"content":"administrator staged file\n"`)) || !bytes.Contains(adminReadResponse.Body.Bytes(), []byte(`"truncated":false`)) {
		t.Fatalf("admin local read=%d %s", adminReadResponse.Code, adminReadResponse.Body.String())
	}
	adminDownloadRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/files/download?id="+adminUploaded.File.ID, nil)
	adminDownloadRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminDownloadRequest, cfg.Auth.SourceSigningSecret, nil)
	adminDownloadResponse := httptest.NewRecorder()
	h.serveOwned(adminDownloadResponse, adminDownloadRequest)
	if adminDownloadResponse.Code != http.StatusOK || !bytes.Equal(adminDownloadResponse.Body.Bytes(), adminBlobBytes) || adminDownloadResponse.Header().Get("content-disposition") != `inline; filename="admin notes.txt"; filename*=UTF-8''admin%20notes.txt` || adminDownloadResponse.Header().Get("content-security-policy") != "sandbox" {
		t.Fatalf("admin local download=%d headers=%v body=%q", adminDownloadResponse.Code, adminDownloadResponse.Header(), adminDownloadResponse.Body.Bytes())
	}
	grantBody := []byte(`{"ownerScopeId":"personal:U1","ref":"deployment:deployment-scope","granteeScopeId":"personal:U2","permission":"write","grantedBy":"U1"}`)
	grantRequest := httptest.NewRequest(http.MethodPost, "/v1/grants", bytes.NewReader(grantBody))
	signSourceRequest(t, grantRequest, cfg.Auth.SourceSigningSecret, grantBody)
	grantResponse := httptest.NewRecorder()
	h.serveOwned(grantResponse, grantRequest)
	if grantResponse.Code != http.StatusOK {
		t.Fatalf("grant=%d %s", grantResponse.Code, grantResponse.Body.String())
	}
	grants, err := h.acl.List(ctx, "personal:U1", "deployment:deployment-scope")
	if err != nil || len(grants) != 1 || grants[0].GranteeScopeID != "personal:U2" || grants[0].Permission != "write" {
		t.Fatalf("grants=%#v err=%v", grants, err)
	}
	grantDeniedBody := []byte(`{"ownerScopeId":"personal:U1","ref":"deployment:deployment-scope","granteeScopeId":"personal:U3","permission":"read","grantedBy":"U2"}`)
	grantDeniedRequest := httptest.NewRequest(http.MethodPost, "/v1/grants?case=denied", bytes.NewReader(grantDeniedBody))
	signSourceRequest(t, grantDeniedRequest, cfg.Auth.SourceSigningSecret, grantDeniedBody)
	grantDeniedResponse := httptest.NewRecorder()
	h.serveOwned(grantDeniedResponse, grantDeniedRequest)
	if grantDeniedResponse.Code != http.StatusBadRequest || !bytes.Contains(grantDeniedResponse.Body.Bytes(), []byte(`"error":"grant_failed"`)) {
		t.Fatalf("grant denied=%d %s", grantDeniedResponse.Code, grantDeniedResponse.Body.String())
	}
	revokeBody := []byte(`{"ownerScopeId":"personal:U1","ref":"deployment:deployment-scope","granteeScopeId":"personal:U2","revokedBy":"U1"}`)
	revokeRequest := httptest.NewRequest(http.MethodPost, "/v1/grants/revoke", bytes.NewReader(revokeBody))
	signSourceRequest(t, revokeRequest, cfg.Auth.SourceSigningSecret, revokeBody)
	revokeResponse := httptest.NewRecorder()
	h.serveOwned(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusOK {
		t.Fatalf("revoke=%d %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	grants, err = h.acl.List(ctx, "personal:U1", "deployment:deployment-scope")
	if err != nil || len(grants) != 0 {
		t.Fatalf("grants after revoke=%#v err=%v", grants, err)
	}
	memoryGet := httptest.NewRequest(http.MethodGet, "/v1/memory?principalId=U1", nil)
	signSourceRequest(t, memoryGet, cfg.Auth.SourceSigningSecret, nil)
	memoryGetResponse := httptest.NewRecorder()
	h.serveOwned(memoryGetResponse, memoryGet)
	if memoryGetResponse.Code != http.StatusOK || !bytes.Contains(memoryGetResponse.Body.Bytes(), []byte(`"revision":"0"`)) {
		t.Fatalf("empty memory=%d %s", memoryGetResponse.Code, memoryGetResponse.Body.String())
	}
	memoryPutBody := []byte(`{"principalId":"U1","content":"first plan  \n\n"}`)
	memoryPut := httptest.NewRequest(http.MethodPut, "/v1/memory", bytes.NewReader(memoryPutBody))
	signSourceRequest(t, memoryPut, cfg.Auth.SourceSigningSecret, memoryPutBody)
	memoryPutResponse := httptest.NewRecorder()
	h.serveOwned(memoryPutResponse, memoryPut)
	if memoryPutResponse.Code != http.StatusOK || !bytes.Contains(memoryPutResponse.Body.Bytes(), []byte(`"revision":"1"`)) || !bytes.Contains(memoryPutResponse.Body.Bytes(), []byte(`"content":"first plan\n"`)) {
		t.Fatalf("memory put=%d %s", memoryPutResponse.Code, memoryPutResponse.Body.String())
	}
	secondMemoryBody := []byte(`{"principalId":"U1","content":"second plan"}`)
	secondMemory := httptest.NewRequest(http.MethodPut, "/v1/memory", bytes.NewReader(secondMemoryBody))
	signSourceRequest(t, secondMemory, cfg.Auth.SourceSigningSecret, secondMemoryBody)
	secondMemoryResponse := httptest.NewRecorder()
	h.serveOwned(secondMemoryResponse, secondMemory)
	if secondMemoryResponse.Code != http.StatusOK || !bytes.Contains(secondMemoryResponse.Body.Bytes(), []byte(`"revision":"2"`)) {
		t.Fatalf("second memory put=%d %s", secondMemoryResponse.Code, secondMemoryResponse.Body.String())
	}
	staleMemoryBody := []byte(`{"principalId":"U1","content":"stale plan","revision":"1"}`)
	staleMemory := httptest.NewRequest(http.MethodPut, "/v1/memory", bytes.NewReader(staleMemoryBody))
	signSourceRequest(t, staleMemory, cfg.Auth.SourceSigningSecret, staleMemoryBody)
	staleMemoryResponse := httptest.NewRecorder()
	h.serveOwned(staleMemoryResponse, staleMemory)
	if staleMemoryResponse.Code != http.StatusConflict || !bytes.Contains(staleMemoryResponse.Body.Bytes(), []byte(`"revision":"2"`)) {
		t.Fatalf("stale memory put=%d %s", staleMemoryResponse.Code, staleMemoryResponse.Body.String())
	}
	memoryHistory := httptest.NewRequest(http.MethodGet, "/v1/memory/history?principalId=U1", nil)
	memoryHistory.Header.Set("x-portal-identity", testPortalIdentityToken(t, cfg.Auth.PortalIdentitySecret, "U1"))
	signSourceRequest(t, memoryHistory, cfg.Auth.SourceSigningSecret, nil)
	memoryHistoryResponse := httptest.NewRecorder()
	h.serveOwned(memoryHistoryResponse, memoryHistory)
	if memoryHistoryResponse.Code != http.StatusOK || !bytes.Contains(memoryHistoryResponse.Body.Bytes(), []byte(`"revision":"2"`)) || !bytes.Contains(memoryHistoryResponse.Body.Bytes(), []byte(`"revision":"1"`)) {
		t.Fatalf("memory history=%d %s", memoryHistoryResponse.Code, memoryHistoryResponse.Body.String())
	}
	restoreMemoryBody := []byte(`{"principalId":"U1","revision":"1","expectedRevision":"2"}`)
	restoreMemory := httptest.NewRequest(http.MethodPost, "/v1/memory/restore", bytes.NewReader(restoreMemoryBody))
	restoreMemory.Header.Set("x-portal-identity", testPortalIdentityToken(t, cfg.Auth.PortalIdentitySecret, "U1"))
	signSourceRequest(t, restoreMemory, cfg.Auth.SourceSigningSecret, restoreMemoryBody)
	restoreMemoryResponse := httptest.NewRecorder()
	h.serveOwned(restoreMemoryResponse, restoreMemory)
	if restoreMemoryResponse.Code != http.StatusOK || !bytes.Contains(restoreMemoryResponse.Body.Bytes(), []byte(`"revision":"3"`)) || !bytes.Contains(restoreMemoryResponse.Body.Bytes(), []byte(`"content":"first plan\n"`)) {
		t.Fatalf("memory restore=%d %s", restoreMemoryResponse.Code, restoreMemoryResponse.Body.String())
	}
	memoryCapability, err := auth.MintCapability(auth.Claims{
		ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
		Memory: &auth.MemoryGrant{Write: "personal:U1", Read: []string{"personal:U1"}},
	}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	capabilityMemoryHistory := httptest.NewRequest(http.MethodGet, "/v1/memory/history", nil)
	capabilityMemoryHistory.Header.Set(auth.CapabilityHeader, memoryCapability)
	capabilityMemoryHistoryResponse := httptest.NewRecorder()
	h.serveOwned(capabilityMemoryHistoryResponse, capabilityMemoryHistory)
	if capabilityMemoryHistoryResponse.Code != http.StatusOK || !bytes.Contains(capabilityMemoryHistoryResponse.Body.Bytes(), []byte(`"revision":"3"`)) {
		t.Fatalf("capability memory history=%d %s", capabilityMemoryHistoryResponse.Code, capabilityMemoryHistoryResponse.Body.String())
	}
	if err := h.memory.Replace(ctx, "org:acme", "organization plan", "admin"); err != nil {
		t.Fatal(err)
	}
	organizationMemoryCapability, err := auth.MintCapability(auth.Claims{
		ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
		Memory: &auth.MemoryGrant{Write: "personal:U1", OrgWrite: "org:acme", Read: []string{"personal:U1", "org:acme"}},
	}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	organizationMemoryHistory := httptest.NewRequest(http.MethodGet, "/v1/memory/history?scope=org", nil)
	organizationMemoryHistory.Header.Set(auth.CapabilityHeader, organizationMemoryCapability)
	organizationMemoryHistoryResponse := httptest.NewRecorder()
	h.serveOwned(organizationMemoryHistoryResponse, organizationMemoryHistory)
	if organizationMemoryHistoryResponse.Code != http.StatusOK || !bytes.Contains(organizationMemoryHistoryResponse.Body.Bytes(), []byte(`"content":"organization plan\n"`)) {
		t.Fatalf("capability organization memory history=%d %s", organizationMemoryHistoryResponse.Code, organizationMemoryHistoryResponse.Body.String())
	}
	capabilityMemoryRestoreBody := []byte(`{"revision":"3","expectedRevision":"3"}`)
	capabilityMemoryRestore := httptest.NewRequest(http.MethodPost, "/v1/memory/restore", bytes.NewReader(capabilityMemoryRestoreBody))
	capabilityMemoryRestore.Header.Set(auth.CapabilityHeader, memoryCapability)
	capabilityMemoryRestoreResponse := httptest.NewRecorder()
	h.serveOwned(capabilityMemoryRestoreResponse, capabilityMemoryRestore)
	if capabilityMemoryRestoreResponse.Code != http.StatusOK || !bytes.Contains(capabilityMemoryRestoreResponse.Body.Bytes(), []byte(`"revision":"3"`)) {
		t.Fatalf("capability memory restore=%d %s", capabilityMemoryRestoreResponse.Code, capabilityMemoryRestoreResponse.Body.String())
	}
	missingMemoryGrant, err := auth.MintCapability(auth.Claims{ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	missingMemoryHistory := httptest.NewRequest(http.MethodGet, "/v1/memory/history", nil)
	missingMemoryHistory.Header.Set(auth.CapabilityHeader, missingMemoryGrant)
	missingMemoryHistoryResponse := httptest.NewRecorder()
	h.serveOwned(missingMemoryHistoryResponse, missingMemoryHistory)
	if missingMemoryHistoryResponse.Code != http.StatusNotFound {
		t.Fatalf("missing capability memory grant=%d %s", missingMemoryHistoryResponse.Code, missingMemoryHistoryResponse.Body.String())
	}
	adminMemoryGet := httptest.NewRequest(http.MethodGet, "/v1/admin/memory?scope=personal:U1", nil)
	adminMemoryGet.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminMemoryGet, cfg.Auth.SourceSigningSecret, nil)
	adminMemoryGetResponse := httptest.NewRecorder()
	h.serveOwned(adminMemoryGetResponse, adminMemoryGet)
	if adminMemoryGetResponse.Code != http.StatusOK || !bytes.Contains(adminMemoryGetResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(adminMemoryGetResponse.Body.Bytes(), []byte(`"content":"first plan\n"`)) {
		t.Fatalf("admin memory get=%d %s", adminMemoryGetResponse.Code, adminMemoryGetResponse.Body.String())
	}
	adminMemoryPutBody := []byte(`{"content":"admin curated plan"}`)
	adminMemoryPut := httptest.NewRequest(http.MethodPut, "/v1/admin/memory?scope=personal:U1", bytes.NewReader(adminMemoryPutBody))
	adminMemoryPut.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminMemoryPut, cfg.Auth.SourceSigningSecret, adminMemoryPutBody)
	adminMemoryPutResponse := httptest.NewRecorder()
	h.serveOwned(adminMemoryPutResponse, adminMemoryPut)
	if adminMemoryPutResponse.Code != http.StatusOK || !bytes.Contains(adminMemoryPutResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("admin memory put=%d %s", adminMemoryPutResponse.Code, adminMemoryPutResponse.Body.String())
	}
	userOnboardingBody := []byte(`{"status":"pending"}`)
	userOnboardingRequest := httptest.NewRequest(http.MethodPut, "/v1/admin/users/U1/onboarding", bytes.NewReader(userOnboardingBody))
	userOnboardingRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, userOnboardingRequest, cfg.Auth.SourceSigningSecret, userOnboardingBody)
	userOnboardingResponse := httptest.NewRecorder()
	h.serveOwned(userOnboardingResponse, userOnboardingRequest)
	if userOnboardingResponse.Code != http.StatusOK || !bytes.Contains(userOnboardingResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(userOnboardingResponse.Body.Bytes(), []byte(`"status":"pending"`)) {
		t.Fatalf("user onboarding=%d %s", userOnboardingResponse.Code, userOnboardingResponse.Body.String())
	}
	updatedMemory, err := h.memory.Head(ctx, "personal:U1")
	if err != nil || !strings.Contains(updatedMemory.Content, "- Onboarding: pending v2 since ") || !strings.Contains(updatedMemory.Content, "admin curated plan\n") {
		t.Fatalf("user onboarding memory=%q err=%v", updatedMemory.Content, err)
	}
	userDetailRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/users/U1", nil)
	userDetailRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, userDetailRequest, cfg.Auth.SourceSigningSecret, nil)
	userDetailResponse := httptest.NewRecorder()
	h.serveOwned(userDetailResponse, userDetailRequest)
	if userDetailResponse.Code != http.StatusOK || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"displayName":"Alice"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"id":"session-reset"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"id":"file-scope"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"id":"deployment-scope"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"id":"cron-1"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"securityPosture":"strict"`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"provider":"github"`)) || bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`encrypted-secret`)) || !bytes.Contains(userDetailResponse.Body.Bytes(), []byte(`"onboarding":"pending"`)) {
		t.Fatalf("user detail=%d %s", userDetailResponse.Code, userDetailResponse.Body.String())
	}
	badOnboardingBody := []byte(`{"status":"unknown"}`)
	badOnboardingRequest := httptest.NewRequest(http.MethodPut, "/v1/admin/users/U1/onboarding", bytes.NewReader(badOnboardingBody))
	badOnboardingRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, badOnboardingRequest, cfg.Auth.SourceSigningSecret, badOnboardingBody)
	badOnboardingResponse := httptest.NewRecorder()
	h.serveOwned(badOnboardingResponse, badOnboardingRequest)
	if badOnboardingResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid user onboarding=%d %s", badOnboardingResponse.Code, badOnboardingResponse.Body.String())
	}
	userResetRequest := httptest.NewRequest(http.MethodPost, "/v1/admin/users/U1/reset", nil)
	userResetRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, userResetRequest, cfg.Auth.SourceSigningSecret, nil)
	userResetResponse := httptest.NewRecorder()
	h.serveOwned(userResetResponse, userResetRequest)
	if userResetResponse.Code != http.StatusOK || !bytes.Contains(userResetResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(userResetResponse.Body.Bytes(), []byte(`"deletedSessions":1`)) {
		t.Fatalf("user reset=%d %s", userResetResponse.Code, userResetResponse.Body.String())
	}
	var resetSessionCount, resetParticipantCount, resetEntryCount, resetLLMCount, resetLeaseCount, resetTapeCount, preservedSessionCount int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM sessions WHERE id='session-reset'").Scan(&resetSessionCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM participants WHERE session_id='session-reset'").Scan(&resetParticipantCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM session_entries WHERE session_id='session-reset'").Scan(&resetEntryCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM session_llm_requests WHERE session_id='session-reset'").Scan(&resetLLMCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM session_leases WHERE session_id='session-reset'").Scan(&resetLeaseCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM session_tape WHERE session_id='session-reset'").Scan(&resetTapeCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM sessions WHERE id='session-1'").Scan(&preservedSessionCount); err != nil {
		t.Fatal(err)
	}
	if resetSessionCount != 0 || resetParticipantCount != 0 || resetEntryCount != 0 || resetLLMCount != 0 || resetLeaseCount != 0 || resetTapeCount != 0 || preservedSessionCount != 1 {
		t.Fatalf("user reset left state sessions=%d participants=%d entries=%d llm=%d leases=%d tape=%d preserved=%d", resetSessionCount, resetParticipantCount, resetEntryCount, resetLLMCount, resetLeaseCount, resetTapeCount, preservedSessionCount)
	}
	resetMemory, err := h.memory.Head(ctx, "personal:U1")
	if err != nil || strings.Contains(resetMemory.Content, "Onboarding:") || !strings.Contains(resetMemory.Content, "admin curated plan\n") {
		t.Fatalf("user reset memory=%q err=%v", resetMemory.Content, err)
	}
	adminMemoryScopes := httptest.NewRequest(http.MethodGet, "/v1/admin/memory/scopes", nil)
	adminMemoryScopes.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminMemoryScopes, cfg.Auth.SourceSigningSecret, nil)
	adminMemoryScopesResponse := httptest.NewRecorder()
	h.serveOwned(adminMemoryScopesResponse, adminMemoryScopes)
	if adminMemoryScopesResponse.Code != http.StatusOK || !bytes.Contains(adminMemoryScopesResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(adminMemoryScopesResponse.Body.Bytes(), []byte(`"label":"Alice"`)) || !bytes.Contains(adminMemoryScopesResponse.Body.Bytes(), []byte(`"hasMemory":true`)) {
		t.Fatalf("admin memory scopes=%d %s", adminMemoryScopesResponse.Code, adminMemoryScopesResponse.Body.String())
	}
	adminDeployments := httptest.NewRequest(http.MethodGet, "/v1/admin/deployments?scope=org:acme", nil)
	adminDeployments.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminDeployments, cfg.Auth.SourceSigningSecret, nil)
	adminDeploymentsResponse := httptest.NewRecorder()
	h.serveOwned(adminDeploymentsResponse, adminDeployments)
	if adminDeploymentsResponse.Code != http.StatusOK || !bytes.Contains(adminDeploymentsResponse.Body.Bytes(), []byte(`"name":"Incident dashboard"`)) || !bytes.Contains(adminDeploymentsResponse.Body.Bytes(), []byte(`"publicUrl":"https://deploy.example/dashboard?x=1"`)) || bytes.Contains(adminDeploymentsResponse.Body.Bytes(), []byte(`access=secret`)) {
		t.Fatalf("admin deployments=%d %s", adminDeploymentsResponse.Code, adminDeploymentsResponse.Body.String())
	}
	adminCrons := httptest.NewRequest(http.MethodGet, "/v1/admin/crons?scope=org:acme", nil)
	adminCrons.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminCrons, cfg.Auth.SourceSigningSecret, nil)
	adminCronsResponse := httptest.NewRecorder()
	h.serveOwned(adminCronsResponse, adminCrons)
	if adminCronsResponse.Code != http.StatusOK || !bytes.Contains(adminCronsResponse.Body.Bytes(), []byte(`"id":"cron-1"`)) || !bytes.Contains(adminCronsResponse.Body.Bytes(), []byte(`"title":"Deploy reminder"`)) || bytes.Contains(adminCronsResponse.Body.Bytes(), []byte(`"fireLog"`)) {
		t.Fatalf("admin crons=%d %s", adminCronsResponse.Code, adminCronsResponse.Body.String())
	}
	adminCronDestinationBody := []byte(`{"destination":{"type":"slack","target":"C1"}}`)
	adminCronDestination := httptest.NewRequest(http.MethodPut, "/v1/admin/crons/cron-1/destination?scope=org:acme", bytes.NewReader(adminCronDestinationBody))
	adminCronDestination.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminCronDestination, cfg.Auth.SourceSigningSecret, adminCronDestinationBody)
	adminCronDestinationResponse := httptest.NewRecorder()
	h.serveOwned(adminCronDestinationResponse, adminCronDestination)
	if adminCronDestinationResponse.Code != http.StatusOK || !bytes.Contains(adminCronDestinationResponse.Body.Bytes(), []byte(`"target":"C1"`)) || !bytes.Contains(adminCronDestinationResponse.Body.Bytes(), []byte(`"fireLog"`)) {
		t.Fatalf("admin cron destination=%d %s", adminCronDestinationResponse.Code, adminCronDestinationResponse.Body.String())
	}
	var cronDestination json.RawMessage
	if err := pg.Pool.QueryRow(ctx, "SELECT json->'destination' FROM crons WHERE id='cron-1'").Scan(&cronDestination); err != nil || !bytes.Contains(cronDestination, []byte(`"target": "C1"`)) {
		t.Fatalf("stored cron destination=%s err=%v", cronDestination, err)
	}
	var cronNoticeDestination, cronNoticeText string
	if err := pg.Pool.QueryRow(ctx, "SELECT destination::text,text FROM deliveries WHERE idempotency_key LIKE 'cron-edit-notice:cron-1:%'").Scan(&cronNoticeDestination, &cronNoticeText); err != nil || !strings.Contains(cronNoticeDestination, `"target": "U1"`) || !strings.Contains(cronNoticeDestination, `"onBehalfOf": "admin"`) || cronNoticeText != `Heads up: admin changed your "Deploy reminder" cron to post somewhere new.` {
		t.Fatalf("cron destination notice destination=%s text=%q err=%v", cronNoticeDestination, cronNoticeText, err)
	}
	var cronRetargetAudits, cronDestinationAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='cron_retarget' AND resource='cron-1'").Scan(&cronRetargetAudits); err != nil || cronRetargetAudits != 1 {
		t.Fatalf("cron retarget audit count=%d err=%v", cronRetargetAudits, err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='cron.destination.update' AND resource='cron-1'").Scan(&cronDestinationAudits); err != nil || cronDestinationAudits != 1 {
		t.Fatalf("cron destination audit count=%d err=%v", cronDestinationAudits, err)
	}
	adminScopes := httptest.NewRequest(http.MethodGet, "/v1/admin/scopes", nil)
	adminScopes.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminScopes, cfg.Auth.SourceSigningSecret, nil)
	adminScopesResponse := httptest.NewRecorder()
	h.serveOwned(adminScopesResponse, adminScopes)
	if adminScopesResponse.Code != http.StatusOK || !bytes.Contains(adminScopesResponse.Body.Bytes(), []byte(`"scopeId":"project:p1"`)) || !bytes.Contains(adminScopesResponse.Body.Bytes(), []byte(`"crons":1`)) || !bytes.Contains(adminScopesResponse.Body.Bytes(), []byte(`"deployments":1`)) || !bytes.Contains(adminScopesResponse.Body.Bytes(), []byte(`"skills":1`)) {
		t.Fatalf("admin scopes=%d %s", adminScopesResponse.Code, adminScopesResponse.Body.String())
	}
	adminResources := httptest.NewRequest(http.MethodGet, "/v1/admin/resources", nil)
	adminResources.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminResources, cfg.Auth.SourceSigningSecret, nil)
	adminResourcesResponse := httptest.NewRecorder()
	h.serveOwned(adminResourcesResponse, adminResources)
	if adminResourcesResponse.Code != http.StatusOK || !bytes.Contains(adminResourcesResponse.Body.Bytes(), []byte(`"id":"security-posture"`)) || !bytes.Contains(adminResourcesResponse.Body.Bytes(), []byte(`"id":"service-credentials"`)) || !bytes.Contains(adminResourcesResponse.Body.Bytes(), []byte(`"secret":true`)) || !bytes.Contains(adminResourcesResponse.Body.Bytes(), []byte(`"id":"gpt-5.6-sol"`)) {
		t.Fatalf("admin resources=%d %s", adminResourcesResponse.Code, adminResourcesResponse.Body.String())
	}
	adminCustomProviders := httptest.NewRequest(http.MethodGet, "/v1/admin/custom-providers", nil)
	adminCustomProviders.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminCustomProviders, cfg.Auth.SourceSigningSecret, nil)
	adminCustomProvidersResponse := httptest.NewRecorder()
	h.serveOwned(adminCustomProvidersResponse, adminCustomProviders)
	if adminCustomProvidersResponse.Code != http.StatusOK || !bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`"id":"example-ai"`)) || !bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`"hasKey":true`)) || !bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`"id":"disabled-ai"`)) || !bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`"disabled":true`)) || bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`must-not-leak`)) || bytes.Contains(adminCustomProvidersResponse.Body.Bytes(), []byte(`disabled-key`)) {
		t.Fatalf("admin custom providers=%d %s", adminCustomProvidersResponse.Code, adminCustomProvidersResponse.Body.String())
	}
	adminSlackInstallation := httptest.NewRequest(http.MethodGet, "/v1/admin/slack-installation", nil)
	adminSlackInstallation.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminSlackInstallation, cfg.Auth.SourceSigningSecret, nil)
	adminSlackInstallationResponse := httptest.NewRecorder()
	h.serveOwned(adminSlackInstallationResponse, adminSlackInstallation)
	if adminSlackInstallationResponse.Code != http.StatusOK || !bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`"configured":true`)) || !bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`"managed":true`)) || !bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`"source":"admin"`)) || !bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`"teamId":"T1"`)) || bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`must-not-leak`)) || bytes.Contains(adminSlackInstallationResponse.Body.Bytes(), []byte(`also-secret`)) {
		t.Fatalf("admin Slack installation=%d %s", adminSlackInstallationResponse.Code, adminSlackInstallationResponse.Body.String())
	}
	var slackResponse struct {
		CreateURL string `json:"createUrl"`
	}
	if err := json.Unmarshal(adminSlackInstallationResponse.Body.Bytes(), &slackResponse); err != nil || !strings.Contains(slackResponse.CreateURL, "https://api.slack.com/apps?") || !strings.Contains(slackResponse.CreateURL, "new_app=1") {
		t.Fatalf("Slack create URL=%q err=%v", slackResponse.CreateURL, err)
	}
	adminSandboxRoutes := httptest.NewRequest(http.MethodGet, "/v1/admin/sandbox-routes", nil)
	adminSandboxRoutes.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminSandboxRoutes, cfg.Auth.SourceSigningSecret, nil)
	adminSandboxRoutesResponse := httptest.NewRecorder()
	h.serveOwned(adminSandboxRoutesResponse, adminSandboxRoutes)
	if adminSandboxRoutesResponse.Code != http.StatusOK || !bytes.Contains(adminSandboxRoutesResponse.Body.Bytes(), []byte(`"defaultBackend":"local"`)) || !bytes.Contains(adminSandboxRoutesResponse.Body.Bytes(), []byte(`"availableBackends":["local","sprites"]`)) || !bytes.Contains(adminSandboxRoutesResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(adminSandboxRoutesResponse.Body.Bytes(), []byte(`"capabilitiesLost":["stageOut"]`)) {
		t.Fatalf("admin sandbox routes=%d %s", adminSandboxRoutesResponse.Code, adminSandboxRoutesResponse.Body.String())
	}
	adminSkills := httptest.NewRequest(http.MethodGet, "/v1/admin/skills?scope=org:acme", nil)
	adminSkills.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminSkills, cfg.Auth.SourceSigningSecret, nil)
	adminSkillsResponse := httptest.NewRecorder()
	h.serveOwned(adminSkillsResponse, adminSkills)
	if adminSkillsResponse.Code != http.StatusOK || !bytes.Contains(adminSkillsResponse.Body.Bytes(), []byte(`"name":"deploy-check"`)) || !bytes.Contains(adminSkillsResponse.Body.Bytes(), []byte(`"pack":{"id":"pack-1","url":"https://github.com/acme/deploy-skills.git"}`)) {
		t.Fatalf("admin skills=%d %s", adminSkillsResponse.Code, adminSkillsResponse.Body.String())
	}
	adminSkillPacks := httptest.NewRequest(http.MethodGet, "/v1/admin/skill-packs", nil)
	adminSkillPacks.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminSkillPacks, cfg.Auth.SourceSigningSecret, nil)
	adminSkillPacksResponse := httptest.NewRecorder()
	h.serveOwned(adminSkillPacksResponse, adminSkillPacks)
	if adminSkillPacksResponse.Code != http.StatusOK || !bytes.Contains(adminSkillPacksResponse.Body.Bytes(), []byte(`"id":"pack-1"`)) || !bytes.Contains(adminSkillPacksResponse.Body.Bytes(), []byte(`"importedCount":1`)) {
		t.Fatalf("admin skill packs=%d %s", adminSkillPacksResponse.Code, adminSkillPacksResponse.Body.String())
	}
	patchSkillPackBody := []byte(`{"ref":" main ","trustTier":"internal","syncMode":"tracked","subset":["deploy-check"],"config":{"skillGlobs":["skills/*.md",42],"exclude":["tmp/**"],"fieldOverrides":{"description":"internal"}}}`)
	patchSkillPack := httptest.NewRequest(http.MethodPatch, "/v1/admin/skill-packs/pack-1", bytes.NewReader(patchSkillPackBody))
	patchSkillPack.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, patchSkillPack, cfg.Auth.SourceSigningSecret, patchSkillPackBody)
	patchSkillPackResponse := httptest.NewRecorder()
	h.serveOwned(patchSkillPackResponse, patchSkillPack)
	if patchSkillPackResponse.Code != http.StatusOK || !bytes.Contains(patchSkillPackResponse.Body.Bytes(), []byte(`"ref":"main"`)) || !bytes.Contains(patchSkillPackResponse.Body.Bytes(), []byte(`"syncMode":"tracked"`)) || !bytes.Contains(patchSkillPackResponse.Body.Bytes(), []byte(`"skillGlobs":["skills/*.md"]`)) || !bytes.Contains(patchSkillPackResponse.Body.Bytes(), []byte(`"fieldOverrides":{"description":"internal"}`)) {
		t.Fatalf("patch skill pack=%d %s", patchSkillPackResponse.Code, patchSkillPackResponse.Body.String())
	}
	var skillPackUpdateAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='skill_pack.update' AND resource='pack-1'").Scan(&skillPackUpdateAudits); err != nil || skillPackUpdateAudits != 1 {
		t.Fatalf("skill pack update audit count=%d err=%v", skillPackUpdateAudits, err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO skills(id,json) VALUES($1,$2::jsonb)", "skill-pack-owned", `{"id":"skill-pack-owned","scopeId":"project:p1","manifest":{"name":"pack-owned","description":"Owned by a removable pack","body":"# Pack owned"},"status":"published","createdBy":"pack:pack-1","version":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS skill_bundles(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO skill_bundles(id,json) VALUES($1,$2::jsonb)", "pack-1", `{"packId":"pack-1","commit":"abc","files":[],"hash":"bundle-hash"}`); err != nil {
		t.Fatal(err)
	}
	removeSkillPack := httptest.NewRequest(http.MethodDelete, "/v1/admin/skill-packs/pack-1", nil)
	removeSkillPack.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, removeSkillPack, cfg.Auth.SourceSigningSecret, nil)
	removeSkillPackResponse := httptest.NewRecorder()
	h.serveOwned(removeSkillPackResponse, removeSkillPack)
	if removeSkillPackResponse.Code != http.StatusOK || !bytes.Contains(removeSkillPackResponse.Body.Bytes(), []byte(`"removed":1`)) {
		t.Fatalf("remove skill pack=%d %s", removeSkillPackResponse.Code, removeSkillPackResponse.Body.String())
	}
	var retainedSkills, removedPackSkills, remainingPacks, remainingBundles int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM skills WHERE id='skill-1'").Scan(&retainedSkills); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM skills WHERE id='skill-pack-owned'").Scan(&removedPackSkills); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM skill_packs WHERE id='pack-1'").Scan(&remainingPacks); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM skill_bundles WHERE id='pack-1'").Scan(&remainingBundles); err != nil {
		t.Fatal(err)
	}
	if retainedSkills != 1 || removedPackSkills != 0 || remainingPacks != 0 || remainingBundles != 0 {
		t.Fatalf("skill pack removal retained=%d pack-skills=%d packs=%d bundles=%d", retainedSkills, removedPackSkills, remainingPacks, remainingBundles)
	}
	var skillPackRemoveAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='skill_pack.remove' AND resource='pack-1'").Scan(&skillPackRemoveAudits); err != nil || skillPackRemoveAudits != 1 {
		t.Fatalf("skill pack removal audit count=%d err=%v", skillPackRemoveAudits, err)
	}
	adminKeychain := httptest.NewRequest(http.MethodGet, "/v1/admin/keychain", nil)
	adminKeychain.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminKeychain, cfg.Auth.SourceSigningSecret, nil)
	adminKeychainResponse := httptest.NewRecorder()
	h.serveOwned(adminKeychainResponse, adminKeychain)
	if adminKeychainResponse.Code != http.StatusOK || !bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`"enabled":true`)) || !bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`"id":"credential-u1"`)) || !bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`"principalId":"U2"`)) || !bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`"status":"expired"`)) || bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`must-not-leak`)) || bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`credential-connector`)) || bytes.Contains(adminKeychainResponse.Body.Bytes(), []byte(`credential-broker`)) {
		t.Fatalf("admin keychain=%d %s", adminKeychainResponse.Code, adminKeychainResponse.Body.String())
	}
	var expiredAskStatus string
	var keychainVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT json->>'status' FROM keychain_asks WHERE id='ask-expired'").Scan(&expiredAskStatus); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_asks'").Scan(&keychainVersion); err != nil {
		t.Fatal(err)
	}
	if expiredAskStatus != "expired" || keychainVersion < 1 {
		t.Fatalf("keychain expired ask=%q map-version=%d", expiredAskStatus, keychainVersion)
	}
	revokeKeychainGrant := httptest.NewRequest(http.MethodPost, "/v1/keychain/grants/grant-active/revoke", nil)
	revokeKeychainGrant.Header.Set(auth.CapabilityHeader, capBody.Token)
	revokeKeychainGrantResponse := httptest.NewRecorder()
	h.serveOwned(revokeKeychainGrantResponse, revokeKeychainGrant)
	if revokeKeychainGrantResponse.Code != http.StatusOK || !bytes.Contains(revokeKeychainGrantResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("revoke keychain grant=%d %s", revokeKeychainGrantResponse.Code, revokeKeychainGrantResponse.Body.String())
	}
	deleteKeychainCredential := httptest.NewRequest(http.MethodDelete, "/v1/keychain/credentials/credential-u1", nil)
	deleteKeychainCredential.Header.Set(auth.CapabilityHeader, capBody.Token)
	deleteKeychainCredentialResponse := httptest.NewRecorder()
	h.serveOwned(deleteKeychainCredentialResponse, deleteKeychainCredential)
	if deleteKeychainCredentialResponse.Code != http.StatusOK || !bytes.Contains(deleteKeychainCredentialResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("delete keychain credential=%d %s", deleteKeychainCredentialResponse.Code, deleteKeychainCredentialResponse.Body.String())
	}
	var remainingCredentials, revokedGrants int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM keychain_credentials WHERE id='credential-u1'").Scan(&remainingCredentials); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM keychain_grants WHERE json->>'credentialId'='credential-u1' AND json->>'status'='revoked'").Scan(&revokedGrants); err != nil {
		t.Fatal(err)
	}
	var credentialVersion, grantVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_credentials'").Scan(&credentialVersion); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_grants'").Scan(&grantVersion); err != nil {
		t.Fatal(err)
	}
	if remainingCredentials != 0 || revokedGrants != 3 || credentialVersion != 1 || grantVersion != 5 {
		t.Fatalf("deleted keychain credential credentials=%d revoked=%d versions=%d/%d", remainingCredentials, revokedGrants, credentialVersion, grantVersion)
	}
	managedKeychainCredential := httptest.NewRequest(http.MethodDelete, "/v1/keychain/credentials/credential-connector", nil)
	managedKeychainCredential.Header.Set(auth.CapabilityHeader, capBody.Token)
	managedKeychainCredentialResponse := httptest.NewRecorder()
	h.serveOwned(managedKeychainCredentialResponse, managedKeychainCredential)
	if managedKeychainCredentialResponse.Code != http.StatusNotFound {
		t.Fatalf("managed keychain credential delete=%d %s", managedKeychainCredentialResponse.Code, managedKeychainCredentialResponse.Body.String())
	}
	githubDefault := testConnectorCredentialID("U2", "api.github.com", "")
	githubPersonal := testConnectorCredentialID("U2", "api.github.com", "personal")
	slackDefault := testConnectorCredentialID("U3", "slack.com", "")
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_credentials(id,json) VALUES($1,$2::jsonb),($3,$4::jsonb),($5,$6::jsonb)", githubDefault, fmt.Sprintf(`{"id":%q,"ownerId":"U2","service":"api.github.com","host":"api.github.com","kind":"env","managed":"connector","secretEnc":"never-returned","createdAt":1,"updatedAt":2}`, githubDefault), githubPersonal, fmt.Sprintf(`{"id":%q,"ownerId":"U2","service":"api.github.com","host":"api.github.com","kind":"env","managed":"connector","secretEnc":"never-returned","refresh":{"accountType":"personal"},"createdAt":1,"updatedAt":2}`, githubPersonal), slackDefault, fmt.Sprintf(`{"id":%q,"ownerId":"U3","service":"slack.com","host":"slack.com","kind":"env","managed":"connector","secretEnc":"never-returned","createdAt":1,"updatedAt":2}`, slackDefault)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_grants(id,json) VALUES($1,$2::jsonb)", "grant-connector-u2", fmt.Sprintf(`{"id":"grant-connector-u2","credentialId":%q,"ownerId":"U2","audienceScopeId":"channel:C1","mode":"standing","purpose":"connector","status":"active","createdAt":1}`, githubPersonal)); err != nil {
		t.Fatal(err)
	}
	memberCapability, err := auth.MintCapability(auth.Claims{ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", KeychainMembers: []auth.KeychainMember{{ID: "U2"}}, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, cfg.Auth.CapabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	nonMemberBody := []byte(`{"principalId":"U3","provider":"github"}`)
	nonMemberRevoke := httptest.NewRequest(http.MethodPost, "/v1/connectors/oauth/revoke", bytes.NewReader(nonMemberBody))
	nonMemberRevoke.Header.Set(auth.CapabilityHeader, memberCapability)
	nonMemberRevokeResponse := httptest.NewRecorder()
	h.serveOwned(nonMemberRevokeResponse, nonMemberRevoke)
	if nonMemberRevokeResponse.Code != http.StatusBadRequest || !bytes.Contains(nonMemberRevokeResponse.Body.Bytes(), []byte(`principalId must be a member of this conversation`)) {
		t.Fatalf("non-member OAuth revoke=%d %s", nonMemberRevokeResponse.Code, nonMemberRevokeResponse.Body.String())
	}
	memberRevokeBody := []byte(`{"principalId":"U2","provider":"github"}`)
	memberRevoke := httptest.NewRequest(http.MethodPost, "/v1/connectors/oauth/revoke", bytes.NewReader(memberRevokeBody))
	memberRevoke.Header.Set(auth.CapabilityHeader, memberCapability)
	memberRevokeResponse := httptest.NewRecorder()
	h.serveOwned(memberRevokeResponse, memberRevoke)
	if memberRevokeResponse.Code != http.StatusOK || !bytes.Contains(memberRevokeResponse.Body.Bytes(), []byte(`"provider":"github"`)) || !bytes.Contains(memberRevokeResponse.Body.Bytes(), []byte(`"hosts":["api.github.com"]`)) {
		t.Fatalf("member OAuth revoke=%d %s", memberRevokeResponse.Code, memberRevokeResponse.Body.String())
	}
	var remainingConnectorCredentials, revokedConnectorGrants int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM keychain_credentials WHERE id IN ($1,$2)", githubDefault, githubPersonal).Scan(&remainingConnectorCredentials); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM keychain_grants WHERE id='grant-connector-u2' AND json->>'status'='revoked'").Scan(&revokedConnectorGrants); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_credentials'").Scan(&credentialVersion); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_grants'").Scan(&grantVersion); err != nil {
		t.Fatal(err)
	}
	if remainingConnectorCredentials != 0 || revokedConnectorGrants != 1 || credentialVersion != 4 || grantVersion != 6 {
		t.Fatalf("member OAuth revoke credentials=%d grants=%d versions=%d/%d", remainingConnectorCredentials, revokedConnectorGrants, credentialVersion, grantVersion)
	}
	sourceRevokeBody := []byte(`{"principalId":"U3","host":"slack.com"}`)
	sourceRevoke := httptest.NewRequest(http.MethodPost, "/v1/connectors/oauth/revoke", bytes.NewReader(sourceRevokeBody))
	signSourceRequest(t, sourceRevoke, cfg.Auth.SourceSigningSecret, sourceRevokeBody)
	sourceRevokeResponse := httptest.NewRecorder()
	h.serveOwned(sourceRevokeResponse, sourceRevoke)
	if sourceRevokeResponse.Code != http.StatusOK || !bytes.Contains(sourceRevokeResponse.Body.Bytes(), []byte(`"host":"slack.com"`)) {
		t.Fatalf("source OAuth revoke=%d %s", sourceRevokeResponse.Code, sourceRevokeResponse.Body.String())
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM keychain_credentials WHERE id=$1", slackDefault).Scan(&remainingConnectorCredentials); err != nil {
		t.Fatal(err)
	}
	if remainingConnectorCredentials != 0 {
		t.Fatalf("source OAuth revoke left credential count=%d", remainingConnectorCredentials)
	}
	triggeredSkillArchive := httptest.NewRequest(http.MethodDelete, "/v1/skills/skill-scope", nil)
	triggeredSkillArchive.Header.Set(auth.CapabilityHeader, testCapabilityTokenWithClaims(t, cfg.Auth.CapabilitySecret, auth.Claims{ActorID: "U1", ScopeID: "channel:C1", Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}))
	triggeredSkillArchiveResponse := httptest.NewRecorder()
	h.serveOwned(triggeredSkillArchiveResponse, triggeredSkillArchive)
	if triggeredSkillArchiveResponse.Code != http.StatusForbidden || !bytes.Contains(triggeredSkillArchiveResponse.Body.Bytes(), []byte(sharedSkillTriggerRefusal)) {
		t.Fatalf("triggered skill archive=%d %s", triggeredSkillArchiveResponse.Code, triggeredSkillArchiveResponse.Body.String())
	}
	liveAuthorSkillArchive := httptest.NewRequest(http.MethodDelete, "/v1/skills/skill-scope", nil)
	liveAuthorSkillArchive.Header.Set(auth.CapabilityHeader, testCapabilityTokenWithClaims(t, cfg.Auth.CapabilitySecret, auth.Claims{ActorID: "U1", ScopeID: "channel:C1", Audience: "control-plane", LiveAuthor: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}))
	liveAuthorSkillArchiveResponse := httptest.NewRecorder()
	h.serveOwned(liveAuthorSkillArchiveResponse, liveAuthorSkillArchive)
	if liveAuthorSkillArchiveResponse.Code != http.StatusOK || !bytes.Contains(liveAuthorSkillArchiveResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("live-author skill archive=%d %s", liveAuthorSkillArchiveResponse.Code, liveAuthorSkillArchiveResponse.Body.String())
	}
	sourceSkillArchiveBody := []byte(`{"principalId":"U1"}`)
	sourceSkillArchive := httptest.NewRequest(http.MethodDelete, "/v1/skills/skill-scope", bytes.NewReader(sourceSkillArchiveBody))
	signSourceRequest(t, sourceSkillArchive, cfg.Auth.SourceSigningSecret, sourceSkillArchiveBody)
	sourceSkillArchiveResponse := httptest.NewRecorder()
	h.serveOwned(sourceSkillArchiveResponse, sourceSkillArchive)
	if sourceSkillArchiveResponse.Code != http.StatusOK {
		t.Fatalf("source skill archive=%d %s", sourceSkillArchiveResponse.Code, sourceSkillArchiveResponse.Body.String())
	}
	var archivedOwnedSkillStatus string
	var ownedSkillArchiveAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT json->>'status' FROM skills WHERE id='skill-scope'").Scan(&archivedOwnedSkillStatus); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='skill_archive' AND resource='skill-scope'").Scan(&ownedSkillArchiveAudits); err != nil {
		t.Fatal(err)
	}
	if archivedOwnedSkillStatus != "archived" || ownedSkillArchiveAudits != 2 {
		t.Fatalf("owned skill archive status=%q audits=%d", archivedOwnedSkillStatus, ownedSkillArchiveAudits)
	}
	adminSkill := httptest.NewRequest(http.MethodGet, "/v1/admin/skills/skill-1?scope=project:p1", nil)
	adminSkill.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminSkill, cfg.Auth.SourceSigningSecret, nil)
	adminSkillResponse := httptest.NewRecorder()
	h.serveOwned(adminSkillResponse, adminSkill)
	if adminSkillResponse.Code != http.StatusOK || !bytes.Contains(adminSkillResponse.Body.Bytes(), []byte(`"body":"# Deploy check"`)) || !bytes.Contains(adminSkillResponse.Body.Bytes(), []byte(`"path":"scripts/check.sh"`)) || !bytes.Contains(adminSkillResponse.Body.Bytes(), []byte(`"executable":true`)) || bytes.Contains(adminSkillResponse.Body.Bytes(), []byte(`echo safe`)) {
		t.Fatalf("admin skill=%d %s", adminSkillResponse.Code, adminSkillResponse.Body.String())
	}
	archiveAdminSkill := httptest.NewRequest(http.MethodDelete, "/v1/admin/skills/skill-1?scope=project:p1", nil)
	archiveAdminSkill.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, archiveAdminSkill, cfg.Auth.SourceSigningSecret, nil)
	archiveAdminSkillResponse := httptest.NewRecorder()
	h.serveOwned(archiveAdminSkillResponse, archiveAdminSkill)
	if archiveAdminSkillResponse.Code != http.StatusOK || !bytes.Contains(archiveAdminSkillResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("archive admin skill=%d %s", archiveAdminSkillResponse.Code, archiveAdminSkillResponse.Body.String())
	}
	var archivedSkillStatus string
	var skillMapVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT json->>'status' FROM skills WHERE id=$1", "skill-1").Scan(&archivedSkillStatus); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='skills'").Scan(&skillMapVersion); err != nil {
		t.Fatal(err)
	}
	if archivedSkillStatus != "archived" || skillMapVersion < 1 {
		t.Fatalf("skill archive persistence status=%q durable-map-version=%d", archivedSkillStatus, skillMapVersion)
	}
	listSessionsRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions?principalId=U1", nil)
	signSourceRequest(t, listSessionsRequest, cfg.Auth.SourceSigningSecret, nil)
	listSessionsResponse := httptest.NewRecorder()
	h.serveOwned(listSessionsResponse, listSessionsRequest)
	if listSessionsResponse.Code != http.StatusOK || !bytes.Contains(listSessionsResponse.Body.Bytes(), []byte(`"id":"session-1"`)) || !bytes.Contains(listSessionsResponse.Body.Bytes(), []byte(`"id":"session-project-approval"`)) || bytes.Contains(listSessionsResponse.Body.Bytes(), []byte(`"id":"session-channel"`)) {
		t.Fatalf("sessions list=%d %s", listSessionsResponse.Code, listSessionsResponse.Body.String())
	}
	listConversationsRequest := httptest.NewRequest(http.MethodGet, "/v1/conversations", nil)
	listConversationsRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	listConversationsResponse := httptest.NewRecorder()
	h.serveOwned(listConversationsResponse, listConversationsRequest)
	if listConversationsResponse.Code != http.StatusOK || !bytes.Contains(listConversationsResponse.Body.Bytes(), []byte(`"id":"session-1"`)) {
		t.Fatalf("conversations list=%d %s", listConversationsResponse.Code, listConversationsResponse.Body.String())
	}
	backgroundRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/background?viewer=U1", nil)
	signSourceRequest(t, backgroundRequest, cfg.Auth.SourceSigningSecret, nil)
	backgroundResponse := httptest.NewRecorder()
	h.serveOwned(backgroundResponse, backgroundRequest)
	if backgroundResponse.Code != http.StatusOK || !bytes.Contains(backgroundResponse.Body.Bytes(), []byte(`"processId":"proc-1"`)) || !bytes.Contains(backgroundResponse.Body.Bytes(), []byte(`"pattern":"ERROR"`)) {
		t.Fatalf("session background=%d %s", backgroundResponse.Code, backgroundResponse.Body.String())
	}
	approvalsRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/approvals?viewer=U1", nil)
	signSourceRequest(t, approvalsRequest, cfg.Auth.SourceSigningSecret, nil)
	approvalsResponse := httptest.NewRecorder()
	h.serveOwned(approvalsResponse, approvalsRequest)
	if approvalsResponse.Code != http.StatusOK || !bytes.Contains(approvalsResponse.Body.Bytes(), []byte(`"command":"deploy --prod"`)) || !bytes.Contains(approvalsResponse.Body.Bytes(), []byte(`"reason":"production confirmation"`)) || !bytes.Contains(approvalsResponse.Body.Bytes(), []byte(`"grantModes":["once"]`)) {
		t.Fatalf("session approvals=%d %s", approvalsResponse.Code, approvalsResponse.Body.String())
	}
	projectApprovalsRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-project-approval/approvals?viewer=U1", nil)
	signSourceRequest(t, projectApprovalsRequest, cfg.Auth.SourceSigningSecret, nil)
	projectApprovalsResponse := httptest.NewRecorder()
	h.serveOwned(projectApprovalsResponse, projectApprovalsRequest)
	if projectApprovalsResponse.Code != http.StatusOK || !bytes.Contains(projectApprovalsResponse.Body.Bytes(), []byte(`"command":"deploy current"`)) || bytes.Contains(projectApprovalsResponse.Body.Bytes(), []byte(`"command":"deploy stale"`)) {
		t.Fatalf("project session approvals=%d %s", projectApprovalsResponse.Code, projectApprovalsResponse.Body.String())
	}
	approvalRequest := httptest.NewRequest(http.MethodGet, "/v1/approvals/approval-1", nil)
	signSourceRequest(t, approvalRequest, cfg.Auth.SourceSigningSecret, nil)
	approvalResponse := httptest.NewRecorder()
	h.serveOwned(approvalResponse, approvalRequest)
	if approvalResponse.Code != http.StatusOK || !bytes.Contains(approvalResponse.Body.Bytes(), []byte(`"requestId":"approval-1"`)) || !bytes.Contains(approvalResponse.Body.Bytes(), []byte(`"command":"deploy --prod"`)) {
		t.Fatalf("approval read=%d %s", approvalResponse.Code, approvalResponse.Body.String())
	}
	staleApprovalRequest := httptest.NewRequest(http.MethodGet, "/v1/approvals/approval-project-stale", nil)
	signSourceRequest(t, staleApprovalRequest, cfg.Auth.SourceSigningSecret, nil)
	staleApprovalResponse := httptest.NewRecorder()
	h.serveOwned(staleApprovalResponse, staleApprovalRequest)
	if staleApprovalResponse.Code != http.StatusNotFound {
		t.Fatalf("stale approval read=%d %s", staleApprovalResponse.Code, staleApprovalResponse.Body.String())
	}
	pendingApprovalRequest := httptest.NewRequest(http.MethodGet, "/v1/approvals/pending?threadRef=thread-1", nil)
	signSourceRequest(t, pendingApprovalRequest, cfg.Auth.SourceSigningSecret, nil)
	pendingApprovalResponse := httptest.NewRecorder()
	h.serveOwned(pendingApprovalResponse, pendingApprovalRequest)
	if pendingApprovalResponse.Code != http.StatusOK || !bytes.Contains(pendingApprovalResponse.Body.Bytes(), []byte(`"status":"pending_approval"`)) || !bytes.Contains(pendingApprovalResponse.Body.Bytes(), []byte(`"command":"deploy --prod"`)) {
		t.Fatalf("pending approval=%d %s", pendingApprovalResponse.Code, pendingApprovalResponse.Body.String())
	}
	contextsRequest := httptest.NewRequest(http.MethodGet, "/v1/contexts?principalId=U1", nil)
	signSourceRequest(t, contextsRequest, cfg.Auth.SourceSigningSecret, nil)
	contextsResponse := httptest.NewRecorder()
	h.serveOwned(contextsResponse, contextsRequest)
	if contextsResponse.Code != http.StatusOK || !bytes.Contains(contextsResponse.Body.Bytes(), []byte(`"scopeId":"personal:U1"`)) || !bytes.Contains(contextsResponse.Body.Bytes(), []byte(`"scopeId":"channel:C1"`)) || !bytes.Contains(contextsResponse.Body.Bytes(), []byte(`"sessionCount":0`)) {
		t.Fatalf("contexts=%d %s", contextsResponse.Code, contextsResponse.Body.String())
	}
	deploymentsRequest := httptest.NewRequest(http.MethodGet, "/v1/deployments", nil)
	signSourceRequest(t, deploymentsRequest, cfg.Auth.SourceSigningSecret, nil)
	deploymentsResponse := httptest.NewRecorder()
	h.serveOwned(deploymentsResponse, deploymentsRequest)
	if deploymentsResponse.Code != http.StatusOK || !bytes.Contains(deploymentsResponse.Body.Bytes(), []byte(`"id":"deployment-1"`)) || !bytes.Contains(deploymentsResponse.Body.Bytes(), []byte(`"updatedAt":2000`)) {
		t.Fatalf("deployments=%d %s", deploymentsResponse.Code, deploymentsResponse.Body.String())
	}
	deploymentRequest := httptest.NewRequest(http.MethodGet, "/v1/deployments/incident-dashboard", nil)
	signSourceRequest(t, deploymentRequest, cfg.Auth.SourceSigningSecret, nil)
	deploymentResponse := httptest.NewRecorder()
	h.serveOwned(deploymentResponse, deploymentRequest)
	if deploymentResponse.Code != http.StatusOK || !bytes.Contains(deploymentResponse.Body.Bytes(), []byte(`"displayName":"Incident dashboard"`)) || !bytes.Contains(deploymentResponse.Body.Bytes(), []byte(`"appliedVersion":1`)) {
		t.Fatalf("deployment=%d %s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	shareMembers := []data.DirectoryMember{{PrincipalID: "U1", DisplayName: "Alice", Type: "internal", SlackID: "U1"}, {PrincipalID: "U2", DisplayName: "Bob", Type: "internal", SlackID: "U2"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &shareMembers}); err != nil {
		t.Fatal(err)
	}
	deploymentShareMissing := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader([]byte(`{}`)))
	deploymentShareMissing.Header.Set(auth.CapabilityHeader, capBody.Token)
	deploymentShareMissingResponse := httptest.NewRecorder()
	h.serveOwned(deploymentShareMissingResponse, deploymentShareMissing)
	var deploymentShareMissingBody struct {
		Message string `json:"message"`
	}
	if deploymentShareMissingResponse.Code != http.StatusBadRequest || json.Unmarshal(deploymentShareMissingResponse.Body.Bytes(), &deploymentShareMissingBody) != nil || deploymentShareMissingBody.Message != "a target is required: pass `scope` (\"org\" or a scope id) or `recipient` (a teammate's name)" {
		t.Fatalf("missing deployment share target=%d %s", deploymentShareMissingResponse.Code, deploymentShareMissingResponse.Body.String())
	}
	deploymentShareNoMatch := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader([]byte(`{"recipient":" Nobody "}`)))
	deploymentShareNoMatch.Header.Set(auth.CapabilityHeader, capBody.Token)
	deploymentShareNoMatchResponse := httptest.NewRecorder()
	h.serveOwned(deploymentShareNoMatchResponse, deploymentShareNoMatch)
	var deploymentShareNoMatchBody struct {
		Message string `json:"message"`
	}
	if deploymentShareNoMatchResponse.Code != http.StatusNotFound || json.Unmarshal(deploymentShareNoMatchResponse.Body.Bytes(), &deploymentShareNoMatchBody) != nil || deploymentShareNoMatchBody.Message != "no teammate matches \" Nobody \"" {
		t.Fatalf("unmatched deployment share target=%d %s", deploymentShareNoMatchResponse.Code, deploymentShareNoMatchResponse.Body.String())
	}
	deploymentShareBody := []byte(`{"recipient":"Bob","access":"manage"}`)
	deploymentShareRequest := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader(deploymentShareBody))
	deploymentShareRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	deploymentShareResponse := httptest.NewRecorder()
	h.serveOwned(deploymentShareResponse, deploymentShareRequest)
	if deploymentShareResponse.Code != http.StatusOK || !bytes.Contains(deploymentShareResponse.Body.Bytes(), []byte(`"scope":"personal:U2"`)) || !bytes.Contains(deploymentShareResponse.Body.Bytes(), []byte(`"label":"Bob"`)) || !bytes.Contains(deploymentShareResponse.Body.Bytes(), []byte(`"access":"manage"`)) || !bytes.Contains(deploymentShareResponse.Body.Bytes(), []byte(`"reach":"1 grantee"`)) || !bytes.Contains(deploymentShareResponse.Body.Bytes(), []byte(`"permission":"write"`)) {
		t.Fatalf("deployment share=%d %s", deploymentShareResponse.Code, deploymentShareResponse.Body.String())
	}
	deploymentGrants, err := h.acl.List(ctx, "personal:U1", "deployment:deployment-scope")
	if err != nil || len(deploymentGrants) != 1 || deploymentGrants[0].GranteeScopeID != "personal:U2" || deploymentGrants[0].Permission != "write" {
		t.Fatalf("deployment share grants=%#v err=%v", deploymentGrants, err)
	}
	deploymentShareSource := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader(deploymentShareBody))
	signSourceRequest(t, deploymentShareSource, cfg.Auth.SourceSigningSecret, deploymentShareBody)
	deploymentShareSourceResponse := httptest.NewRecorder()
	h.serveOwned(deploymentShareSourceResponse, deploymentShareSource)
	if deploymentShareSourceResponse.Code != http.StatusForbidden || !bytes.Contains(deploymentShareSourceResponse.Body.Bytes(), []byte(`sharing requires an agent capability token`)) {
		t.Fatalf("source deployment share=%d %s", deploymentShareSourceResponse.Code, deploymentShareSourceResponse.Body.String())
	}
	deploymentShareOther := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader(deploymentShareBody))
	deploymentShareOther.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U2", "personal:U2"))
	deploymentShareOtherResponse := httptest.NewRecorder()
	h.serveOwned(deploymentShareOtherResponse, deploymentShareOther)
	if deploymentShareOtherResponse.Code != http.StatusForbidden || !bytes.Contains(deploymentShareOtherResponse.Body.Bytes(), []byte("only the owner can change who can reach")) || !bytes.Contains(deploymentShareOtherResponse.Body.Bytes(), []byte("scope-dashboard")) {
		t.Fatalf("non-owner deployment share=%d %s", deploymentShareOtherResponse.Code, deploymentShareOtherResponse.Body.String())
	}
	deploymentOrgShareBody := []byte(`{"scope":"org","access":"view"}`)
	deploymentOrgShareRequest := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-scope/share", bytes.NewReader(deploymentOrgShareBody))
	deploymentOrgShareRequest.Header.Set(auth.CapabilityHeader, capBody.Token)
	deploymentOrgShareResponse := httptest.NewRecorder()
	h.serveOwned(deploymentOrgShareResponse, deploymentOrgShareRequest)
	if deploymentOrgShareResponse.Code != http.StatusOK || !bytes.Contains(deploymentOrgShareResponse.Body.Bytes(), []byte(`"scope":"org:acme"`)) || !bytes.Contains(deploymentOrgShareResponse.Body.Bytes(), []byte(`"reach":"everyone in acme"`)) || !bytes.Contains(deploymentOrgShareResponse.Body.Bytes(), []byte(`"permission":"read"`)) {
		t.Fatalf("org deployment share=%d %s", deploymentOrgShareResponse.Code, deploymentOrgShareResponse.Body.String())
	}
	var deploymentShareAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action='deploy_share' AND resource='deployment:deployment-scope'").Scan(&deploymentShareAudits); err != nil || deploymentShareAudits != 2 {
		t.Fatalf("deployment share audit count=%d err=%v", deploymentShareAudits, err)
	}
	viewerSessionRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1?viewer=U1&tailTurns=1", nil)
	signSourceRequest(t, viewerSessionRequest, cfg.Auth.SourceSigningSecret, nil)
	viewerSessionResponse := httptest.NewRecorder()
	h.serveOwned(viewerSessionResponse, viewerSessionRequest)
	if viewerSessionResponse.Code != http.StatusOK || !bytes.Contains(viewerSessionResponse.Body.Bytes(), []byte(`"seq":4`)) || bytes.Contains(viewerSessionResponse.Body.Bytes(), []byte(`"seq":3`)) || !bytes.Contains(viewerSessionResponse.Body.Bytes(), []byte(`"earlierEntries":2`)) {
		t.Fatalf("viewer session=%d %s", viewerSessionResponse.Code, viewerSessionResponse.Body.String())
	}
	viewerEntryRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/entries/2?viewer=U1", nil)
	signSourceRequest(t, viewerEntryRequest, cfg.Auth.SourceSigningSecret, nil)
	viewerEntryResponse := httptest.NewRecorder()
	h.serveOwned(viewerEntryResponse, viewerEntryRequest)
	if viewerEntryResponse.Code != http.StatusOK || !bytes.Contains(viewerEntryResponse.Body.Bytes(), []byte(`"text":"I can help"`)) {
		t.Fatalf("viewer entry=%d %s", viewerEntryResponse.Code, viewerEntryResponse.Body.String())
	}
	agentConversationRequest := httptest.NewRequest(http.MethodGet, "/v1/conversations/session-1", nil)
	agentConversationRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	agentConversationResponse := httptest.NewRecorder()
	h.serveOwned(agentConversationResponse, agentConversationRequest)
	if agentConversationResponse.Code != http.StatusOK || !bytes.Contains(agentConversationResponse.Body.Bytes(), []byte(`"seq":4`)) || bytes.Contains(agentConversationResponse.Body.Bytes(), []byte(`"seq":3`)) {
		t.Fatalf("agent conversation=%d %s", agentConversationResponse.Code, agentConversationResponse.Body.String())
	}
	agentSinceRequest := httptest.NewRequest(http.MethodGet, "/v1/conversations/session-1?sinceSeq=0", nil)
	agentSinceRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	agentSinceResponse := httptest.NewRecorder()
	h.serveOwned(agentSinceResponse, agentSinceRequest)
	if agentSinceResponse.Code != http.StatusBadRequest || !bytes.Contains(agentSinceResponse.Body.Bytes(), []byte(`agent transcript paging supports tailTurns and beforeSeq`)) {
		t.Fatalf("agent conversation since=%d %s", agentSinceResponse.Code, agentSinceResponse.Body.String())
	}
	viewerPatchBody := []byte(`{"principalId":"U1","title":"  Rollout plan  ","pinned":true,"color":"#AABBCC"}`)
	viewerPatchRequest := httptest.NewRequest(http.MethodPost, "/v1/sessions/session-1", bytes.NewReader(viewerPatchBody))
	signSourceRequest(t, viewerPatchRequest, cfg.Auth.SourceSigningSecret, viewerPatchBody)
	viewerPatchResponse := httptest.NewRecorder()
	h.serveOwned(viewerPatchResponse, viewerPatchRequest)
	if viewerPatchResponse.Code != http.StatusOK || !bytes.Contains(viewerPatchResponse.Body.Bytes(), []byte(`"title":"Rollout plan"`)) || !bytes.Contains(viewerPatchResponse.Body.Bytes(), []byte(`"pinned":true`)) || !bytes.Contains(viewerPatchResponse.Body.Bytes(), []byte(`"color":"#aabbcc"`)) {
		t.Fatalf("viewer patch=%d %s", viewerPatchResponse.Code, viewerPatchResponse.Body.String())
	}
	agentPatchBody := []byte(`{"archived":true}`)
	agentPatchRequest := httptest.NewRequest(http.MethodPost, "/v1/conversations/session-1", bytes.NewReader(agentPatchBody))
	agentPatchRequest.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	agentPatchResponse := httptest.NewRecorder()
	h.serveOwned(agentPatchResponse, agentPatchRequest)
	if agentPatchResponse.Code != http.StatusOK || !bytes.Contains(agentPatchResponse.Body.Bytes(), []byte(`"archived":true`)) || !bytes.Contains(agentPatchResponse.Body.Bytes(), []byte(`"color":"#aabbcc"`)) {
		t.Fatalf("agent patch=%d %s", agentPatchResponse.Code, agentPatchResponse.Body.String())
	}
	badViewerEntryRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/entries/-1?viewer=U1", nil)
	signSourceRequest(t, badViewerEntryRequest, cfg.Auth.SourceSigningSecret, nil)
	badViewerEntryResponse := httptest.NewRecorder()
	h.serveOwned(badViewerEntryResponse, badViewerEntryRequest)
	if badViewerEntryResponse.Code != http.StatusBadRequest || !bytes.Contains(badViewerEntryResponse.Body.Bytes(), []byte(`seq must be a non-negative integer`)) {
		t.Fatalf("bad viewer entry=%d %s", badViewerEntryResponse.Code, badViewerEntryResponse.Body.String())
	}
	cronListRequest := httptest.NewRequest(http.MethodGet, "/v1/crons", nil)
	signSourceRequest(t, cronListRequest, cfg.Auth.SourceSigningSecret, nil)
	cronListResponse := httptest.NewRecorder()
	h.serveOwned(cronListResponse, cronListRequest)
	if cronListResponse.Code != http.StatusOK || !bytes.Contains(cronListResponse.Body.Bytes(), []byte(`"title":"Deploy reminder"`)) || bytes.Contains(cronListResponse.Body.Bytes(), []byte(`"fireLog"`)) {
		t.Fatalf("cron list=%d %s", cronListResponse.Code, cronListResponse.Body.String())
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-visible", []byte(`{"id":"cron-visible","title":"Visible reminder","ownerScopeId":"personal:U2","owner":"U2","createdBy":"U2","enabled":true,"archived":false,"schedule":{"everyMs":60000},"members":[{"id":"U1"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-private", []byte(`{"id":"cron-private","title":"Private reminder","ownerScopeId":"personal:U2","owner":"U2","createdBy":"U2","enabled":true,"archived":false,"schedule":{"everyMs":60000}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := data.NewCronRepository(pg).Put(ctx, "cron-consent", []byte(`{"id":"cron-consent","title":"Recipient consent","ownerScopeId":"personal:U2","owner":"U2","createdBy":"U2","enabled":true,"archived":false,"schedule":{"everyMs":60000},"recipientConsent":{"recipientId":"U1","status":"pending","requestedAt":1}}`)); err != nil {
		t.Fatal(err)
	}
	capabilityCronList := httptest.NewRequest(http.MethodGet, "/v1/crons", nil)
	capabilityCronList.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	capabilityCronListResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronListResponse, capabilityCronList)
	if capabilityCronListResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronListResponse.Body.Bytes(), []byte(`"id":"cron-1"`)) || !bytes.Contains(capabilityCronListResponse.Body.Bytes(), []byte(`"id":"cron-visible"`)) || bytes.Contains(capabilityCronListResponse.Body.Bytes(), []byte(`"id":"cron-private"`)) {
		t.Fatalf("capability cron list=%d %s", capabilityCronListResponse.Code, capabilityCronListResponse.Body.String())
	}
	capabilityVisibleCron := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-visible", nil)
	capabilityVisibleCron.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	capabilityVisibleCronResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityVisibleCronResponse, capabilityVisibleCron)
	if capabilityVisibleCronResponse.Code != http.StatusOK || !bytes.Contains(capabilityVisibleCronResponse.Body.Bytes(), []byte(`"id":"cron-visible"`)) {
		t.Fatalf("capability visible cron=%d %s", capabilityVisibleCronResponse.Code, capabilityVisibleCronResponse.Body.String())
	}
	capabilityVisibleCronRuns := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-visible/runs", nil)
	capabilityVisibleCronRuns.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	capabilityVisibleCronRunsResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityVisibleCronRunsResponse, capabilityVisibleCronRuns)
	if capabilityVisibleCronRunsResponse.Code != http.StatusForbidden || !bytes.Contains(capabilityVisibleCronRunsResponse.Body.Bytes(), []byte(`"message":"not your cron"`)) {
		t.Fatalf("capability visible cron runs=%d %s", capabilityVisibleCronRunsResponse.Code, capabilityVisibleCronRunsResponse.Body.String())
	}
	capabilityPrivateCron := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-private", nil)
	capabilityPrivateCron.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	capabilityPrivateCronResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityPrivateCronResponse, capabilityPrivateCron)
	if capabilityPrivateCronResponse.Code != http.StatusForbidden || !bytes.Contains(capabilityPrivateCronResponse.Body.Bytes(), []byte(`"message":"not your cron"`)) {
		t.Fatalf("capability private cron=%d %s", capabilityPrivateCronResponse.Code, capabilityPrivateCronResponse.Body.String())
	}
	consentBody := []byte(`{"decision":"accept"}`)
	foreignConsent := httptest.NewRequest(http.MethodPost, "/v1/triggers/cron-consent/consent", bytes.NewReader(consentBody))
	foreignConsent.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U2", "personal:U2"))
	foreignConsentResponse := httptest.NewRecorder()
	h.ServeHTTP(foreignConsentResponse, foreignConsent)
	if foreignConsentResponse.Code != http.StatusForbidden || !bytes.Contains(foreignConsentResponse.Body.Bytes(), []byte(`only the delivery recipient`)) {
		t.Fatalf("foreign trigger consent=%d %s", foreignConsentResponse.Code, foreignConsentResponse.Body.String())
	}
	triggerConsent := httptest.NewRequest(http.MethodPost, "/v1/triggers/cron-consent/consent", bytes.NewReader(consentBody))
	triggerConsent.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	triggerConsentResponse := httptest.NewRecorder()
	h.ServeHTTP(triggerConsentResponse, triggerConsent)
	if triggerConsentResponse.Code != http.StatusOK || !bytes.Contains(triggerConsentResponse.Body.Bytes(), []byte(`"status":"accepted"`)) || !bytes.Contains(triggerConsentResponse.Body.Bytes(), []byte(`"decidedAt":`)) {
		t.Fatalf("trigger consent=%d %s", triggerConsentResponse.Code, triggerConsentResponse.Body.String())
	}
	declineConsentBody := []byte(`{"decision":"decline"}`)
	declineConsent := httptest.NewRequest(http.MethodPost, "/v1/triggers/cron-consent/consent", bytes.NewReader(declineConsentBody))
	declineConsent.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	declineConsentResponse := httptest.NewRecorder()
	h.ServeHTTP(declineConsentResponse, declineConsent)
	if declineConsentResponse.Code != http.StatusOK || !bytes.Contains(declineConsentResponse.Body.Bytes(), []byte(`"status":"declined"`)) {
		t.Fatalf("decline trigger consent=%d %s", declineConsentResponse.Code, declineConsentResponse.Body.String())
	}
	sourceConsent := httptest.NewRequest(http.MethodPost, "/v1/triggers/cron-consent/consent", bytes.NewReader(consentBody))
	signSourceRequest(t, sourceConsent, cfg.Auth.SourceSigningSecret, consentBody)
	sourceConsentResponse := httptest.NewRecorder()
	h.ServeHTTP(sourceConsentResponse, sourceConsent)
	if sourceConsentResponse.Code != http.StatusForbidden || !bytes.Contains(sourceConsentResponse.Body.Bytes(), []byte(`consent requires an agent capability token`)) {
		t.Fatalf("source trigger consent=%d %s", sourceConsentResponse.Code, sourceConsentResponse.Body.String())
	}
	capabilityCronRuns := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-1/runs?limit=1", nil)
	capabilityCronRuns.Header.Set(auth.CapabilityHeader, testCapabilityToken(t, cfg.Auth.CapabilitySecret, "U1", "personal:U1"))
	capabilityCronRunsResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronRunsResponse, capabilityCronRuns)
	if capabilityCronRunsResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronRunsResponse.Body.Bytes(), []byte(`"fireKey":"old"`)) {
		t.Fatalf("capability cron runs=%d %s", capabilityCronRunsResponse.Code, capabilityCronRunsResponse.Body.String())
	}
	capabilityCronDisable := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-1/disable", nil)
	capabilityCronDisable.Header.Set(auth.CapabilityHeader, capBody.Token)
	capabilityCronDisableResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronDisableResponse, capabilityCronDisable)
	if capabilityCronDisableResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronDisableResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("capability cron disable=%d %s", capabilityCronDisableResponse.Code, capabilityCronDisableResponse.Body.String())
	}
	cronDisableRequest := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-1/disable", nil)
	signSourceRequest(t, cronDisableRequest, cfg.Auth.SourceSigningSecret, nil)
	cronDisableResponse := httptest.NewRecorder()
	h.serveOwned(cronDisableResponse, cronDisableRequest)
	if cronDisableResponse.Code != http.StatusOK {
		t.Fatalf("cron disable=%d %s", cronDisableResponse.Code, cronDisableResponse.Body.String())
	}
	cronGetRequest := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-1", nil)
	signSourceRequest(t, cronGetRequest, cfg.Auth.SourceSigningSecret, nil)
	cronGetResponse := httptest.NewRecorder()
	h.serveOwned(cronGetResponse, cronGetRequest)
	if cronGetResponse.Code != http.StatusOK || !bytes.Contains(cronGetResponse.Body.Bytes(), []byte(`"enabled":false`)) {
		t.Fatalf("cron get=%d %s", cronGetResponse.Code, cronGetResponse.Body.String())
	}
	cronRunsRequest := httptest.NewRequest(http.MethodGet, "/v1/crons/cron-1/runs?limit=1", nil)
	signSourceRequest(t, cronRunsRequest, cfg.Auth.SourceSigningSecret, nil)
	cronRunsResponse := httptest.NewRecorder()
	h.serveOwned(cronRunsResponse, cronRunsRequest)
	if cronRunsResponse.Code != http.StatusOK || !bytes.Contains(cronRunsResponse.Body.Bytes(), []byte(`"fireKey":"old"`)) || !bytes.Contains(cronRunsResponse.Body.Bytes(), []byte(`"total":1`)) {
		t.Fatalf("cron runs=%d %s", cronRunsResponse.Code, cronRunsResponse.Body.String())
	}
	cronPatchBody := []byte(`{"title":"  Updated   reminder  "}`)
	cronPatchRequest := httptest.NewRequest(http.MethodPatch, "/v1/crons/cron-1", bytes.NewReader(cronPatchBody))
	signSourceRequest(t, cronPatchRequest, cfg.Auth.SourceSigningSecret, cronPatchBody)
	cronPatchResponse := httptest.NewRecorder()
	h.serveOwned(cronPatchResponse, cronPatchRequest)
	if cronPatchResponse.Code != http.StatusOK || !bytes.Contains(cronPatchResponse.Body.Bytes(), []byte(`"title":"Updated reminder"`)) {
		t.Fatalf("cron patch=%d %s", cronPatchResponse.Code, cronPatchResponse.Body.String())
	}
	// Interval schedule edits are now Go-owned. The Node Croner path still owns
	// calendar schedules, but this verifies the shared row contains the same
	// normalized first/next fire timestamps the Node scheduler consumes.
	schedulePatchBody := []byte(`{"schedule":{"everyMs":120000}}`)
	schedulePatchRequest := httptest.NewRequest(http.MethodPatch, "/v1/crons/cron-1", bytes.NewReader(schedulePatchBody))
	signSourceRequest(t, schedulePatchRequest, cfg.Auth.SourceSigningSecret, schedulePatchBody)
	schedulePatchResponse := httptest.NewRecorder()
	schedulePatchStarted := time.Now().UnixMilli()
	h.ServeHTTP(schedulePatchResponse, schedulePatchRequest)
	if schedulePatchResponse.Code != http.StatusOK {
		t.Fatalf("interval cron patch=%d %s", schedulePatchResponse.Code, schedulePatchResponse.Body.String())
	}
	var scheduledCron struct {
		Cron struct {
			Schedule struct {
				EveryMS     int64 `json:"everyMs"`
				FirstFireAt int64 `json:"firstFireAt"`
			} `json:"schedule"`
			NextFireAt int64 `json:"nextFireAt"`
		} `json:"cron"`
	}
	if err := json.Unmarshal(schedulePatchResponse.Body.Bytes(), &scheduledCron); err != nil {
		t.Fatal(err)
	}
	if scheduledCron.Cron.Schedule.EveryMS != 120000 || scheduledCron.Cron.Schedule.FirstFireAt != scheduledCron.Cron.NextFireAt || scheduledCron.Cron.NextFireAt < schedulePatchStarted+119000 {
		t.Fatalf("normalized interval cron=%s", schedulePatchResponse.Body.String())
	}
	var storedNextFireAt int64
	if err := pg.Pool.QueryRow(ctx, "SELECT next_fire_at FROM crons WHERE id='cron-1'").Scan(&storedNextFireAt); err != nil || storedNextFireAt != scheduledCron.Cron.NextFireAt {
		t.Fatalf("stored interval next fire=%d response=%d err=%v", storedNextFireAt, scheduledCron.Cron.NextFireAt, err)
	}
	fastScheduleBody := []byte(`{"schedule":{"everyMs":1}}`)
	fastScheduleRequest := httptest.NewRequest(http.MethodPatch, "/v1/crons/cron-1", bytes.NewReader(fastScheduleBody))
	signSourceRequest(t, fastScheduleRequest, cfg.Auth.SourceSigningSecret, fastScheduleBody)
	fastScheduleResponse := httptest.NewRecorder()
	h.ServeHTTP(fastScheduleResponse, fastScheduleRequest)
	if fastScheduleResponse.Code != http.StatusBadRequest || !bytes.Contains(fastScheduleResponse.Body.Bytes(), []byte(`"error":"cron_update_failed"`)) || !bytes.Contains(fastScheduleResponse.Body.Bytes(), []byte(`at least 60000ms`)) {
		t.Fatalf("fast interval cron patch=%d %s", fastScheduleResponse.Code, fastScheduleResponse.Body.String())
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT next_fire_at FROM crons WHERE id='cron-1'").Scan(&storedNextFireAt); err != nil || storedNextFireAt != scheduledCron.Cron.NextFireAt {
		t.Fatalf("rejected interval patch changed next fire=%d err=%v", storedNextFireAt, err)
	}
	capabilityCronPatchBody := []byte(`{"title":"  Owner   updated reminder  ","enabled":false}`)
	capabilityCronPatch := httptest.NewRequest(http.MethodPatch, "/v1/crons/cron-cap-patch", bytes.NewReader(capabilityCronPatchBody))
	capabilityCronPatch.Header.Set(auth.CapabilityHeader, capBody.Token)
	capabilityCronPatchResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronPatchResponse, capabilityCronPatch)
	if capabilityCronPatchResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronPatchResponse.Body.Bytes(), []byte(`"title":"Owner updated reminder"`)) || !bytes.Contains(capabilityCronPatchResponse.Body.Bytes(), []byte(`"enabled":false`)) {
		t.Fatalf("capability cron patch=%d %s", capabilityCronPatchResponse.Code, capabilityCronPatchResponse.Body.String())
	}
	var capabilityCronPatchAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log WHERE action='cron_update' AND resource='cron-cap-patch'").Scan(&capabilityCronPatchAudits); err != nil || capabilityCronPatchAudits != 1 {
		t.Fatalf("capability cron patch audits=%d err=%v", capabilityCronPatchAudits, err)
	}
	retargetToken := testCapabilityTokenWithClaims(t, cfg.Auth.CapabilitySecret, auth.Claims{
		ActorID: "U1", ScopeID: "personal:U1", Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
		Destinations: auth.DestinationCandidates{{Key: "slack-c1", Label: "#general", Type: "slack", Target: "C1", AudienceScopeID: "channel:C1"}},
	})
	capabilityCronRetarget := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-cap-retarget/destination", bytes.NewReader([]byte(`{"destinationKey":"slack-c1"}`)))
	capabilityCronRetarget.Header.Set(auth.CapabilityHeader, retargetToken)
	capabilityCronRetargetResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronRetargetResponse, capabilityCronRetarget)
	if capabilityCronRetargetResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronRetargetResponse.Body.Bytes(), []byte(`"target":"C1"`)) || !bytes.Contains(capabilityCronRetargetResponse.Body.Bytes(), []byte(`"audienceScopeId":"channel:C1"`)) {
		t.Fatalf("capability cron retarget=%d %s", capabilityCronRetargetResponse.Code, capabilityCronRetargetResponse.Body.String())
	}
	unknownCapabilityCronDestination := httptest.NewRequest(http.MethodPost, "/v1/crons/cron-cap-retarget/destination", bytes.NewReader([]byte(`{"destinationKey":"missing"}`)))
	unknownCapabilityCronDestination.Header.Set(auth.CapabilityHeader, retargetToken)
	unknownCapabilityCronDestinationResponse := httptest.NewRecorder()
	h.ServeHTTP(unknownCapabilityCronDestinationResponse, unknownCapabilityCronDestination)
	if unknownCapabilityCronDestinationResponse.Code != http.StatusBadRequest || !bytes.Contains(unknownCapabilityCronDestinationResponse.Body.Bytes(), []byte(`"error":"unknown_destination"`)) {
		t.Fatalf("unknown capability cron destination=%d %s", unknownCapabilityCronDestinationResponse.Code, unknownCapabilityCronDestinationResponse.Body.String())
	}
	var capabilityCronRetargetAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log WHERE action='cron_retarget' AND resource='cron-cap-retarget'").Scan(&capabilityCronRetargetAudits); err != nil || capabilityCronRetargetAudits != 1 {
		t.Fatalf("capability cron retarget audits=%d err=%v", capabilityCronRetargetAudits, err)
	}
	capabilityCronDelete := httptest.NewRequest(http.MethodDelete, "/v1/crons/cron-cap-delete", nil)
	capabilityCronDelete.Header.Set(auth.CapabilityHeader, capBody.Token)
	capabilityCronDeleteResponse := httptest.NewRecorder()
	h.ServeHTTP(capabilityCronDeleteResponse, capabilityCronDelete)
	if capabilityCronDeleteResponse.Code != http.StatusOK || !bytes.Contains(capabilityCronDeleteResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("capability cron delete=%d %s", capabilityCronDeleteResponse.Code, capabilityCronDeleteResponse.Body.String())
	}
	var deletedCronCount, capabilityCronDeleteAudits int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM crons WHERE id='cron-cap-delete'").Scan(&deletedCronCount); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log WHERE action='cron_delete' AND resource='cron-cap-delete'").Scan(&capabilityCronDeleteAudits); err != nil {
		t.Fatal(err)
	}
	if deletedCronCount != 0 || capabilityCronDeleteAudits != 1 {
		t.Fatalf("capability cron deletion count=%d audits=%d", deletedCronCount, capabilityCronDeleteAudits)
	}
	cronDeleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/crons/cron-1", nil)
	signSourceRequest(t, cronDeleteRequest, cfg.Auth.SourceSigningSecret, nil)
	cronDeleteResponse := httptest.NewRecorder()
	h.serveOwned(cronDeleteResponse, cronDeleteRequest)
	if cronDeleteResponse.Code != http.StatusOK || !bytes.Contains(cronDeleteResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("cron delete=%d %s", cronDeleteResponse.Code, cronDeleteResponse.Body.String())
	}
	activeRunRequest := httptest.NewRequest(http.MethodGet, "/v1/runs?threadRef=thread-1", nil)
	signSourceRequest(t, activeRunRequest, cfg.Auth.SourceSigningSecret, nil)
	activeRunResponse := httptest.NewRecorder()
	h.serveOwned(activeRunResponse, activeRunRequest)
	if activeRunResponse.Code != http.StatusOK || !bytes.Contains(activeRunResponse.Body.Bytes(), []byte(`"runId":"run-1"`)) {
		t.Fatalf("active run=%d %s", activeRunResponse.Code, activeRunResponse.Body.String())
	}
	deliveryStateBody := []byte(`{"editRef":"C1:3.0"}`)
	deliveryStateRequest := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/delivery-state", bytes.NewReader(deliveryStateBody))
	signSourceRequest(t, deliveryStateRequest, cfg.Auth.SourceSigningSecret, deliveryStateBody)
	deliveryStateResponse := httptest.NewRecorder()
	h.serveOwned(deliveryStateResponse, deliveryStateRequest)
	if deliveryStateResponse.Code != http.StatusOK || !bytes.Contains(deliveryStateResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("delivery state=%d %s", deliveryStateResponse.Code, deliveryStateResponse.Body.String())
	}
	var deliveryState, destination string
	if err := pg.Pool.QueryRow(ctx, "SELECT delivery_state FROM runs WHERE id=$1", "run-1").Scan(&deliveryState); err != nil {
		t.Fatal(err)
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT destination::text FROM deliveries WHERE id=$1", "delivery-1").Scan(&destination); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deliveryState, `"editRef":"C1:3.0"`) || !strings.Contains(destination, `"editRef": "C1:3.0"`) {
		t.Fatalf("delivery state persistence mismatch state=%s destination=%s", deliveryState, destination)
	}
	signalBody := []byte(`{"kind":"steer","text":"please focus on rollback"}`)
	signalRequest := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/signal", bytes.NewReader(signalBody))
	signSourceRequest(t, signalRequest, cfg.Auth.SourceSigningSecret, signalBody)
	signalResponse := httptest.NewRecorder()
	h.serveOwned(signalResponse, signalRequest)
	if signalResponse.Code != http.StatusOK || !bytes.Contains(signalResponse.Body.Bytes(), []byte(`"accepted":true`)) {
		t.Fatalf("run signal=%d %s", signalResponse.Code, signalResponse.Body.String())
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO runs(id,session_id,status,request,attempts,max_attempts,created_at,finished_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", "run-terminal", "thread-1", "done", `{}`, 1, 3, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	terminalSignalBody := []byte(`{"kind":"abort"}`)
	terminalSignalRequest := httptest.NewRequest(http.MethodPost, "/v1/runs/run-terminal/signal", bytes.NewReader(terminalSignalBody))
	signSourceRequest(t, terminalSignalRequest, cfg.Auth.SourceSigningSecret, terminalSignalBody)
	terminalSignalResponse := httptest.NewRecorder()
	h.serveOwned(terminalSignalResponse, terminalSignalRequest)
	if terminalSignalResponse.Code != http.StatusConflict || !bytes.Contains(terminalSignalResponse.Body.Bytes(), []byte(`"reason":"terminal"`)) {
		t.Fatalf("terminal run signal=%d %s", terminalSignalResponse.Code, terminalSignalResponse.Body.String())
	}
	pendingDeliveryRequest := httptest.NewRequest(http.MethodGet, "/v1/deliveries?type=slack&claimMs=5000", nil)
	signSourceRequest(t, pendingDeliveryRequest, cfg.Auth.SourceSigningSecret, nil)
	pendingDeliveryResponse := httptest.NewRecorder()
	h.serveOwned(pendingDeliveryResponse, pendingDeliveryRequest)
	if pendingDeliveryResponse.Code != http.StatusOK || !bytes.Contains(pendingDeliveryResponse.Body.Bytes(), []byte(`"id":"delivery-claim"`)) || !bytes.Contains(pendingDeliveryResponse.Body.Bytes(), []byte(`"deliveredAt":null`)) {
		t.Fatalf("pending deliveries=%d %s", pendingDeliveryResponse.Code, pendingDeliveryResponse.Body.String())
	}
	var claimExpiresAt *int64
	if err := pg.Pool.QueryRow(ctx, "SELECT claim_expires_at FROM deliveries WHERE id=$1", "delivery-claim").Scan(&claimExpiresAt); err != nil {
		t.Fatal(err)
	}
	if claimExpiresAt == nil {
		t.Fatal("pending delivery was not claimed")
	}
	ackClaimBody := []byte(`{"slackApiMs":11}`)
	ackClaimRequest := httptest.NewRequest(http.MethodPost, "/v1/deliveries/delivery-claim/ack", bytes.NewReader(ackClaimBody))
	signSourceRequest(t, ackClaimRequest, cfg.Auth.SourceSigningSecret, ackClaimBody)
	ackClaimResponse := httptest.NewRecorder()
	h.serveOwned(ackClaimResponse, ackClaimRequest)
	if ackClaimResponse.Code != http.StatusOK || !bytes.Contains(ackClaimResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("delivery ack=%d %s", ackClaimResponse.Code, ackClaimResponse.Body.String())
	}
	var claimedSlackMS *int
	if err := pg.Pool.QueryRow(ctx, "SELECT slack_api_ms FROM deliveries WHERE id=$1", "delivery-claim").Scan(&claimedSlackMS); err != nil {
		t.Fatal(err)
	}
	if claimedSlackMS == nil || *claimedSlackMS != 11 {
		t.Fatalf("delivery ack slack api ms=%v", claimedSlackMS)
	}
	principalAckBody := []byte(`{"recipientThreadRef":"dm-thread-U2","slackApiMs":9}`)
	principalAckRequest := httptest.NewRequest(http.MethodPost, "/v1/deliveries/delivery-principal/ack", bytes.NewReader(principalAckBody))
	signSourceRequest(t, principalAckRequest, cfg.Auth.SourceSigningSecret, principalAckBody)
	principalAckResponse := httptest.NewRecorder()
	h.serveOwned(principalAckResponse, principalAckRequest)
	if principalAckResponse.Code != http.StatusOK || !bytes.Contains(principalAckResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("principal delivery ack=%d %s", principalAckResponse.Code, principalAckResponse.Body.String())
	}
	var recipientThread string
	var principalSlackMS *int
	if err := pg.Pool.QueryRow(ctx, "SELECT recipient_thread_ref,slack_api_ms FROM deliveries WHERE id=$1", "delivery-principal").Scan(&recipientThread, &principalSlackMS); err != nil {
		t.Fatal(err)
	}
	if recipientThread != "dm-thread-U2" || principalSlackMS != nil {
		t.Fatalf("principal ack state thread=%q slack=%v", recipientThread, principalSlackMS)
	}
	var principalSessionID string
	if err := pg.Pool.QueryRow(ctx, "SELECT id FROM sessions WHERE thread_ref=$1 AND type='dm' AND scope_id='personal:U2'", "dm-thread-U2").Scan(&principalSessionID); err != nil {
		t.Fatal(err)
	}
	var participantCount int
	if err := pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM participants WHERE session_id=$1 AND principal_id='U2' AND valid_to IS NULL", principalSessionID).Scan(&participantCount); err != nil {
		t.Fatal(err)
	}
	if participantCount != 1 {
		t.Fatalf("principal participant count=%d", participantCount)
	}
	ackByKeyBody := []byte(`{"idempotencyKey":"run:run-1"}`)
	ackByKeyRequest := httptest.NewRequest(http.MethodPost, "/v1/deliveries/ack-by-key", bytes.NewReader(ackByKeyBody))
	signSourceRequest(t, ackByKeyRequest, cfg.Auth.SourceSigningSecret, ackByKeyBody)
	ackByKeyResponse := httptest.NewRecorder()
	h.serveOwned(ackByKeyResponse, ackByKeyRequest)
	if ackByKeyResponse.Code != http.StatusOK || !bytes.Contains(ackByKeyResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("ack by key=%d %s", ackByKeyResponse.Code, ackByKeyResponse.Body.String())
	}
	var deliveredAt *int64
	if err := pg.Pool.QueryRow(ctx, "SELECT delivered_at FROM deliveries WHERE id=$1", "delivery-1").Scan(&deliveredAt); err != nil {
		t.Fatal(err)
	}
	if deliveredAt == nil {
		t.Fatal("ack by key did not mark the delivery as delivered")
	}
	egressAuditBody := []byte(`{"records":[{"host":"relay.example","verdict":"ok","scopeLabel":"project:p1","port":443,"via":"proxy-a"},{"host":false}]}`)
	egressAuditRequest := httptest.NewRequest(http.MethodPost, "/v1/egress-audit", bytes.NewReader(egressAuditBody))
	signSourceRequest(t, egressAuditRequest, cfg.Auth.SourceSigningSecret, egressAuditBody)
	egressAuditResponse := httptest.NewRecorder()
	h.serveOwned(egressAuditResponse, egressAuditRequest)
	if egressAuditResponse.Code != http.StatusOK || !bytes.Contains(egressAuditResponse.Body.Bytes(), []byte(`"accepted":1`)) || !bytes.Contains(egressAuditResponse.Body.Bytes(), []byte(`"rejected":1`)) {
		t.Fatalf("egress audit=%d %s", egressAuditResponse.Code, egressAuditResponse.Body.String())
	}
	var auditHost string
	if err := pg.Pool.QueryRow(ctx, "SELECT host FROM egress_events WHERE source='proxy' ORDER BY id DESC LIMIT 1").Scan(&auditHost); err != nil {
		t.Fatal(err)
	}
	if auditHost != "relay.example" {
		t.Fatalf("egress audit host=%q", auditHost)
	}
	brokerExpiry := time.Now().Add(time.Hour).UnixMilli()
	brokerBody := []byte(fmt.Sprintf(`{"ids":["broker-first","broker-fallback"],"expiresAtMs":%d}`, brokerExpiry))
	brokerRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/broker/claim", bytes.NewReader(brokerBody))
	signSourceRequest(t, brokerRequest, cfg.Auth.SourceSigningSecret, brokerBody)
	brokerResponse := httptest.NewRecorder()
	h.serveOwned(brokerResponse, brokerRequest)
	if brokerResponse.Code != http.StatusOK || !bytes.Contains(brokerResponse.Body.Bytes(), []byte(`"claimed":"broker-first"`)) {
		t.Fatalf("broker claim=%d %s", brokerResponse.Code, brokerResponse.Body.String())
	}
	brokerRetryRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/broker/claim?retry=1", bytes.NewReader(brokerBody))
	signSourceRequest(t, brokerRetryRequest, cfg.Auth.SourceSigningSecret, brokerBody)
	brokerRetryResponse := httptest.NewRecorder()
	h.serveOwned(brokerRetryResponse, brokerRetryRequest)
	if brokerRetryResponse.Code != http.StatusOK || !bytes.Contains(brokerRetryResponse.Body.Bytes(), []byte(`"claimed":"broker-fallback"`)) {
		t.Fatalf("broker retry=%d %s", brokerRetryResponse.Code, brokerRetryResponse.Body.String())
	}
	metricsBody := []byte(`{"deliverMs":23,"slackInflightMs":7}`)
	metricsRequest := httptest.NewRequest(http.MethodPost, "/v1/turns/run-1/metrics", bytes.NewReader(metricsBody))
	signSourceRequest(t, metricsRequest, cfg.Auth.SourceSigningSecret, metricsBody)
	metricsResponse := httptest.NewRecorder()
	h.serveOwned(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK || !bytes.Contains(metricsResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("turn metrics=%d %s", metricsResponse.Code, metricsResponse.Body.String())
	}
	var deliverMS, slackInflightMS int
	if err := pg.Pool.QueryRow(ctx, "SELECT deliver_ms,slack_inflight_ms FROM turn_metrics WHERE run_id=$1", "run-1").Scan(&deliverMS, &slackInflightMS); err != nil {
		t.Fatal(err)
	}
	if deliverMS != 23 || slackInflightMS != 7 {
		t.Fatalf("turn metric values deliver=%d slack=%d", deliverMS, slackInflightMS)
	}
	shadowRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/deliveries/shadow?scope=project:p1", nil)
	shadowRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, shadowRequest, cfg.Auth.SourceSigningSecret, nil)
	shadowResponse := httptest.NewRecorder()
	h.serveOwned(shadowResponse, shadowRequest)
	if shadowResponse.Code != http.StatusOK || !bytes.Contains(shadowResponse.Body.Bytes(), []byte(`"deliveryId":"shadow-1"`)) || !bytes.Contains(shadowResponse.Body.Bytes(), []byte(`"kind":"cron"`)) || !bytes.Contains(shadowResponse.Body.Bytes(), []byte(`"cron":null`)) {
		t.Fatalf("shadow deliveries=%d %s", shadowResponse.Code, shadowResponse.Body.String())
	}
	directoryRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/directory?q=ali", nil)
	directoryRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, directoryRequest, cfg.Auth.SourceSigningSecret, nil)
	directoryResponse := httptest.NewRecorder()
	h.serveOwned(directoryResponse, directoryRequest)
	if directoryResponse.Code != http.StatusOK || !bytes.Contains(directoryResponse.Body.Bytes(), []byte(`"displayName":"Alice"`)) {
		t.Fatalf("directory=%d %s", directoryResponse.Code, directoryResponse.Body.String())
	}
	whoamiRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/whoami", nil)
	whoamiRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, whoamiRequest, cfg.Auth.SourceSigningSecret, nil)
	whoamiResponse := httptest.NewRecorder()
	h.serveOwned(whoamiResponse, whoamiRequest)
	if whoamiResponse.Code != http.StatusOK || !bytes.Contains(whoamiResponse.Body.Bytes(), []byte(`"isAdmin":true`)) || !bytes.Contains(whoamiResponse.Body.Bytes(), []byte(`"permissions":["admin"]`)) {
		t.Fatalf("whoami=%d %s", whoamiResponse.Code, whoamiResponse.Body.String())
	}
	mirrorRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/slack-mirror/messages?q=incident", nil)
	mirrorRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, mirrorRequest, cfg.Auth.SourceSigningSecret, nil)
	mirrorResponse := httptest.NewRecorder()
	h.serveOwned(mirrorResponse, mirrorRequest)
	if mirrorResponse.Code != http.StatusOK || !bytes.Contains(mirrorResponse.Body.Bytes(), []byte(`"authorName":"Alice"`)) {
		t.Fatalf("mirror=%d %s", mirrorResponse.Code, mirrorResponse.Body.String())
	}
	ambientRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/ambient-judgments?container=C1", nil)
	ambientRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, ambientRequest, cfg.Auth.SourceSigningSecret, nil)
	ambientResponse := httptest.NewRecorder()
	h.serveOwned(ambientResponse, ambientRequest)
	if ambientResponse.Code != http.StatusOK || !bytes.Contains(ambientResponse.Body.Bytes(), []byte(`"act":1`)) || bytes.Contains(ambientResponse.Body.Bytes(), []byte(`"prompt"`)) {
		t.Fatalf("ambient list=%d %s", ambientResponse.Code, ambientResponse.Body.String())
	}
	ambientDetailRequest := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/admin/ambient-judgments?id=%d", judgmentID), nil)
	ambientDetailRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, ambientDetailRequest, cfg.Auth.SourceSigningSecret, nil)
	ambientDetailResponse := httptest.NewRecorder()
	h.serveOwned(ambientDetailResponse, ambientDetailRequest)
	if ambientDetailResponse.Code != http.StatusOK || !bytes.Contains(ambientDetailResponse.Body.Bytes(), []byte(`"prompt":"full ambient prompt"`)) {
		t.Fatalf("ambient detail=%d %s", ambientDetailResponse.Code, ambientDetailResponse.Body.String())
	}
	ackRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/ack-emoji-picks?container=C1", nil)
	ackRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, ackRequest, cfg.Auth.SourceSigningSecret, nil)
	ackResponse := httptest.NewRecorder()
	h.serveOwned(ackResponse, ackRequest)
	if ackResponse.Code != http.StatusOK || !bytes.Contains(ackResponse.Body.Bytes(), []byte(`"picked":1`)) || bytes.Contains(ackResponse.Body.Bytes(), []byte(`"candidates"`)) {
		t.Fatalf("ack list=%d %s", ackResponse.Code, ackResponse.Body.String())
	}
	ackDetailRequest := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/admin/ack-emoji-picks?id=%d", pickID), nil)
	ackDetailRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, ackDetailRequest, cfg.Auth.SourceSigningSecret, nil)
	ackDetailResponse := httptest.NewRecorder()
	h.serveOwned(ackDetailResponse, ackDetailRequest)
	if ackDetailResponse.Code != http.StatusOK || !bytes.Contains(ackDetailResponse.Body.Bytes(), []byte(`"candidates":"eyes,white_check_mark"`)) {
		t.Fatalf("ack detail=%d %s", ackDetailResponse.Code, ackDetailResponse.Body.String())
	}
	egressRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/egress?scope=project:p1", nil)
	egressRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, egressRequest, cfg.Auth.SourceSigningSecret, nil)
	egressResponse := httptest.NewRecorder()
	h.serveOwned(egressResponse, egressRequest)
	if egressResponse.Code != http.StatusOK || !bytes.Contains(egressResponse.Body.Bytes(), []byte(`"total":3`)) || !bytes.Contains(egressResponse.Body.Bytes(), []byte(`"denied":1`)) || !bytes.Contains(egressResponse.Body.Bytes(), []byte(`"broker":1`)) || !bytes.Contains(egressResponse.Body.Bytes(), []byte(`"firewall":2`)) || !bytes.Contains(egressResponse.Body.Bytes(), []byte(`"host":"relay.example"`)) {
		t.Fatalf("egress=%d %s", egressResponse.Code, egressResponse.Body.String())
	}
	errorsRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/errors?scope=project:p1&sessionId=session-1", nil)
	errorsRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, errorsRequest, cfg.Auth.SourceSigningSecret, nil)
	errorsResponse := httptest.NewRecorder()
	h.serveOwned(errorsResponse, errorsRequest)
	if errorsResponse.Code != http.StatusOK || !bytes.Contains(errorsResponse.Body.Bytes(), []byte(`"code":"lease_expired"`)) {
		t.Fatalf("errors=%d %s", errorsResponse.Code, errorsResponse.Body.String())
	}
	errorCountRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/errors?scope=project:p1&count=1", nil)
	errorCountRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, errorCountRequest, cfg.Auth.SourceSigningSecret, nil)
	errorCountResponse := httptest.NewRecorder()
	h.serveOwned(errorCountResponse, errorCountRequest)
	if errorCountResponse.Code != http.StatusOK || !bytes.Contains(errorCountResponse.Body.Bytes(), []byte(`"total":1`)) {
		t.Fatalf("error count=%d %s", errorCountResponse.Code, errorCountResponse.Body.String())
	}
	runsRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/runs?scope=project:p1", nil)
	runsRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, runsRequest, cfg.Auth.SourceSigningSecret, nil)
	runsResponse := httptest.NewRecorder()
	h.serveOwned(runsResponse, runsRequest)
	if runsResponse.Code != http.StatusOK || !bytes.Contains(runsResponse.Body.Bytes(), []byte(`"id":"run-1"`)) || !bytes.Contains(runsResponse.Body.Bytes(), []byte(`"active":1`)) {
		t.Fatalf("runs=%d %s", runsResponse.Code, runsResponse.Body.String())
	}
	sessionsRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions?scope=project:p1&category=all&limit=1", nil)
	sessionsRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, sessionsRequest, cfg.Auth.SourceSigningSecret, nil)
	sessionsResponse := httptest.NewRecorder()
	h.serveOwned(sessionsResponse, sessionsRequest)
	if sessionsResponse.Code != http.StatusOK || !bytes.Contains(sessionsResponse.Body.Bytes(), []byte(`"id":"session-cron"`)) || !bytes.Contains(sessionsResponse.Body.Bytes(), []byte(`"category":"background"`)) || !bytes.Contains(sessionsResponse.Body.Bytes(), []byte(`"nextCursor"`)) || !bytes.Contains(sessionsResponse.Body.Bytes(), []byte(`"distinctCrons":1`)) {
		t.Fatalf("sessions=%d %s", sessionsResponse.Code, sessionsResponse.Body.String())
	}
	conversationSessionsRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions?scope=project:p1", nil)
	conversationSessionsRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, conversationSessionsRequest, cfg.Auth.SourceSigningSecret, nil)
	conversationSessionsResponse := httptest.NewRecorder()
	h.serveOwned(conversationSessionsResponse, conversationSessionsRequest)
	if conversationSessionsResponse.Code != http.StatusOK || !bytes.Contains(conversationSessionsResponse.Body.Bytes(), []byte(`"firstMessage":"Need deployment help"`)) || !bytes.Contains(conversationSessionsResponse.Body.Bytes(), []byte(`"origin":null`)) {
		t.Fatalf("conversation sessions=%d %s", conversationSessionsResponse.Code, conversationSessionsResponse.Body.String())
	}
	llmRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions/session-1/llm?scope=project:p1&turnSeq=1", nil)
	llmRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, llmRequest, cfg.Auth.SourceSigningSecret, nil)
	llmResponse := httptest.NewRecorder()
	h.serveOwned(llmResponse, llmRequest)
	if llmResponse.Code != http.StatusOK || !bytes.Contains(llmResponse.Body.Bytes(), []byte(`"id":"llm-1"`)) || !bytes.Contains(llmResponse.Body.Bytes(), []byte(`"ttftMs":12`)) || !bytes.Contains(llmResponse.Body.Bytes(), []byte(`"request":{"messages":[]}`)) {
		t.Fatalf("session LLM=%d %s", llmResponse.Code, llmResponse.Body.String())
	}
	llmMetadataRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions/session-1/llm?scope=project:p1", nil)
	llmMetadataRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, llmMetadataRequest, cfg.Auth.SourceSigningSecret, nil)
	llmMetadataResponse := httptest.NewRecorder()
	h.serveOwned(llmMetadataResponse, llmMetadataRequest)
	if llmMetadataResponse.Code != http.StatusOK || !bytes.Contains(llmMetadataResponse.Body.Bytes(), []byte(`"id":"llm-1"`)) || bytes.Contains(llmMetadataResponse.Body.Bytes(), []byte(`"request"`)) {
		t.Fatalf("session LLM metadata=%d %s", llmMetadataResponse.Code, llmMetadataResponse.Body.String())
	}
	sessionDetailRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions/session-1?scope=project:p1", nil)
	sessionDetailRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, sessionDetailRequest, cfg.Auth.SourceSigningSecret, nil)
	sessionDetailResponse := httptest.NewRecorder()
	h.serveOwned(sessionDetailResponse, sessionDetailRequest)
	if sessionDetailResponse.Code != http.StatusOK || !bytes.Contains(sessionDetailResponse.Body.Bytes(), []byte(`"name":"Alice"`)) || !bytes.Contains(sessionDetailResponse.Body.Bytes(), []byte(`"type":"outbound_delivery"`)) || !bytes.Contains(sessionDetailResponse.Body.Bytes(), []byte(`"shadow":true`)) || !bytes.Contains(sessionDetailResponse.Body.Bytes(), []byte(`"kind":"cron"`)) {
		t.Fatalf("session detail=%d %s", sessionDetailResponse.Code, sessionDetailResponse.Body.String())
	}
	usersRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	usersRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, usersRequest, cfg.Auth.SourceSigningSecret, nil)
	usersResponse := httptest.NewRecorder()
	h.serveOwned(usersResponse, usersRequest)
	if usersResponse.Code != http.StatusOK || !bytes.Contains(usersResponse.Body.Bytes(), []byte(`"principalId":"U1"`)) || !bytes.Contains(usersResponse.Body.Bytes(), []byte(`"turnCount":2`)) || !bytes.Contains(usersResponse.Body.Bytes(), []byte(`"principalId":"admin"`)) || !bytes.Contains(usersResponse.Body.Bytes(), []byte(`"isAdmin":true`)) {
		t.Fatalf("users=%d %s", usersResponse.Code, usersResponse.Body.String())
	}
	adminFilesRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/files?scope=project:p1", nil)
	adminFilesRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, adminFilesRequest, cfg.Auth.SourceSigningSecret, nil)
	adminFilesResponse := httptest.NewRecorder()
	h.serveOwned(adminFilesResponse, adminFilesRequest)
	if adminFilesResponse.Code != http.StatusOK || !bytes.Contains(adminFilesResponse.Body.Bytes(), []byte(`"id":"file-1"`)) || !bytes.Contains(adminFilesResponse.Body.Bytes(), []byte(`"openable":true`)) {
		t.Fatalf("admin files=%d %s", adminFilesResponse.Code, adminFilesResponse.Body.String())
	}
	impersonateBody := []byte(`{"target":"U1"}`)
	impersonateRequest := httptest.NewRequest(http.MethodPost, "/v1/admin/impersonate", bytes.NewReader(impersonateBody))
	impersonateRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, impersonateRequest, cfg.Auth.SourceSigningSecret, impersonateBody)
	impersonateResponse := httptest.NewRecorder()
	h.serveOwned(impersonateResponse, impersonateRequest)
	if impersonateResponse.Code != http.StatusOK || !bytes.Contains(impersonateResponse.Body.Bytes(), []byte(`"target":"U1"`)) || !bytes.Contains(impersonateResponse.Body.Bytes(), []byte(`"displayName":"Alice"`)) {
		t.Fatalf("impersonate=%d %s", impersonateResponse.Code, impersonateResponse.Body.String())
	}
	stopImpersonateBody := []byte(`{"target":"U1"}`)
	stopImpersonateRequest := httptest.NewRequest(http.MethodPost, "/v1/admin/impersonate/stop", bytes.NewReader(stopImpersonateBody))
	stopImpersonateRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, stopImpersonateRequest, cfg.Auth.SourceSigningSecret, stopImpersonateBody)
	stopImpersonateResponse := httptest.NewRecorder()
	h.serveOwned(stopImpersonateResponse, stopImpersonateRequest)
	if stopImpersonateResponse.Code != http.StatusOK || !bytes.Contains(stopImpersonateResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("stop impersonate=%d %s", stopImpersonateResponse.Code, stopImpersonateResponse.Body.String())
	}
	cronSessionsRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/sessions?scope=project:p1&origin=cron", nil)
	cronSessionsRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, cronSessionsRequest, cfg.Auth.SourceSigningSecret, nil)
	cronSessionsResponse := httptest.NewRecorder()
	h.serveOwned(cronSessionsResponse, cronSessionsRequest)
	if cronSessionsResponse.Code != http.StatusOK || !bytes.Contains(cronSessionsResponse.Body.Bytes(), []byte(`"cronId":"cron-2"`)) || !bytes.Contains(cronSessionsResponse.Body.Bytes(), []byte(`"total":1`)) {
		t.Fatalf("cron sessions=%d %s", cronSessionsResponse.Code, cronSessionsResponse.Body.String())
	}
	metricsReadRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/metrics?scope=project:p1", nil)
	metricsReadRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, metricsReadRequest, cfg.Auth.SourceSigningSecret, nil)
	metricsReadResponse := httptest.NewRecorder()
	h.serveOwned(metricsReadResponse, metricsReadRequest)
	if metricsReadResponse.Code != http.StatusOK || !bytes.Contains(metricsReadResponse.Body.Bytes(), []byte(`"turnLatency":{"count":1,"p50":10`)) || !bytes.Contains(metricsReadResponse.Body.Bytes(), []byte(`"phase":"deliver"`)) || !bytes.Contains(metricsReadResponse.Body.Bytes(), []byte(`"missTurns":1`)) || !bytes.Contains(metricsReadResponse.Body.Bytes(), []byte(`"scopeId":"project:p1"`)) {
		t.Fatalf("metrics=%d %s", metricsReadResponse.Code, metricsReadResponse.Body.String())
	}
	retentionRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/retention?scope=org:acme", nil)
	retentionRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, retentionRequest, cfg.Auth.SourceSigningSecret, nil)
	retentionResponse := httptest.NewRecorder()
	h.serveOwned(retentionResponse, retentionRequest)
	if retentionResponse.Code != http.StatusOK || !bytes.Contains(retentionResponse.Body.Bytes(), []byte(`"dau":1`)) || !bytes.Contains(retentionResponse.Body.Bytes(), []byte(`"users":1`)) {
		t.Fatalf("retention=%d %s", retentionResponse.Code, retentionResponse.Body.String())
	}
	auditRequest := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?scope=org:acme", nil)
	auditRequest.Header.Set("x-admin-actor", "admin@acme")
	signSourceRequest(t, auditRequest, cfg.Auth.SourceSigningSecret, nil)
	auditResponse := httptest.NewRecorder()
	h.serveOwned(auditResponse, auditRequest)
	if auditResponse.Code != http.StatusOK || !bytes.Contains(auditResponse.Body.Bytes(), []byte(`"action":"audit.read"`)) {
		t.Fatalf("audit=%d %s", auditResponse.Code, auditResponse.Body.String())
	}
	deploymentNameBody := []byte(`{"name":"incident-dashboard-renamed"}`)
	deploymentNameRequest := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-1/name", bytes.NewReader(deploymentNameBody))
	signSourceRequest(t, deploymentNameRequest, cfg.Auth.SourceSigningSecret, deploymentNameBody)
	deploymentNameResponse := httptest.NewRecorder()
	h.serveOwned(deploymentNameResponse, deploymentNameRequest)
	if deploymentNameResponse.Code != http.StatusOK || !bytes.Contains(deploymentNameResponse.Body.Bytes(), []byte(`"name":"incident-dashboard-renamed"`)) {
		t.Fatalf("deployment name=%d %s", deploymentNameResponse.Code, deploymentNameResponse.Body.String())
	}
	displayNameBody := []byte(`{"displayName":"Incident Dashboard"}`)
	displayNameRequest := httptest.NewRequest(http.MethodPost, "/v1/deployments/deployment-1/display-name", bytes.NewReader(displayNameBody))
	signSourceRequest(t, displayNameRequest, cfg.Auth.SourceSigningSecret, displayNameBody)
	displayNameResponse := httptest.NewRecorder()
	h.serveOwned(displayNameResponse, displayNameRequest)
	if displayNameResponse.Code != http.StatusOK || !bytes.Contains(displayNameResponse.Body.Bytes(), []byte(`"displayName":"Incident Dashboard"`)) {
		t.Fatalf("deployment display name=%d %s", displayNameResponse.Code, displayNameResponse.Body.String())
	}
	var deploymentMapVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='deployments'").Scan(&deploymentMapVersion); err != nil || deploymentMapVersion < 2 {
		t.Fatalf("deployment durable-map version=%d err=%v", deploymentMapVersion, err)
	}
	contextResultBody := []byte(`{"messages":[{"text":"context loaded"}],"hasMore":true,"nextBefore":"1.0","file":{"blobId":"blob-1","name":"notes.txt","sizeBytes":12,"mimetype":"text/plain"}}`)
	contextResultRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-context/context-1/result", bytes.NewReader(contextResultBody))
	signSourceRequest(t, contextResultRequest, cfg.Auth.SourceSigningSecret, contextResultBody)
	contextResultResponse := httptest.NewRecorder()
	h.serveOwned(contextResultResponse, contextResultRequest)
	if contextResultResponse.Code != http.StatusOK || !bytes.Contains(contextResultResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("surface context result=%d %s", contextResultResponse.Code, contextResultResponse.Body.String())
	}
	var contextRaw []byte
	if err := pg.Pool.QueryRow(ctx, "SELECT json FROM context_requests WHERE id='context-1'").Scan(&contextRaw); err != nil {
		t.Fatal(err)
	}
	var storedContext struct {
		Status string `json:"status"`
		Result struct {
			File struct {
				BlobID string `json:"blobId"`
			} `json:"file"`
		} `json:"result"`
	}
	if err := json.Unmarshal(contextRaw, &storedContext); err != nil || storedContext.Status != "done" || storedContext.Result.File.BlobID != "blob-1" {
		t.Fatalf("surface context stored=%s parsed=%#v err=%v", contextRaw, storedContext, err)
	}
	missingContextRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-context/missing/result", bytes.NewReader(contextResultBody))
	signSourceRequest(t, missingContextRequest, cfg.Auth.SourceSigningSecret, contextResultBody)
	missingContextResponse := httptest.NewRecorder()
	h.serveOwned(missingContextResponse, missingContextRequest)
	if missingContextResponse.Code != http.StatusNotFound {
		t.Fatalf("missing surface context=%d %s", missingContextResponse.Code, missingContextResponse.Body.String())
	}
	nullContextBody := []byte(`{"error":null,"messages":null,"hasMore":null,"nextBefore":null,"note":null}`)
	nullContextRequest := httptest.NewRequest(http.MethodPost, "/v1/surface-context/context-2/result", bytes.NewReader(nullContextBody))
	signSourceRequest(t, nullContextRequest, cfg.Auth.SourceSigningSecret, nullContextBody)
	nullContextResponse := httptest.NewRecorder()
	h.serveOwned(nullContextResponse, nullContextRequest)
	if nullContextResponse.Code != http.StatusOK {
		t.Fatalf("null surface context=%d %s", nullContextResponse.Code, nullContextResponse.Body.String())
	}
	if err := pg.Pool.QueryRow(ctx, "SELECT json FROM context_requests WHERE id='context-2'").Scan(&contextRaw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contextRaw, &storedContext); err != nil || storedContext.Status != "done" {
		t.Fatalf("null surface context stored=%s parsed=%#v err=%v", contextRaw, storedContext, err)
	}
}

func testCapabilityToken(t *testing.T, secret, actorID, scopeID string) string {
	t.Helper()
	return testCapabilityTokenWithClaims(t, secret, auth.Claims{ActorID: actorID, ScopeID: scopeID, Audience: "control-plane", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
}

func testCapabilityTokenWithClaims(t *testing.T, secret string, claims auth.Claims) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func testConnectorCredentialID(principalID, host, accountType string) string {
	slot := "oauth:"
	if accountType != "" && accountType != "default" {
		slot += accountType
	}
	digest := sha256.Sum256([]byte(principalID + "\x00" + strings.ToLower(host) + "\x00" + slot))
	return hex.EncodeToString(digest[:])[:16]
}

func testPortalIdentityToken(t *testing.T, secret, principalID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload, err := json.Marshal(map[string]any{"p": principalID, "exp": time.Now().Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signSourceRequest(t *testing.T, request *http.Request, secret string, body []byte) {
	t.Helper()
	timestamp := time.Now().Unix()
	canonical := request.Method + "\n" + request.URL.RequestURI() + "\n" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + fmt.Sprint(timestamp) + ":" + canonical))
	request.Header.Set("x-timestamp", fmt.Sprint(timestamp))
	request.Header.Set("x-signature", "v0="+fmt.Sprintf("%x", mac.Sum(nil)))
}

func signBlobSourceRequest(t *testing.T, request *http.Request, secret string) {
	t.Helper()
	timestamp := time.Now().Unix()
	tail := ""
	if request.Method == http.MethodPost {
		tail = request.Header.Get("x-content-sha256")
	}
	canonical := request.Method + "\n" + request.URL.RequestURI() + "\n" + tail
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + fmt.Sprint(timestamp) + ":" + canonical))
	request.Header.Set("x-timestamp", fmt.Sprint(timestamp))
	request.Header.Set("x-signature", "v0="+fmt.Sprintf("%x", mac.Sum(nil)))
}
