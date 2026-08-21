package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestProjectFileMaintenanceFailureOnlyFailsClaimedMembershipsAndRecovers(t *testing.T) {
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

	projectID := "worker-failure-" + strings.ToLower(dataWorkerTestID(t))
	scopeID := "group:web-project-" + projectID
	ref := knowlega.ScopeRef{OrgID: "acme", ExternalScopeID: scopeID, Kind: "project", Name: "Worker failure test"}
	provider := &projectFileToggleOverviewProvider{}
	agent, err := knowlega.New(knowlega.AgentOptions{
		RootDir:  t.TempDir(),
		Compiler: provider,
		ProjectIDFor: func(knowlega.ScopeRef) string {
			return "knowledge-" + projectID
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	memberships := data.NewProjectFileMembershipRepository(pg)
	scopes := data.NewKnowledgeScopeRepository(pg)
	files := data.NewFileArtifactRepository(pg)
	var fileIDs []string
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM project_file_memberships WHERE project_id=$1", projectID)
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM knowledge_scopes WHERE org_id=$1 AND external_scope_id=$2 AND scope_kind='project'", ref.OrgID, scopeID)
		if len(fileIDs) > 0 {
			_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM file_artifacts WHERE id=ANY($1::text[])", fileIDs)
		}
	})

	first := enqueueProjectFileWorkerTestMembership(t, ctx, files, memberships, agent, projectID, ref, "first.md", []byte("# First\n\nHistorical ready evidence.\n"))
	result, err := ProcessProjectFileScope(ctx, projectID, ref, memberships, scopes, agent, false, false)
	if err != nil || result.Status != "ok" {
		t.Fatalf("first maintenance=%+v err=%v", result, err)
	}
	first, err = memberships.Get(ctx, projectID, first.FileID)
	if err != nil || first.Status != "ready" {
		t.Fatalf("first membership=%+v err=%v", first, err)
	}
	fileIDs = append(fileIDs, first.FileID)
	readyStatus, err := agent.Status(ref)
	if err != nil {
		t.Fatal(err)
	}
	maintenancePath := filepath.Join(readyStatus.ProjectPath, ".kbcore", "maintenance-status.json")
	maintenanceBefore, err := os.ReadFile(maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := ProcessProjectFileScope(ctx, projectID, ref, memberships, scopes, agent, false, false)
	if err != nil || idle.Status != "ok" || len(idle.Steps) != 0 {
		t.Fatalf("idle maintenance=%+v err=%v", idle, err)
	}
	maintenanceAfter, err := os.ReadFile(maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(maintenanceAfter) != string(maintenanceBefore) {
		t.Fatalf("idle worker changed maintenance state:\nbefore=%s\nafter=%s", maintenanceBefore, maintenanceAfter)
	}
	idleScope, err := scopes.Get(ctx, ref.OrgID, scopeID, ref.Kind)
	if err != nil || idleScope.Status != agentservice.KnowledgeWorkspaceReady {
		t.Fatalf("idle scope=%+v err=%v", idleScope, err)
	}
	interruptedState := strings.Replace(string(maintenanceBefore), `"status": "ready"`, `"status": "processing"`, 1)
	if interruptedState == string(maintenanceBefore) {
		t.Fatalf("ready maintenance state missing: %s", maintenanceBefore)
	}
	if err := os.WriteFile(maintenancePath, []byte(interruptedState), 0o644); err != nil {
		t.Fatal(err)
	}
	recoveredIdle, err := ProcessProjectFileScope(ctx, projectID, ref, memberships, scopes, agent, false, true)
	if err != nil || recoveredIdle.Status != "ok" || len(recoveredIdle.Steps) != 0 {
		t.Fatalf("recovered idle maintenance=%+v err=%v", recoveredIdle, err)
	}
	recoveredStatus, err := agent.Status(ref)
	if err != nil || recoveredStatus.State != agentservice.KnowledgeWorkspaceReady {
		t.Fatalf("recovered workspace=%+v err=%v", recoveredStatus, err)
	}

	second := enqueueProjectFileWorkerTestMembership(t, ctx, files, memberships, agent, projectID, ref, "second.md", []byte("# Second\n\nNew evidence requiring navigation refresh.\n"))
	fileIDs = append(fileIDs, second.FileID)
	provider.FailOverview = true
	result, err = ProcessProjectFileScope(ctx, projectID, ref, memberships, scopes, agent, false, false)
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "refresh_navigation") {
		t.Fatalf("failed maintenance=%+v err=%v", result, err)
	}
	firstAfterFailure, firstErr := memberships.Get(ctx, projectID, first.FileID)
	secondAfterFailure, secondErr := memberships.Get(ctx, projectID, second.FileID)
	if firstErr != nil || firstAfterFailure.Status != "ready" {
		t.Fatalf("historical membership changed=%+v err=%v", firstAfterFailure, firstErr)
	}
	if secondErr != nil || secondAfterFailure.Status != "failed" || secondAfterFailure.LastError == nil || !strings.Contains(*secondAfterFailure.LastError, "refresh_navigation") {
		t.Fatalf("claimed membership=%+v err=%v", secondAfterFailure, secondErr)
	}
	failedScope, err := scopes.Get(ctx, ref.OrgID, scopeID, ref.Kind)
	if err != nil || failedScope.Status != agentservice.KnowledgeWorkspaceFailed {
		t.Fatalf("failed scope=%+v err=%v", failedScope, err)
	}

	provider.FailOverview = false
	result, err = ProcessProjectFileScope(ctx, projectID, ref, memberships, scopes, agent, false, false)
	if err != nil || result.Status != "ok" {
		t.Fatalf("recovery maintenance=%+v err=%v", result, err)
	}
	firstRecovered, firstErr := memberships.Get(ctx, projectID, first.FileID)
	secondRecovered, secondErr := memberships.Get(ctx, projectID, second.FileID)
	if firstErr != nil || secondErr != nil || firstRecovered.Status != "ready" || secondRecovered.Status != "ready" || secondRecovered.LastError != nil {
		t.Fatalf("recovered memberships first=%+v second=%+v errors=%v/%v", firstRecovered, secondRecovered, firstErr, secondErr)
	}
	recoveredScope, err := scopes.Get(ctx, ref.OrgID, scopeID, ref.Kind)
	if err != nil || recoveredScope.Status != agentservice.KnowledgeWorkspaceReady {
		t.Fatalf("recovered scope=%+v err=%v", recoveredScope, err)
	}
}

func enqueueProjectFileWorkerTestMembership(t *testing.T, ctx context.Context, files *data.FileArtifactRepository, memberships *data.ProjectFileMembershipRepository, agent *knowlega.Agent, projectID string, ref knowlega.ScopeRef, name string, content []byte) data.ProjectFileMembership {
	t.Helper()
	fileID, err := data.NewFileArtifactID()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	blobKey := "files/" + hash
	now := time.Now().UnixMilli()
	if _, err := files.Create(ctx, data.FileArtifact{
		ID:             fileID,
		OwnerScopeID:   "user:alice",
		CreatedBy:      "alice",
		Name:           name,
		Path:           "uploads/" + fileID + "/" + name,
		Mimetype:       "text/markdown",
		Direction:      "upload",
		SHA256:         hash,
		SizeBytes:      int64(len(content)),
		CreatedAt:      now,
		UpdatedAt:      now,
		BlobKey:        &blobKey,
		CreatedInScope: &ref.ExternalScopeID,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := agent.EnsureScope(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	membership, err := memberships.PutQueued(ctx, data.ProjectFileMembership{
		ProjectID:          projectID,
		ProjectScopeID:     ref.ExternalScopeID,
		FileID:             fileID,
		KnowledgeProjectID: status.ProjectID,
		SourceSHA256:       hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := agent.EnqueueQMFile(ref, fileID, name, hash, content)
	if err != nil {
		t.Fatal(err)
	}
	rawPath, err := filepath.Rel(status.ProjectPath, task.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	rawPath = filepath.ToSlash(rawPath)
	if err := memberships.SetState(ctx, projectID, fileID, "queued", &rawPath, &task.ID, 0, nil); err != nil {
		t.Fatal(err)
	}
	membership.RawPath = &rawPath
	membership.QueueTaskID = &task.ID
	return membership
}

func dataWorkerTestID(t *testing.T) string {
	t.Helper()
	id, err := data.NewFileArtifactID()
	if err != nil {
		t.Fatal(err)
	}
	return id[:12]
}

type projectFileToggleOverviewProvider struct {
	Mock         compiler.MockProvider
	FailOverview bool
}

func (p *projectFileToggleOverviewProvider) Analyze(input compiler.AnalysisInput) (string, error) {
	return p.Mock.Analyze(input)
}

func (p *projectFileToggleOverviewProvider) Generate(analysis string, input compiler.AnalysisInput) (string, error) {
	return p.Mock.Generate(analysis, input)
}

func (p *projectFileToggleOverviewProvider) SynthesizeOverview(input compiler.OverviewInput) (string, error) {
	if p.FailOverview {
		return "", errors.New("temporary overview provider failure")
	}
	return p.Mock.SynthesizeOverview(input)
}
