package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestImportGraphifyCodeSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	graph := filepath.Join(t.TempDir(), "graph.json")
	graphJSON := `{
	  "nodes": [
	    {"id":"file:main.go","label":"main.go","type":"file","source_file":"main.go"},
	    {"id":"fn:run","label":"run","type":"function","source_file":"main.go"}
	  ],
	  "edges": [
	    {"source":"file:main.go","target":"fn:run","relation":"defines","confidence":"EXTRACTED","weight":1}
	  ]
	}`
	if err := os.WriteFile(graph, []byte(graphJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ImportGraphifyCodeSnapshot(CodeImportOptions{
		ProjectPath: root,
		RepoID:      "demo-repo",
		RepoPath:    "/tmp/demo-repo",
		GraphPath:   graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeCount != 2 || result.EdgeCount != 1 {
		t.Fatalf("unexpected graph counts: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, result.Overview)); err != nil {
		t.Fatalf("expected overview: %v", err)
	}
}

func TestSyncCodeGraphSnapshotWritesRepoNodesAndEdges(t *testing.T) {
	store := &recordingCodeGraphStore{}
	importedAt := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	snap := codegraph.Snapshot{
		RepoID:     "demo-repo",
		RepoPath:   "/tmp/demo-repo",
		Commit:     "abc123",
		Source:     "graphify",
		ImportedAt: importedAt,
		Nodes: []codegraph.Node{
			{
				ID:         "file:main.go",
				Kind:       codegraph.NodeFile,
				Label:      "main.go",
				SourceFile: "main.go",
				Community:  "core",
				Props:      map[string]any{"language": "go"},
			},
			{
				ID:         "fn:run",
				Kind:       codegraph.NodeFunction,
				Label:      "run",
				SourceFile: "main.go",
			},
		},
		Edges: []codegraph.Edge{
			{
				Source:     "file:main.go",
				Target:     "fn:run",
				Relation:   codegraph.RelDefines,
				Confidence: "EXTRACTED",
				Weight:     1,
				Props:      map[string]any{"line": float64(12)},
			},
		},
	}

	result, err := SyncCodeGraphSnapshot(context.Background(), store, "project-1", snap, "raw/code-graphs/demo-repo/graphify/graph.json")
	if err != nil {
		t.Fatal(err)
	}
	if result.Nodes != 2 || result.Edges != 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(store.repos) != 1 {
		t.Fatalf("repos=%+v", store.repos)
	}
	repo := store.repos[0]
	if repo.ID == "demo-repo" || repo.ProjectID != "project-1" || repo.IndexedCommit != "abc123" || repo.PrimaryGraphSource != "graphify" {
		t.Fatalf("unexpected repo: %+v", repo)
	}
	if repo.SyncedAt == nil || !repo.SyncedAt.Equal(importedAt) {
		t.Fatalf("synced_at=%v", repo.SyncedAt)
	}
	if len(store.deleted) != 1 || store.deleted[0] != repo.ID {
		t.Fatalf("expected old graph facts to be deleted for repo %s, got %+v", repo.ID, store.deleted)
	}
	if len(store.ops) < 3 || store.ops[0] != "repo" || store.ops[1] != "delete" || store.ops[2] != "node" {
		t.Fatalf("expected repo upsert, stale fact delete, then node writes; ops=%+v", store.ops)
	}
	if len(store.nodes) != 2 {
		t.Fatalf("nodes=%+v", store.nodes)
	}
	if store.nodes[0].ID == "file:main.go" || store.nodes[0].RepoID != repo.ID || store.nodes[0].SourceRef != "raw/code-graphs/demo-repo/graphify/graph.json#node/file:main.go" {
		t.Fatalf("unexpected node: %+v", store.nodes[0])
	}
	if store.nodes[0].Props["raw_id"] != "file:main.go" || store.nodes[0].Props["language"] != "go" {
		t.Fatalf("node props=%+v", store.nodes[0].Props)
	}
	if len(store.edges) != 1 {
		t.Fatalf("edges=%+v", store.edges)
	}
	edge := store.edges[0]
	if edge.RepoID != repo.ID || edge.SourceID == "file:main.go" || edge.TargetID == "fn:run" {
		t.Fatalf("unexpected edge ids: %+v", edge)
	}
	if edge.Confidence != core.ConfidenceExtracted || edge.Props["raw_source"] != "file:main.go" || edge.Props["raw_target"] != "fn:run" {
		t.Fatalf("unexpected edge facts: %+v", edge)
	}
	if edge.Props["source_ref"] != "raw/code-graphs/demo-repo/graphify/graph.json#edge/file:main.go/fn:run" {
		t.Fatalf("edge source_ref=%+v", edge.Props["source_ref"])
	}
}

func TestSyncCodeGraphSnapshotUsesTransactionalStore(t *testing.T) {
	store := &transactionalRecordingCodeGraphStore{
		recordingCodeGraphStore: recordingCodeGraphStore{failEdge: true},
	}
	snap := codegraph.Snapshot{
		RepoID:   "demo-repo",
		RepoPath: "/tmp/demo-repo",
		Source:   "graphify",
		Nodes: []codegraph.Node{
			{ID: "file:main.go", Kind: codegraph.NodeFile, Label: "main.go"},
			{ID: "fn:run", Kind: codegraph.NodeFunction, Label: "run"},
		},
		Edges: []codegraph.Edge{
			{Source: "file:main.go", Target: "fn:run", Relation: codegraph.RelDefines, Confidence: "EXTRACTED", Weight: 1},
		},
	}

	_, err := SyncCodeGraphSnapshot(context.Background(), store, "project-1", snap, "raw/code-graphs/demo-repo/graphify/graph.json")
	if err == nil || !strings.Contains(err.Error(), "edge failed") {
		t.Fatalf("expected edge failure, got %v", err)
	}
	if !store.txCalled {
		t.Fatal("expected transactional graph store to be used")
	}
	if len(store.repos) != 0 || len(store.nodes) != 0 || len(store.edges) != 0 {
		t.Fatalf("transactional graph store should not commit partial graph sync: repos=%+v nodes=%+v edges=%+v", store.repos, store.nodes, store.edges)
	}
}

func TestIndexedCodeRevisionDistinguishesDirtySource(t *testing.T) {
	tests := []struct {
		name string
		snap codegraph.Snapshot
		want string
	}{
		{name: "clean", snap: codegraph.Snapshot{Commit: "abc123", SourceSHA256: strings.Repeat("a", 64)}, want: "abc123"},
		{name: "dirty commit", snap: codegraph.Snapshot{Commit: "abc123", Dirty: true, SourceSHA256: strings.Repeat("b", 64)}, want: "abc123+dirty.bbbbbbbbbbbb"},
		{name: "dirty worktree", snap: codegraph.Snapshot{Dirty: true, SourceSHA256: strings.Repeat("c", 64)}, want: "dirty.cccccccccccc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := indexedCodeRevision(test.snap); got != test.want {
				t.Fatalf("indexed revision=%q want=%q", got, test.want)
			}
		})
	}
}

type recordingCodeGraphStore struct {
	repos    []core.CodeRepo
	deleted  []string
	nodes    []core.GraphNode
	edges    []core.GraphEdge
	ops      []string
	failEdge bool
}

func (s *recordingCodeGraphStore) UpsertCodeRepo(_ context.Context, repo core.CodeRepo) error {
	s.ops = append(s.ops, "repo")
	s.repos = append(s.repos, repo)
	return nil
}

func (s *recordingCodeGraphStore) DeleteGraphFacts(_ context.Context, _ string, repoID string) error {
	s.ops = append(s.ops, "delete")
	s.deleted = append(s.deleted, repoID)
	return nil
}

func (s *recordingCodeGraphStore) AddGraphNode(_ context.Context, node core.GraphNode) error {
	s.ops = append(s.ops, "node")
	s.nodes = append(s.nodes, node)
	return nil
}

func (s *recordingCodeGraphStore) AddGraphEdge(_ context.Context, edge core.GraphEdge) error {
	s.ops = append(s.ops, "edge")
	if s.failEdge {
		return fmt.Errorf("edge failed")
	}
	s.edges = append(s.edges, edge)
	return nil
}

type transactionalRecordingCodeGraphStore struct {
	recordingCodeGraphStore
	txCalled bool
}

func (s *transactionalRecordingCodeGraphStore) WithCodeGraphStoreTx(ctx context.Context, fn func(CodeGraphStore) error) error {
	s.txCalled = true
	txStore := &recordingCodeGraphStore{failEdge: s.failEdge}
	if err := fn(txStore); err != nil {
		return err
	}
	s.repos = append(s.repos, txStore.repos...)
	s.deleted = append(s.deleted, txStore.deleted...)
	s.nodes = append(s.nodes, txStore.nodes...)
	s.edges = append(s.edges, txStore.edges...)
	s.ops = append(s.ops, txStore.ops...)
	return nil
}
