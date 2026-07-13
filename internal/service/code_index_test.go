package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestIndexGoCodeSnapshotWritesPortableGraphAndVersionedWikiPages(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "code"}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package demo
func Entry(){ Middle() }
func Middle(){ Leaf() }
func Leaf(){}
`
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := IndexGoCodeSnapshot(GoCodeIndexOptions{ProjectPath: project, RepoID: "demo", RepoPath: repo, Commit: "abcdef0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Nodes == 0 || result.Edges == 0 || result.SnapshotPath == "" || result.OverviewPath != "wiki/code/demo/overview.md" {
		t.Fatalf("result=%+v", result)
	}
	for _, rel := range []string{result.SnapshotPath, result.ReportPath, result.OverviewPath, ".kbcore/graph-snapshots/code/demo/latest.json"} {
		if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	overview, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(result.OverviewPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overview), `graph_source: "go-native"`) || !strings.Contains(string(overview), `working_tree_dirty: "false"`) || !strings.Contains(string(overview), result.SourceSHA256) || !strings.Contains(string(overview), result.SnapshotPath) {
		t.Fatalf("overview missing graph provenance:\n%s", overview)
	}
	var oldCommunity string
	for _, rel := range result.WrittenPaths {
		if strings.Contains(rel, "/communities/") {
			oldCommunity = rel
			break
		}
	}
	if oldCommunity == "" {
		t.Fatal("expected one publishable community page")
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package demo\nfunc Only(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := IndexGoCodeSnapshot(GoCodeIndexOptions{ProjectPath: project, RepoID: "demo", RepoPath: repo, Commit: "abcdef0123456789"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(oldCommunity))); !os.IsNotExist(err) {
		t.Fatalf("stale community page was not pruned: %v", err)
	}
}
