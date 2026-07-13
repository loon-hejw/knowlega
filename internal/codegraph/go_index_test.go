package codegraph

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexGoRepositoryBuildsExactSemanticGraph(t *testing.T) {
	repo := t.TempDir()
	source := `package demo

import "net/http"

type BaseRunner interface { Run() error }
type Runner interface { BaseRunner }
type Outer struct { Inner }
type Inner struct{}
type Worker struct{}

func (Worker) Run() error { return nil }

func Register() { http.HandleFunc("GET /items", Entry) }
func Entry(http.ResponseWriter, *http.Request) { Middle() }
func Middle() { Leaf() }
func Leaf() {}
`
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := IndexGoRepository(GoIndexOptions{RepoID: "demo", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []NodeKind{NodePackage, NodeFile, NodeFunction, NodeMethod, NodeStruct, NodeInterface, NodeRoute, NodeCommunity, NodeProcess} {
		if countNodes(snapshot.Nodes, kind) == 0 {
			t.Fatalf("missing %s node in %+v", kind, snapshot.Nodes)
		}
	}
	for _, relation := range []Relation{RelContains, RelDefines, RelImports, RelCalls, RelHasMethod, RelImplements, RelHandlesRoute, RelMemberOf, RelStepInProcess} {
		if countEdges(snapshot.Edges, relation) == 0 {
			t.Fatalf("missing %s edge in %+v", relation, snapshot.Edges)
		}
	}
	if countEdges(snapshot.Edges, RelImplements) < 2 || countEdges(snapshot.Edges, RelEmbeds) < 2 {
		t.Fatalf("embedded interfaces/types were not resolved: edges=%+v", snapshot.Edges)
	}
	nodesByID := map[string]Node{}
	for _, node := range snapshot.Nodes {
		nodesByID[node.ID] = node
	}
	for _, edge := range snapshot.Edges {
		if edge.Relation == RelEmbeds && edge.Target != "" && nodesByID[edge.Target].Props["external"] == true {
			t.Fatalf("local embedded type was classified as external: %+v", edge)
		}
	}
	for _, edge := range snapshot.Edges {
		if edge.Relation == RelCalls && edge.Confidence == "EXTRACTED" && edge.ConfidenceScore != 1 {
			t.Fatalf("exact call confidence=%+v", edge)
		}
	}
	if snapshot.ReportBody == "" || len(snapshot.Communities) == 0 || len(snapshot.Processes) == 0 {
		t.Fatalf("analysis missing: communities=%+v processes=%+v report=%q", snapshot.Communities, snapshot.Processes, snapshot.ReportBody)
	}
	if len(snapshot.SourceSHA256) != 64 {
		t.Fatalf("source hash=%q", snapshot.SourceSHA256)
	}

	again, err := IndexGoRepository(GoIndexOptions{RepoID: "demo", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Nodes) != len(again.Nodes) || len(snapshot.Edges) != len(again.Edges) {
		t.Fatalf("unstable graph size: first=%d/%d second=%d/%d", len(snapshot.Nodes), len(snapshot.Edges), len(again.Nodes), len(again.Edges))
	}
	if again.SourceSHA256 != snapshot.SourceSHA256 {
		t.Fatalf("unstable source hash: %s != %s", snapshot.SourceSHA256, again.SourceSHA256)
	}
	for index := range snapshot.Nodes {
		if snapshot.Nodes[index].ID != again.Nodes[index].ID {
			t.Fatalf("unstable node id: %s != %s", snapshot.Nodes[index].ID, again.Nodes[index].ID)
		}
	}
}

func TestIndexGoRepositoryRecordsDirtyWorkingTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	mainPath := filepath.Join(repo, "main.go")
	if err := os.WriteFile(mainPath, []byte("package demo\nfunc One(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("-c", "user.name=kbcore", "-c", "user.email=kbcore@example.invalid", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repo, ".kbcore-managed-repo.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err := IndexGoRepository(GoIndexOptions{RepoID: "demo", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || clean.Commit == "" {
		t.Fatalf("clean snapshot=%+v", clean)
	}
	if err := os.WriteFile(mainPath, []byte("package demo\nfunc Two(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := IndexGoRepository(GoIndexOptions{RepoID: "demo", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.Commit != clean.Commit || dirty.SourceSHA256 == clean.SourceSHA256 {
		t.Fatalf("dirty provenance mismatch: clean=%+v dirty=%+v", clean, dirty)
	}
}

func TestIndexGoRepositoryRejectsUnsafeRepoID(t *testing.T) {
	if _, err := IndexGoRepository(GoIndexOptions{RepoID: "../escape", RepoPath: t.TempDir()}); err == nil {
		t.Fatal("expected unsafe repo id to be rejected")
	}
}

func countNodes(nodes []Node, kind NodeKind) int {
	count := 0
	for _, node := range nodes {
		if node.Kind == kind {
			count++
		}
	}
	return count
}

func countEdges(edges []Edge, relation Relation) int {
	count := 0
	for _, edge := range edges {
		if edge.Relation == relation {
			count++
		}
	}
	return count
}
