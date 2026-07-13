package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestGraphManagerQueuesCoalescesAndIndexesManagedRepository(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "graph-manager"}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "source-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package demo\nfunc Entry(){ Leaf() }\nfunc Leaf(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "init", "-b", "main")
	runTestGit(t, repo, "add", "main.go")
	runTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	graphConfig := config.GraphConfig{
		Enabled: true, Worker: true, CheckoutRoot: filepath.Join(t.TempDir(), "checkouts"), MaxParallelJobs: 1,
		Registries: []config.CodeRegistryConfig{{
			ID: "local", Provider: "gitea", BaseURL: "https://git.example.test", WebhookSecret: "secret", Directory: "local",
			Repositories: []config.CodeRepositoryConfig{{ID: "demo", FullName: "team/demo", CloneURL: repo, Branch: "main"}},
		}},
	}
	wikiStore := &recordingWikiPageStore{}
	manager, err := NewGraphManager(GraphManagerOptions{ProjectPath: project, ProjectID: "project", Config: graphConfig, WikiStore: wikiStore})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Queue(QueueGraphJobOptions{RegistryID: "local", RepositoryID: "demo", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Queue(QueueGraphJobOptions{RegistryID: "local", RepositoryID: "demo", Trigger: "manual-latest"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("coalesced jobs should preserve audit history with distinct IDs")
	}
	completed, err := manager.RunPendingOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Status != GraphJobDone || completed[0].SnapshotPath == "" {
		t.Fatalf("completed=%+v", completed)
	}
	jobs, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[1].Status != GraphJobSuperseded {
		t.Fatalf("jobs=%+v", jobs)
	}
	checkout := filepath.Join(graphConfig.CheckoutRoot, "local", "demo")
	if _, err := os.Stat(filepath.Join(checkout, ".kbcore-managed-repo.json")); err != nil {
		t.Fatalf("managed marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(completed[0].SnapshotPath))); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if !containsSyncedWikiPath(wikiStore.pages, "wiki/code/demo/overview.md") {
		t.Fatalf("generated code wiki pages were not synced: %+v", wikiStore.pages)
	}
}

func containsSyncedWikiPath(pages []core.WikiPage, want string) bool {
	for _, page := range pages {
		if page.Path == want {
			return true
		}
	}
	return false
}

func TestGraphManagerVerifiesGitHubWebhookAndAllowlist(t *testing.T) {
	graphConfig := config.GraphConfig{MaxParallelJobs: 1, Registries: []config.CodeRegistryConfig{{
		ID: "github", Provider: "github", BaseURL: "https://github.example.test", WebhookSecret: "top-secret", Directory: "github",
		Repositories: []config.CodeRepositoryConfig{{ID: "demo", FullName: "team/demo", CloneURL: "https://github.example.test/team/demo.git", Branch: "main"}},
	}}}
	project := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "webhook"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewGraphManager(GraphManagerOptions{ProjectPath: project, Config: graphConfig})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main","after":"abcdef123456","repository":{"full_name":"team/demo"}}`)
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "push")
	headers.Set("X-GitHub-Delivery", "delivery-1")
	headers.Set("X-Hub-Signature-256", "sha256="+webhookSignature("top-secret", body))
	result, err := manager.HandleWebhook("github", headers, body)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Ignored || result.Job.Status != GraphJobPending {
		t.Fatalf("result=%+v", result)
	}
	duplicate, err := manager.HandleWebhook("github", headers, body)
	if err != nil || duplicate.Job.ID != result.Job.ID {
		t.Fatalf("delivery dedupe failed: duplicate=%+v err=%v", duplicate, err)
	}
	headers.Set("X-Hub-Signature-256", "sha256=00")
	if _, err := manager.HandleWebhook("github", headers, body); err == nil {
		t.Fatal("invalid signature was accepted")
	}
}

func webhookSignature(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func runTestGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
