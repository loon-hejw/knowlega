package knowlega

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
)

func TestQMFileCreatesReadOnlyRebuildableMirrorAndCanBeRemovedWhileQueued(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-p1", Kind: "project", Name: "Project"}
	content := []byte("# Project source\n")
	sum := sha256.Sum256(content)
	task, err := agent.EnqueueQMFile(ref, "file-1", "source.md", hex.EncodeToString(sum[:]), content)
	if err != nil {
		t.Fatal(err)
	}
	status, err := agent.Status(ref)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(status.ProjectPath, filepath.FromSlash(task.SourcePath))
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, "raw/sources/qm/file-1/") {
		t.Fatalf("raw mirror path=%s", rel)
	}
	info, err := os.Stat(task.SourcePath)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("raw mirror info=%v err=%v", info, err)
	}
	if _, err := agent.RemoveQMFile(ref, rel, task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFreshKnowledgeScopeIsEmptyNotReady(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	status, err := agent.EnsureScope(t.Context(), ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-empty", Kind: "project", Name: "Empty"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != service.KnowledgeWorkspaceEmpty || status.Ready || status.SourceCount != 0 {
		t.Fatalf("status=%+v", status)
	}
}

func TestQMFileRawEvidenceIsVisibleBeforeBackgroundCompilation(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir(), Compiler: compiler.MockProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-p1", Kind: "project", Name: "Project"}
	content := []byte("# Release Plan\n\nDeploy after review.\n")
	sum := sha256.Sum256(content)
	if _, err := agent.EnqueueQMFile(ref, "file-1", "release.md", hex.EncodeToString(sum[:]), content); err != nil {
		t.Fatal(err)
	}
	status, err := agent.Status(ref)
	if err != nil || status.State != service.KnowledgeWorkspaceQueued || status.Ready || status.SourceCount != 1 || status.Queue.Pending != 1 {
		t.Fatalf("queued status=%+v err=%v", status, err)
	}
	results, err := agent.Search(t.Context(), ref, "Deploy after review", 10)
	if err != nil || !containsAgentKnowledgeKind(results, "raw-source") {
		t.Fatalf("raw search results=%+v err=%v", results, err)
	}
	maintained, err := agent.Maintain(context.Background(), ref, false)
	if err != nil || maintained.Status != "ok" {
		t.Fatalf("maintain=%+v err=%v", maintained, err)
	}
	status, err = agent.Status(ref)
	if err != nil || status.State != service.KnowledgeWorkspaceReady || !status.Ready || status.LastSuccessfulAt == nil || status.WikiPageCount <= 4 {
		t.Fatalf("ready status=%+v err=%v", status, err)
	}
}

func TestAgentMaintainReturnsFailedStepAsError(t *testing.T) {
	provider := failingOverviewProvider{MockProvider: compiler.MockProvider{}}
	agent, err := New(AgentOptions{RootDir: t.TempDir(), Compiler: provider})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-failed-maintenance", Kind: "project", Name: "Project"}
	content := []byte("# Release Plan\n\nDeploy after review.\n")
	sum := sha256.Sum256(content)
	if _, err := agent.EnqueueQMFile(ref, "file-1", "release.md", hex.EncodeToString(sum[:]), content); err != nil {
		t.Fatal(err)
	}
	result, err := agent.Maintain(t.Context(), ref, false)
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "refresh_navigation") {
		t.Fatalf("maintenance=%+v err=%v", result, err)
	}
	status, statusErr := agent.Status(ref)
	if statusErr != nil || status.State != service.KnowledgeWorkspaceFailed || status.LastError == "" {
		t.Fatalf("status=%+v err=%v", status, statusErr)
	}
}

func TestAgentExposesDeterministicKnowledgeFactsAndWriteback(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "local", ExternalScopeID: "project-1", Kind: "project", Name: "Demo"}
	status, err := agent.EnsureScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	page := "---\ntitle: Release\ntype: entity\naliases: [Deploy Guide]\nsources: [raw/sources/release.md]\n---\n\n# Release\n\nDeploy after review.\n"
	if err := os.MkdirAll(filepath.Join(status.ProjectPath, "wiki", "entities"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(status.ProjectPath, "wiki", "entities", "release.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(status.ProjectPath, "raw", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(status.ProjectPath, "raw", "sources", "release.md"), []byte("# Release source\nDeploy after review."), 0o444); err != nil {
		t.Fatal(err)
	}

	results, err := agent.Search(t.Context(), ref, "Deploy Guide", 5)
	if err != nil || len(results) == 0 || results[0].Path != "wiki/entities/release.md" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	doc, err := agent.Read(ref, "Deploy Guide")
	if err != nil || doc.Path != "wiki/entities/release.md" {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	pages, err := agent.List(ref, "Release", 5)
	if err != nil || len(pages) == 0 {
		t.Fatalf("pages=%+v err=%v", pages, err)
	}

	submission := core.KnowledgeSubmission{
		Question: "How is Release deployed?", Answer: "Deploy after review.", Status: "complete",
		Citations: []core.KnowledgeCitation{{Path: doc.Path, Title: doc.Title, Kind: doc.Kind}},
	}
	writeback, err := agent.Writeback(t.Context(), ref, "Release Guidance", submission)
	if err != nil || !writeback.Written || !strings.HasPrefix(writeback.Path, "wiki/syntheses/") {
		t.Fatalf("writeback=%+v err=%v", writeback, err)
	}
	again, err := agent.Writeback(t.Context(), ref, "Release Guidance", submission)
	if err != nil || again.Written {
		t.Fatalf("idempotent writeback=%+v err=%v", again, err)
	}
}

func TestAgentStatusCountsLogicalManifestSourcesInsteadOfRawMirrors(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-p1", Kind: "project", Name: "Project"}
	status, err := agent.EnsureScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chapter-001.txt", "qm/file-1/chapter-001.txt"} {
		path := filepath.Join(status.ProjectPath, "raw", "sources", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("same logical source"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := manifestfile.Save(status.ProjectPath, manifestfile.File{Sources: map[string]manifestfile.Entry{"chapter-001.txt": {RawPath: "raw/sources/chapter-001.txt", Title: "chapter 1"}}}); err != nil {
		t.Fatal(err)
	}
	status, err = agent.Status(ref)
	if err != nil || status.SourceCount != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestAgentMaintenanceConcurrencyIsScopedPerProject(t *testing.T) {
	agent, err := New(AgentOptions{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	first := agent.scopeMaintenanceLock("project-1")
	same := agent.scopeMaintenanceLock("project-1")
	other := agent.scopeMaintenanceLock("project-2")
	if first != same || first == other {
		t.Fatalf("maintenance locks are not project scoped: first=%p same=%p other=%p", first, same, other)
	}
	first.Lock()
	defer first.Unlock()
	if same.TryLock() {
		same.Unlock()
		t.Fatal("same project acquired a second maintenance slot")
	}
	if !other.TryLock() {
		t.Fatal("different project was unnecessarily serialized")
	}
	other.Unlock()
}

func containsAgentKnowledgeKind(results []core.KnowledgeSearchResult, kind string) bool {
	for _, result := range results {
		if result.Kind == kind {
			return true
		}
	}
	return false
}

type failingOverviewProvider struct {
	compiler.MockProvider
}

func (failingOverviewProvider) SynthesizeOverview(compiler.OverviewInput) (string, error) {
	return "", errors.New("overview unavailable")
}
