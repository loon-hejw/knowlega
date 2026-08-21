package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestWorkspaceStatusForProjectSummarizesProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	status, err := WorkspaceStatusForProject(WorkspaceStatusOptions{
		ProjectPath:         root,
		ProjectID:           "project-1",
		Agent:               "llm",
		PGConfigured:        true,
		EmbeddingConfigured: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.OK || status.ProjectPath != root || status.ProjectID != "project-1" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.Status != KnowledgeWorkspaceEmpty || status.SourceCount != 0 {
		t.Fatalf("fresh workspace should be empty: %+v", status)
	}
	if len(status.Files) != 5 {
		t.Fatalf("expected project file statuses, got %+v", status.Files)
	}
	if !status.PGConfigured || !status.EmbeddingConfigured {
		t.Fatalf("expected configured stores: %+v", status)
	}
}

func TestInspectKnowledgeWorkspaceStateTransitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	state, err := InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceEmpty || state.SourceCount != 0 {
		t.Fatalf("empty state=%+v err=%v", state, err)
	}

	source := filepath.Join(root, "raw", "sources", "alpha.md")
	if err := os.WriteFile(source, []byte("# Alpha\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	task, err := QueueIngestSource(QueueIngestOptions{ProjectPath: root, SourcePath: source, Title: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	state, err = InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceQueued || state.SourceCount != 1 || state.Queue.Pending != 1 {
		t.Fatalf("queued state=%+v err=%v", state, err)
	}

	queue, err := LoadIngestQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	queue.Tasks[0].Status = IngestTaskProcessing
	queue.Tasks[0].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := SaveIngestQueue(root, queue); err != nil {
		t.Fatal(err)
	}
	state, err = InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceProcessing || state.Queue.Processing != 1 {
		t.Fatalf("processing state=%+v err=%v", state, err)
	}

	queue.Tasks[0].Status = IngestTaskFailed
	queue.Tasks[0].Error = "provider unavailable"
	queue.Tasks[0].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := SaveIngestQueue(root, queue); err != nil {
		t.Fatal(err)
	}
	state, err = InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceFailed || state.LastError != "provider unavailable" || state.Queue.Failed != 1 {
		t.Fatalf("failed state=%+v err=%v", state, err)
	}

	completedAt := time.Now().UTC().Truncate(time.Microsecond)
	queue.Tasks[0].Status = IngestTaskDone
	queue.Tasks[0].Error = ""
	queue.Tasks[0].RawPath = "raw/sources/alpha.md"
	queue.Tasks[0].UpdatedAt = completedAt.Format(time.RFC3339Nano)
	if err := SaveIngestQueue(root, queue); err != nil {
		t.Fatal(err)
	}
	if err := manifestfile.Save(root, manifestfile.File{Sources: map[string]manifestfile.Entry{
		"alpha.md": {OriginalPath: "alpha.md", RawPath: "raw/sources/alpha.md", Title: "Alpha", UpdatedAt: completedAt.Format(time.RFC3339Nano)},
	}}); err != nil {
		t.Fatal(err)
	}
	state, err = InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceReady || state.SourceCount != 1 || state.LastSuccessfulAt == nil || !state.LastSuccessfulAt.Equal(completedAt) {
		t.Fatalf("ready state=%+v err=%v task=%+v", state, err, task)
	}
}

func TestRecoverInterruptedKnowledgeMaintenancePreservesLastSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := finishKnowledgeMaintenance(root, true, ""); err != nil {
		t.Fatal(err)
	}
	before, found, err := loadKnowledgeMaintenanceState(root)
	if err != nil || !found || before.LastSuccessfulAt == "" {
		t.Fatalf("successful maintenance state=%+v found=%v err=%v", before, found, err)
	}
	if err := beginKnowledgeMaintenance(root); err != nil {
		t.Fatal(err)
	}

	recovered, err := RecoverInterruptedKnowledgeMaintenance(root)
	if err != nil || !recovered {
		t.Fatalf("recovered=%v err=%v", recovered, err)
	}
	after, found, err := loadKnowledgeMaintenanceState(root)
	if err != nil || !found {
		t.Fatalf("recovered maintenance state=%+v found=%v err=%v", after, found, err)
	}
	if after.Status != KnowledgeWorkspaceReady || after.Error != "" || after.LastSuccessfulAt != before.LastSuccessfulAt {
		t.Fatalf("recovered maintenance state=%+v before=%+v", after, before)
	}
	workspace, err := InspectKnowledgeWorkspace(root)
	if err != nil || workspace.Status != KnowledgeWorkspaceEmpty {
		t.Fatalf("recovery must not make an empty workspace ready: state=%+v err=%v", workspace, err)
	}
	recovered, err = RecoverInterruptedKnowledgeMaintenance(root)
	if err != nil || recovered {
		t.Fatalf("second recovery=%v err=%v", recovered, err)
	}
}

func TestMaintainWikiRunsDeterministicLoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "raw", "sources", "alpha.md")
	if err := os.WriteFile(source, []byte("# Alpha\n\nAlpha links Beta."), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := MaintainWiki(MaintainWikiOptions{
		ProjectPath:    root,
		ProjectID:      "project-1",
		QueueValidator: fakeWorkspaceValidator,
		SkipUnchanged:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%s steps=%+v", result.Status, result.Steps)
	}
	wantSteps := []string{"scan_sources", "run_ingest_queue", "refresh_navigation", "structural_lint", "llm_review", "sweep_reviews", "sync_pg"}
	if len(result.Steps) != len(wantSteps) {
		t.Fatalf("steps=%+v", result.Steps)
	}
	for i, want := range wantSteps {
		if result.Steps[i].Name != want {
			t.Fatalf("step %d=%s want %s", i, result.Steps[i].Name, want)
		}
	}
	if result.Steps[4].Status != "skipped" || result.Steps[6].Status != "skipped" {
		t.Fatalf("expected optional steps skipped: %+v", result.Steps)
	}
	logData, err := os.ReadFile(filepath.Join(root, "wiki", "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "maintain | workspace maintenance") {
		t.Fatalf("maintenance log missing:\n%s", logData)
	}
}

func TestMaintainWikiReportsLLMReviewAgentErrorAsStepFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	result, err := MaintainWiki(MaintainWikiOptions{
		ProjectPath:    root,
		ProjectID:      "project-1",
		QueueValidator: fakeWorkspaceValidator,
		RunLLMReview:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" {
		t.Fatalf("status=%s steps=%+v", result.Status, result.Steps)
	}
	if len(result.Steps) < 5 || result.Steps[4].Name != "llm_review" || result.Steps[4].Status != "failed" {
		t.Fatalf("expected llm_review failure, got %+v", result.Steps)
	}
}

func TestMaintainWikiCallsOnStep(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	var steps []string
	_, err := MaintainWiki(MaintainWikiOptions{
		ProjectPath:    root,
		ProjectID:      "project-1",
		QueueValidator: fakeWorkspaceValidator,
		OnStep: func(_ MaintainWikiResult, step MaintainWikiStep) {
			steps = append(steps, step.Name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 7 || steps[0] != "scan_sources" || steps[6] != "sync_pg" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestMaintainWikiExposesProcessingForWholeTick(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	var observed KnowledgeWorkspaceState
	var observedErr error
	_, err := MaintainWiki(MaintainWikiOptions{
		ProjectPath:    root,
		ProjectID:      "project-1",
		QueueValidator: fakeWorkspaceValidator,
		OnStep: func(_ MaintainWikiResult, step MaintainWikiStep) {
			if step.Name == "scan_sources" {
				observed, observedErr = InspectKnowledgeWorkspace(root)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observedErr != nil || observed.Status != KnowledgeWorkspaceProcessing {
		t.Fatalf("maintenance state=%+v err=%v", observed, observedErr)
	}
}

func TestMaintainWikiPersistsStepFailures(t *testing.T) {
	t.Run("ingest", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "kb")
		if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "raw", "sources", "alpha.md"), []byte("# Alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		result, err := MaintainWiki(MaintainWikiOptions{
			ProjectPath: root,
			ProjectID:   "project-1",
			QueueValidator: func(QueueValidateOptions) (QueueValidateResult, error) {
				return QueueValidateResult{}, errors.New("compiler unavailable")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertFailedMaintenanceState(t, root, result, "run_ingest_queue")
	})

	t.Run("overview", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "kb")
		if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "raw", "sources", "alpha.md"), []byte("# Alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		result, err := MaintainWiki(MaintainWikiOptions{
			ProjectPath:    root,
			ProjectID:      "project-1",
			QueueValidator: fakeWorkspaceValidator,
			RefreshOverview: func() (bool, error) {
				return false, errors.New("overview provider unavailable")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertFailedMaintenanceState(t, root, result, "refresh_navigation")
	})

	t.Run("pg sync", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "kb")
		if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
			t.Fatal(err)
		}
		result, err := MaintainWiki(MaintainWikiOptions{
			ProjectPath:    root,
			ProjectID:      "project-1",
			QueueValidator: fakeWorkspaceValidator,
			RunPGSync:      true,
			WikiStore:      failingWorkspaceWikiStore{},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertFailedMaintenanceState(t, root, result, "sync_pg")
	})
}

func TestMaintainWikiRetriesFailedOverviewAndClearsFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "raw", "sources", "alpha.md"), []byte("# Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	refresh := func() (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("temporary overview failure")
		}
		return true, wiki.WriteVersionedPage(root, "wiki/overview.md", []byte("# Wiki Overview\n\nRecovered.\n"), "test overview recovery")
	}
	first, err := MaintainWiki(MaintainWikiOptions{ProjectPath: root, ProjectID: "project-1", QueueValidator: fakeWorkspaceValidator, SkipUnchanged: true, RefreshOverview: refresh})
	if err != nil {
		t.Fatal(err)
	}
	assertFailedMaintenanceState(t, root, first, "refresh_navigation")
	if err := manifestfile.Save(root, manifestfile.File{Sources: map[string]manifestfile.Entry{
		"alpha.md": {
			OriginalPath: "alpha.md",
			RawPath:      "raw/sources/alpha.md",
			Title:        "Alpha",
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}); err != nil {
		t.Fatal(err)
	}

	second, err := MaintainWiki(MaintainWikiOptions{ProjectPath: root, ProjectID: "project-1", QueueValidator: fakeWorkspaceValidator, SkipUnchanged: true, RefreshOverview: refresh})
	if err != nil || second.Status != "ok" || calls != 2 {
		t.Fatalf("second maintenance=%+v calls=%d err=%v", second, calls, err)
	}
	state, err := InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceReady || state.LastError != "" || state.LastSuccessfulAt == nil {
		t.Fatalf("recovered state=%+v err=%v", state, err)
	}
}

func TestMaintainWikiRefreshesAndVersionsOverviewOnlyAfterChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "raw", "sources", "alpha.md"), []byte("# Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	refresh := func() (bool, error) {
		calls++
		content := []byte("# Wiki Overview\n\nRefresh one.\n")
		return true, wiki.WriteVersionedPage(root, "wiki/overview.md", content, "test changed-source overview")
	}
	first, err := MaintainWiki(MaintainWikiOptions{ProjectPath: root, ProjectID: "project-1", QueueValidator: fakeWorkspaceValidator, SkipUnchanged: true, RefreshOverview: refresh})
	if err != nil || first.Status != "ok" || calls != 1 {
		t.Fatalf("first maintenance=%+v calls=%d err=%v", first, calls, err)
	}
	versions, err := os.ReadDir(filepath.Join(root, ".kbcore", "page-versions", "wiki", "overview"))
	if err != nil || len(versions) == 0 {
		t.Fatalf("overview versions=%d err=%v", len(versions), err)
	}

	second, err := MaintainWiki(MaintainWikiOptions{ProjectPath: root, ProjectID: "project-1", QueueValidator: fakeWorkspaceValidator, SkipUnchanged: true, RefreshOverview: refresh})
	if err != nil || second.Status != "ok" || calls != 1 {
		t.Fatalf("unchanged maintenance=%+v calls=%d err=%v", second, calls, err)
	}
}

func TestWorkspaceMaintainJobStoreCreatesExistingAndRecoversStale(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	req := WorkspaceMaintainRequest{Agent: "llm", SkipUnchanged: true}
	job, existing, err := CreateWorkspaceMaintainJob(root, "project-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if existing || job.ID == "" || job.Status != "queued" {
		t.Fatalf("unexpected job existing=%v job=%+v", existing, job)
	}
	second, existing, err := CreateWorkspaceMaintainJob(root, "project-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || second.ID != job.ID {
		t.Fatalf("expected existing job, got existing=%v job=%+v", existing, second)
	}
	workspaceJobMu.Lock()
	delete(activeWorkspaceJob, workspaceJobActiveKey(root, job.ID))
	workspaceJobMu.Unlock()
	if err := RecoverWorkspaceJobs(root); err != nil {
		t.Fatal(err)
	}
	recovered, err := GetWorkspaceJob(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "failed" || !strings.Contains(recovered.Error, "server stopped") {
		t.Fatalf("expected stale job failed, got %+v", recovered)
	}
}

func fakeWorkspaceValidator(opts QueueValidateOptions) (QueueValidateResult, error) {
	return QueueValidateResult{
		RawPath: filepath.ToSlash(filepath.Join("raw", "sources", filepath.Base(opts.SourcePath))),
		Files:   []string{"wiki/sources/" + strings.TrimSuffix(filepath.Base(opts.SourcePath), filepath.Ext(opts.SourcePath)) + ".md"},
		SHA256:  "test-sha",
	}, nil
}

func assertFailedMaintenanceState(t *testing.T, root string, result MaintainWikiResult, stepName string) {
	t.Helper()
	if result.Status != "failed" {
		t.Fatalf("maintenance=%+v", result)
	}
	found := false
	for _, step := range result.Steps {
		if step.Name == stepName && step.Status == "failed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing failed %s step: %+v", stepName, result.Steps)
	}
	state, err := InspectKnowledgeWorkspace(root)
	if err != nil || state.Status != KnowledgeWorkspaceFailed || state.LastError == "" {
		t.Fatalf("failed state=%+v err=%v", state, err)
	}
}

type failingWorkspaceWikiStore struct{}

func (failingWorkspaceWikiStore) UpsertWikiPages(context.Context, []core.WikiPage) error {
	return errors.New("pg unavailable")
}

func (failingWorkspaceWikiStore) DeleteWikiPagesNotIn(context.Context, string, []string) error {
	return nil
}

func (failingWorkspaceWikiStore) WikiPageEmbeddingStatus(context.Context, string, string) (core.WikiPageEmbeddingStatus, error) {
	return core.WikiPageEmbeddingStatus{}, nil
}

func (failingWorkspaceWikiStore) UpsertWikiPageEmbedding(context.Context, string, string, []float32, core.WikiPageEmbeddingMetadata) error {
	return nil
}
