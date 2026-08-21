package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	knowlegapostgres "github.com/loon-hejw/knowlega/internal/agent/knowlega/postgres"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestQMProjectFileDrivesKnowledgeLifecycle(t *testing.T) {
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE project_file_memberships,file_artifacts,knowledge_scopes,projects,durable_map_versions,deactivated_principals,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS knowledge_core"); err != nil {
		t.Fatal(err)
	}
	knowledgeDB, err := sql.Open("pgx", projectFileKnowledgeDSN(databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = knowledgeDB.Close() })
	if err := knowledgeDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	knowledgeStore := knowlegapostgres.NewStore(knowledgeDB)
	if err := knowledgeStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "project-files-source-secret"
	cfg.Auth.CapabilitySecret = "project-files-capability-secret"
	cfg.QM.FileStore.Mode = "local"
	cfg.QM.FileStore.LocalDir = filepath.Join(t.TempDir(), "files")
	cfg.QM.FileStore.TransferLocalDir = filepath.Join(t.TempDir(), "transfer")
	if err := os.MkdirAll(cfg.QM.FileStore.TransferLocalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	directory := data.NewDirectoryRepository(pg, cfg.QM.OrgID)
	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}}
	if err := directory.Sync(ctx, data.DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	projectRepo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	project, err := projectRepo.Create(ctx, "alice", "Knowledge Project")
	if err != nil {
		t.Fatal(err)
	}
	secondProject, err := projectRepo.Create(ctx, "alice", "Second Knowledge Project")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := knowlega.New(knowlega.AgentOptions{
		RootDir: t.TempDir(), Compiler: compiler.MockProvider{}, WikiStore: knowledgeStore, SearchStore: knowledgeStore, GraphStore: knowledgeStore,
		ProjectIDFor: func(ref knowlega.ScopeRef) string {
			return "qm-project-file-it-" + strings.TrimPrefix(ref.ExternalScopeID, "group:web-project-")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = knowledgeDB.ExecContext(context.Background(), "DELETE FROM projects WHERE id IN ($1,$2)", "qm-project-file-it-"+project.ID, "qm-project-file-it-"+secondProject.ID)
	})
	projectFiles := data.NewProjectFileMembershipRepository(pg)
	knowledgeScopes := data.NewKnowledgeScopeRepository(pg)
	h := &HTTPServer{
		config:          cfg,
		projects:        biz.NewProjectUsecase(projectRepo),
		projectRepo:     projectRepo,
		knowledgeAgent:  agent,
		directory:       directory,
		acl:             data.NewACLRepository(pg),
		files:           data.NewFileArtifactRepository(pg),
		projectFiles:    projectFiles,
		knowledgeScopes: knowledgeScopes,
		sessions:        data.NewSessionRepository(pg),
		crons:           data.NewCronRepository(pg),
		deployments:     data.NewDeploymentRepository(pg),
		skills:          data.NewSkillRepository(pg),
		audit:           data.NewAuditor(pg),
		auth:            auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Minute, DB: pg.Pool},
		logger:          slog.Default(),
	}

	content := []byte("# Release Plan\n\nDeploy after review.\n")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	blobID, _, err := data.PutLocalTransferBlob(cfg.QM.FileStore.TransferLocalDir, bytes.NewReader(content), sha, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	scopeID := biz.ProjectScopeID(project.ID)
	uploadBody := []byte(`{"principalId":"alice","blobId":"` + blobID + `","name":"release.md","mimetype":"text/markdown","scopeId":"` + scopeID + `"}`)
	uploadRequest := httptest.NewRequest(http.MethodPost, "/v1/files/upload", bytes.NewReader(uploadBody))
	signSourceRequest(t, uploadRequest, cfg.Auth.SourceSigningSecret, uploadBody)
	uploadResponse := httptest.NewRecorder()
	h.serveOwned(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var uploaded struct {
		File struct {
			ID string `json:"id"`
		} `json:"file"`
		ProjectFile data.ProjectFileMembership `json:"projectFile"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.File.ID == "" || uploaded.ProjectFile.Status != "queued" || uploaded.ProjectFile.QueueTaskID == nil || uploaded.ProjectFile.RawPath == nil {
		t.Fatalf("upload response=%s", uploadResponse.Body.String())
	}
	queuedScope, err := knowledgeScopes.Get(ctx, cfg.QM.OrgID, scopeID, "project")
	if err != nil || queuedScope.Status != agentservice.KnowledgeWorkspaceQueued {
		t.Fatalf("queued scope=%+v err=%v", queuedScope, err)
	}
	queuedBinding, err := knowledgeStore.GetScopeBinding(ctx, "qm", scopeID)
	if err != nil || queuedBinding.Status != agentservice.KnowledgeWorkspaceQueued {
		t.Fatalf("queued binding=%+v err=%v", queuedBinding, err)
	}
	rawMirror := filepath.Join(agentRootForScope(t, agent, cfg.QM.OrgID, scopeID), filepath.FromSlash(*uploaded.ProjectFile.RawPath))
	info, err := os.Stat(rawMirror)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("raw mirror info=%v err=%v", info, err)
	}
	queuedRecall, err := agent.Search(ctx, knowlega.ScopeRef{OrgID: cfg.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: project.Name}, "Deploy after review", 20)
	if err != nil || !projectFileSearchHasPrefix(queuedRecall, "raw/") {
		t.Fatalf("queued recall=%+v err=%v", queuedRecall, err)
	}
	queuedResourcesRequest := httptest.NewRequest(http.MethodGet, "/v1/scope-resources?principalId=alice&scope="+scopeID, nil)
	signSourceRequest(t, queuedResourcesRequest, cfg.Auth.SourceSigningSecret, nil)
	queuedResourcesResponse := httptest.NewRecorder()
	h.serveOwned(queuedResourcesResponse, queuedResourcesRequest)
	if queuedResourcesResponse.Code != http.StatusOK || !strings.Contains(queuedResourcesResponse.Body.String(), `"knowledge"`) || !strings.Contains(queuedResourcesResponse.Body.String(), `"sourceCount":1`) || !strings.Contains(queuedResourcesResponse.Body.String(), `"status":"queued"`) {
		t.Fatalf("queued resources status=%d body=%s", queuedResourcesResponse.Code, queuedResourcesResponse.Body.String())
	}

	ref := knowlega.ScopeRef{OrgID: cfg.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: project.Name}
	maintained, err := ProcessProjectFileScope(ctx, project.ID, ref, projectFiles, knowledgeScopes, agent, false, false)
	if err != nil || maintained.Status != "ok" {
		t.Fatalf("maintain status=%s err=%v steps=%+v", maintained.Status, err, maintained.Steps)
	}
	membership, err := projectFiles.Get(ctx, project.ID, uploaded.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Status != "ready" || membership.GeneratedPageCount == 0 {
		t.Fatalf("membership=%+v", membership)
	}
	readyStatus, err := agent.Status(ref)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := agentservice.LoadIngestQueue(readyStatus.ProjectPath)
	if err != nil || len(queue.Tasks) != 1 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
	readyScope, err := knowledgeScopes.Get(ctx, cfg.QM.OrgID, scopeID, "project")
	if err != nil || readyScope.Status != agentservice.KnowledgeWorkspaceReady {
		t.Fatalf("ready scope=%+v err=%v", readyScope, err)
	}
	readyBinding, err := knowledgeStore.GetScopeBinding(ctx, "qm", scopeID)
	if err != nil || readyBinding.Status != agentservice.KnowledgeWorkspaceReady {
		t.Fatalf("ready binding=%+v err=%v", readyBinding, err)
	}
	readyRecall, err := agent.Search(ctx, ref, "Deploy after review", 20)
	if err != nil || !projectFileSearchHasPrefix(readyRecall, "raw/") || !projectFileSearchHasPrefix(readyRecall, "wiki/sources/") || !projectFileSearchHasPrefix(readyRecall, "wiki/concepts/") || !projectFileSearchHasPrefix(readyRecall, "wiki/entities/") {
		t.Fatalf("ready recall=%+v err=%v", readyRecall, err)
	}
	for _, prefix := range []string{"raw/", "wiki/sources/", "wiki/concepts/", "wiki/entities/"} {
		path := projectFileSearchPath(readyRecall, prefix)
		if _, err := agent.Read(ref, path); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}
	if len(queue.Tasks[0].Files) < 3 {
		t.Fatalf("generated task files=%+v", queue.Tasks[0].Files)
	}
	for _, prefix := range []string{"wiki/sources/", "wiki/concepts/", "wiki/entities/"} {
		if !slices.ContainsFunc(queue.Tasks[0].Files, func(path string) bool { return strings.HasPrefix(path, prefix) }) {
			t.Fatalf("generated task files missing %s: %+v", prefix, queue.Tasks[0].Files)
		}
	}
	generatedArtifacts := append([]string(nil), queue.Tasks[0].Files...)
	generatedArtifacts = append(generatedArtifacts, "wiki/index.md", "wiki/overview.md", "wiki/log.md", "wiki/reviews.md")
	for _, rel := range generatedArtifacts {
		if info, err := os.Stat(filepath.Join(readyStatus.ProjectPath, filepath.FromSlash(rel))); err != nil || info.Size() == 0 {
			t.Fatalf("generated artifact %s info=%v err=%v", rel, info, err)
		}
	}
	projectFileAssertKnowledgeRows(t, knowledgeDB, readyStatus.ProjectID, map[string]int{"source_manifest": 1, "sources": 1, "review_items": 1})
	initialWikiRows := projectFileKnowledgeCount(t, knowledgeDB, "wiki_pages", readyStatus.ProjectID)
	if initialWikiRows != int(readyStatus.WikiPageCount) {
		pages := initialWikiRows
		t.Fatalf("PostgreSQL wiki pages=%d filesystem=%d", pages, readyStatus.WikiPageCount)
	}
	initialTaskID := queue.Tasks[0].ID
	duplicateAttachBody := []byte(`{"principalId":"alice","fileId":"` + uploaded.File.ID + `"}`)
	duplicateAttachRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/"+project.ID+"/files", bytes.NewReader(duplicateAttachBody))
	signSourceRequest(t, duplicateAttachRequest, cfg.Auth.SourceSigningSecret, duplicateAttachBody)
	duplicateAttachResponse := httptest.NewRecorder()
	h.serveOwned(duplicateAttachResponse, duplicateAttachRequest)
	if duplicateAttachResponse.Code != http.StatusAccepted || !strings.Contains(duplicateAttachResponse.Body.String(), `"status":"queued"`) {
		t.Fatalf("duplicate attach status=%d body=%s", duplicateAttachResponse.Code, duplicateAttachResponse.Body.String())
	}
	duplicateMaintained, err := ProcessProjectFileScope(ctx, project.ID, ref, projectFiles, knowledgeScopes, agent, false, false)
	if err != nil || duplicateMaintained.Status != "ok" {
		t.Fatalf("duplicate maintenance=%+v err=%v", duplicateMaintained, err)
	}
	refreshFound := false
	for _, step := range duplicateMaintained.Steps {
		if step.Name != "refresh_navigation" {
			continue
		}
		refreshFound = true
		if step.Summary["changed_files"] != 0 || step.Summary["overview_refreshed"] != false {
			t.Fatalf("duplicate attachment triggered synthesis: %+v", step)
		}
	}
	if !refreshFound {
		t.Fatalf("duplicate maintenance missing navigation step: %+v", duplicateMaintained.Steps)
	}
	membership, err = projectFiles.Get(ctx, project.ID, uploaded.File.ID)
	if err != nil || membership.Status != "ready" || membership.GeneratedPageCount < 3 || membership.QueueTaskID == nil || *membership.QueueTaskID != initialTaskID {
		t.Fatalf("duplicate membership=%+v err=%v", membership, err)
	}
	queue, err = agentservice.LoadIngestQueue(readyStatus.ProjectPath)
	if err != nil || len(queue.Tasks) != 1 || queue.Tasks[0].ID != initialTaskID || queue.Tasks[0].Status != agentservice.IngestTaskDone {
		t.Fatalf("duplicate queue=%+v err=%v", queue, err)
	}
	projectFileAssertKnowledgeRows(t, knowledgeDB, readyStatus.ProjectID, map[string]int{"source_manifest": 1, "sources": 1, "review_items": 1})
	if pages := projectFileKnowledgeCount(t, knowledgeDB, "wiki_pages", readyStatus.ProjectID); pages != initialWikiRows {
		t.Fatalf("duplicate attachment changed wiki page count: before=%d after=%d", initialWikiRows, pages)
	}
	queue.Tasks[0].Status = agentservice.IngestTaskProcessing
	if err := agentservice.SaveIngestQueue(readyStatus.ProjectPath, queue); err != nil {
		t.Fatal(err)
	}
	if err := projectFiles.SetState(ctx, project.ID, uploaded.File.ID, "processing", membership.RawPath, membership.QueueTaskID, membership.GeneratedPageCount, nil); err != nil {
		t.Fatal(err)
	}
	restarted, err := ProcessProjectFileScope(ctx, project.ID, ref, projectFiles, knowledgeScopes, agent, false, true)
	if err != nil || restarted.Status != "ok" {
		t.Fatalf("restart maintain status=%s err=%v steps=%+v", restarted.Status, err, restarted.Steps)
	}
	membership, err = projectFiles.Get(ctx, project.ID, uploaded.File.ID)
	if err != nil || membership.Status != "ready" {
		t.Fatalf("membership after restart=%+v err=%v", membership, err)
	}
	queue, err = agentservice.LoadIngestQueue(readyStatus.ProjectPath)
	if err != nil || queue.Tasks[0].Status != agentservice.IngestTaskDone || queue.Tasks[0].RetryCount == 0 {
		t.Fatalf("queue after restart=%+v err=%v", queue, err)
	}
	resourcesRequest := httptest.NewRequest(http.MethodGet, "/v1/scope-resources?principalId=alice&scope="+scopeID, nil)
	signSourceRequest(t, resourcesRequest, cfg.Auth.SourceSigningSecret, nil)
	resourcesResponse := httptest.NewRecorder()
	h.serveOwned(resourcesResponse, resourcesRequest)
	if resourcesResponse.Code != http.StatusOK || !strings.Contains(resourcesResponse.Body.String(), `"projectFile"`) || !strings.Contains(resourcesResponse.Body.String(), `"knowledge"`) || !strings.Contains(resourcesResponse.Body.String(), `"status":"ready"`) {
		t.Fatalf("project resources status=%d body=%s", resourcesResponse.Code, resourcesResponse.Body.String())
	}
	status, err := agent.Status(ref)
	if err != nil {
		t.Fatal(err)
	}
	manifestValue, err := manifest.Load(status.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifestValue.Sources) != 1 {
		t.Fatalf("manifest=%+v", manifestValue)
	}
	for _, entry := range manifestValue.Sources {
		if entry.QMFileID != uploaded.File.ID || entry.QMProjectID != project.ID || entry.QMScopeID != scopeID || entry.QMSourceSHA256 != sha {
			t.Fatalf("manifest entry=%+v", entry)
		}
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/files?viewer=alice&scope="+scopeID, nil)
	signSourceRequest(t, listRequest, cfg.Auth.SourceSigningSecret, nil)
	listResponse := httptest.NewRecorder()
	h.serveOwned(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"status":"ready"`) {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	attachBody := []byte(`{"principalId":"alice","fileId":"` + uploaded.File.ID + `"}`)
	attachRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/"+secondProject.ID+"/files", bytes.NewReader(attachBody))
	signSourceRequest(t, attachRequest, cfg.Auth.SourceSigningSecret, attachBody)
	attachResponse := httptest.NewRecorder()
	h.serveOwned(attachResponse, attachRequest)
	if attachResponse.Code != http.StatusAccepted {
		t.Fatalf("attach status=%d body=%s", attachResponse.Code, attachResponse.Body.String())
	}
	secondScopeID := biz.ProjectScopeID(secondProject.ID)
	secondRef := knowlega.ScopeRef{OrgID: cfg.QM.OrgID, ExternalScopeID: secondScopeID, Kind: "project", Name: secondProject.Name}
	maintained, err = ProcessProjectFileScope(ctx, secondProject.ID, secondRef, projectFiles, knowledgeScopes, agent, false, false)
	if err != nil || maintained.Status != "ok" {
		t.Fatalf("second maintain status=%s err=%v steps=%+v", maintained.Status, err, maintained.Steps)
	}
	secondMembership, err := projectFiles.Get(ctx, secondProject.ID, uploaded.File.ID)
	if err != nil || secondMembership.Status != "ready" || secondMembership.RawPath == nil {
		t.Fatalf("second membership=%+v err=%v", secondMembership, err)
	}
	secondRawMirror := filepath.Join(agentRootForScope(t, agent, cfg.QM.OrgID, secondScopeID), filepath.FromSlash(*secondMembership.RawPath))

	deleteBody := []byte(`{"principalId":"alice"}`)
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/projects/"+project.ID+"/files/"+uploaded.File.ID, bytes.NewReader(deleteBody))
	signSourceRequest(t, deleteRequest, cfg.Auth.SourceSigningSecret, deleteBody)
	deleteResponse := httptest.NewRecorder()
	h.serveOwned(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK || !strings.Contains(deleteResponse.Body.String(), `"fileDeleted":false`) {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	persistedFile, err := h.files.Get(ctx, uploaded.File.ID)
	if err != nil || persistedFile == nil {
		t.Fatalf("file=%+v err=%v", persistedFile, err)
	}
	if membership, err := projectFiles.Find(ctx, project.ID, uploaded.File.ID); err != nil || membership != nil {
		t.Fatalf("membership=%+v err=%v", membership, err)
	}
	grants, err := h.acl.List(ctx, persistedFile.OwnerScopeID, persistedFile.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		if grant.GranteeScopeID == scopeID {
			t.Fatalf("removed project retained file grant: %+v", grants)
		}
	}
	if !slices.ContainsFunc(grants, func(grant data.Grant) bool { return grant.GranteeScopeID == secondScopeID }) {
		t.Fatalf("second project file grant missing: %+v", grants)
	}
	if _, err := os.Stat(rawMirror); !os.IsNotExist(err) {
		t.Fatalf("raw mirror still exists: %v", err)
	}
	manifestValue, err = manifest.Load(status.ProjectPath)
	if err != nil || len(manifestValue.Sources) != 0 {
		t.Fatalf("manifest after delete=%+v err=%v", manifestValue, err)
	}
	if _, err := os.Stat(secondRawMirror); err != nil {
		t.Fatalf("second raw mirror missing after first delete: %v", err)
	}
	emptyScope, err := knowledgeScopes.Get(ctx, cfg.QM.OrgID, scopeID, "project")
	if err != nil || emptyScope.Status != agentservice.KnowledgeWorkspaceEmpty {
		t.Fatalf("empty scope=%+v err=%v", emptyScope, err)
	}
	emptyBinding, err := knowledgeStore.GetScopeBinding(ctx, "qm", scopeID)
	if err != nil || emptyBinding.Status != agentservice.KnowledgeWorkspaceEmpty {
		t.Fatalf("empty binding=%+v err=%v", emptyBinding, err)
	}
	projectFileAssertKnowledgeRows(t, knowledgeDB, status.ProjectID, map[string]int{"source_manifest": 0, "sources": 0})
	if versions := projectFileKnowledgeCount(t, knowledgeDB, "wiki_page_versions", status.ProjectID); versions == 0 {
		t.Fatal("deleted generated pages were not preserved in PostgreSQL page versions")
	}
	secondStatus, err := agent.Status(secondRef)
	if err != nil {
		t.Fatal(err)
	}
	projectFileAssertKnowledgeRows(t, knowledgeDB, secondStatus.ProjectID, map[string]int{"source_manifest": 1, "sources": 1})

	deleteSecondRequest := httptest.NewRequest(http.MethodDelete, "/v1/projects/"+secondProject.ID+"/files/"+uploaded.File.ID, bytes.NewReader(deleteBody))
	signSourceRequest(t, deleteSecondRequest, cfg.Auth.SourceSigningSecret, deleteBody)
	deleteSecondResponse := httptest.NewRecorder()
	h.serveOwned(deleteSecondResponse, deleteSecondRequest)
	if deleteSecondResponse.Code != http.StatusOK || !strings.Contains(deleteSecondResponse.Body.String(), `"fileDeleted":true`) {
		t.Fatalf("second delete status=%d body=%s", deleteSecondResponse.Code, deleteSecondResponse.Body.String())
	}
	if file, err := h.files.Get(ctx, uploaded.File.ID); err != nil || file != nil {
		t.Fatalf("file after final delete=%+v err=%v", file, err)
	}
	if _, err := os.Stat(secondRawMirror); !os.IsNotExist(err) {
		t.Fatalf("second raw mirror still exists: %v", err)
	}

	unsupported := []byte("image bytes")
	unsupportedSum := sha256.Sum256(unsupported)
	unsupportedBlobID, _, err := data.PutLocalTransferBlob(cfg.QM.FileStore.TransferLocalDir, bytes.NewReader(unsupported), hex.EncodeToString(unsupportedSum[:]), int64(len(unsupported)))
	if err != nil {
		t.Fatal(err)
	}
	unsupportedBody := []byte(`{"principalId":"alice","blobId":"` + unsupportedBlobID + `","name":"image.png","mimetype":"image/png","scopeId":"` + scopeID + `"}`)
	unsupportedRequest := httptest.NewRequest(http.MethodPost, "/v1/files/upload", bytes.NewReader(unsupportedBody))
	signSourceRequest(t, unsupportedRequest, cfg.Auth.SourceSigningSecret, unsupportedBody)
	unsupportedResponse := httptest.NewRecorder()
	h.serveOwned(unsupportedResponse, unsupportedRequest)
	var unsupportedUpload struct {
		File struct {
			ID string `json:"id"`
		} `json:"file"`
		ProjectFile data.ProjectFileMembership `json:"projectFile"`
	}
	if err := json.Unmarshal(unsupportedResponse.Body.Bytes(), &unsupportedUpload); err != nil || unsupportedResponse.Code != http.StatusOK || unsupportedUpload.ProjectFile.Status != "unsupported" || unsupportedUpload.ProjectFile.RawPath != nil {
		t.Fatalf("unsupported upload status=%d body=%s parsed=%+v err=%v", unsupportedResponse.Code, unsupportedResponse.Body.String(), unsupportedUpload, err)
	}
	deleteUnsupported := httptest.NewRequest(http.MethodDelete, "/v1/projects/"+project.ID+"/files/"+unsupportedUpload.File.ID, bytes.NewReader(deleteBody))
	signSourceRequest(t, deleteUnsupported, cfg.Auth.SourceSigningSecret, deleteBody)
	deleteUnsupportedResponse := httptest.NewRecorder()
	h.serveOwned(deleteUnsupportedResponse, deleteUnsupported)
	if deleteUnsupportedResponse.Code != http.StatusOK || !strings.Contains(deleteUnsupportedResponse.Body.String(), `"fileDeleted":true`) {
		t.Fatalf("unsupported delete status=%d body=%s", deleteUnsupportedResponse.Code, deleteUnsupportedResponse.Body.String())
	}
}

func projectFileKnowledgeDSN(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		query := parsed.Query()
		query.Set("search_path", "knowledge_core,public")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	separator := "?"
	if strings.Contains(raw, "?") {
		separator = "&"
	}
	return raw + separator + "search_path=knowledge_core,public"
}

func projectFileKnowledgeCount(t *testing.T, db *sql.DB, table, projectID string) int {
	t.Helper()
	allowed := map[string]bool{"wiki_pages": true, "wiki_page_versions": true, "source_manifest": true, "sources": true, "review_items": true}
	if !allowed[table] {
		t.Fatalf("unsupported knowledge table %q", table)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id=$1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func projectFileAssertKnowledgeRows(t *testing.T, db *sql.DB, projectID string, expected map[string]int) {
	t.Helper()
	for table, want := range expected {
		if got := projectFileKnowledgeCount(t, db, table, projectID); got != want {
			t.Fatalf("%s rows=%d want=%d project=%s", table, got, want, projectID)
		}
	}
}

func projectFileSearchHasPrefix(results []core.KnowledgeSearchResult, prefix string) bool {
	return projectFileSearchPath(results, prefix) != ""
}

func projectFileSearchPath(results []core.KnowledgeSearchResult, prefix string) string {
	for _, result := range results {
		if strings.HasPrefix(result.Path, prefix) {
			return result.Path
		}
	}
	return ""
}

func agentRootForScope(t *testing.T, agent *knowlega.Agent, orgID, scopeID string) string {
	t.Helper()
	status, err := agent.Status(knowlega.ScopeRef{OrgID: orgID, ExternalScopeID: scopeID, Kind: "project"})
	if err != nil {
		t.Fatal(err)
	}
	return status.ProjectPath
}
