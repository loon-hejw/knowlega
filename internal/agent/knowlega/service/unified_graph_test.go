package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestUnifiedProjectGraphCombinesWikiSourcesAndNativeCodeOnDemand(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "unified"}); err != nil {
		t.Fatal(err)
	}
	rawPath := "raw/sources/alpha.md"
	if err := os.WriteFile(filepath.Join(project, filepath.FromSlash(rawPath)), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(project, "wiki/concepts/alpha.md", []byte(`---
type: "concept"
title: "Alpha"
sources:
  - "raw/sources/alpha.md"
---

# Alpha

See [[beta]].
`), "test"); err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(project, "wiki/entities/beta.md", []byte(`---
type: "entity"
title: "Beta"
sources:
  - "raw/sources/alpha.md"
---

# Beta
`), "test"); err != nil {
		t.Fatal(err)
	}
	manifest := sourceManifestFile{Version: 1, Sources: map[string]sourceManifestFileEntry{
		"alpha": {OriginalPath: "alpha", RawPath: rawPath, ContentPath: rawPath, Title: "Alpha", SHA256: "hash"},
	}}
	if err := saveSourceManifestFile(project, manifest); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package demo\nfunc Entry(){ Leaf() }\nfunc Leaf(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := IndexGoCodeSnapshot(GoCodeIndexOptions{ProjectPath: project, RepoID: "demo", RepoPath: repo, Commit: "1234567890abcdef"}); err != nil {
		t.Fatal(err)
	}
	graph, err := BuildUnifiedProjectGraph(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"wiki", "source", "code"} {
		if graph.Stats.ByDomain[domain] == 0 {
			t.Fatalf("missing %s domain: %+v", domain, graph.Stats)
		}
	}
	for _, relation := range []string{"WIKILINK", "DERIVED_FROM", "RELATED_TO", "CALLS"} {
		if !apiGraphHasRelation(graph, relation) {
			t.Fatalf("missing %s relation", relation)
		}
	}
	subgraph, err := QueryProjectGraph(ProjectGraphQuery{ProjectPath: project, SeedIDs: []string{"wiki/concepts/alpha.md"}, Depth: 1, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(subgraph.Nodes) < 2 || len(subgraph.Nodes) >= len(graph.Nodes) {
		t.Fatalf("unexpected on-demand subgraph: nodes=%d full=%d", len(subgraph.Nodes), len(graph.Nodes))
	}
	if _, err := os.Stat(filepath.Join(project, ".kbcore", "relations.json")); err != nil {
		t.Fatalf("relations artifact missing: %v", err)
	}
}

func apiGraphHasRelation(graph WikiGraphAPIResult, relation string) bool {
	for _, edge := range graph.Edges {
		if edge.Relation == relation {
			return true
		}
	}
	return false
}
