package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if len(status.Files) != 5 {
		t.Fatalf("expected project file statuses, got %+v", status.Files)
	}
	if !status.PGConfigured || !status.EmbeddingConfigured {
		t.Fatalf("expected configured stores: %+v", status)
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
	wantSteps := []string{"scan_sources", "run_ingest_queue", "structural_lint", "llm_review", "sweep_reviews", "sync_pg"}
	if len(result.Steps) != len(wantSteps) {
		t.Fatalf("steps=%+v", result.Steps)
	}
	for i, want := range wantSteps {
		if result.Steps[i].Name != want {
			t.Fatalf("step %d=%s want %s", i, result.Steps[i].Name, want)
		}
	}
	if result.Steps[3].Status != "skipped" || result.Steps[5].Status != "skipped" {
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
	if len(result.Steps) < 4 || result.Steps[3].Name != "llm_review" || result.Steps[3].Status != "failed" {
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
	if len(steps) != 6 || steps[0] != "scan_sources" || steps[5] != "sync_pg" {
		t.Fatalf("unexpected steps: %+v", steps)
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
