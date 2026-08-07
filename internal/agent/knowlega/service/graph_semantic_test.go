package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type fakeGraphRelationAgent struct{}

func (fakeGraphRelationAgent) InferRelations(_ context.Context, _ GraphRelationInput) ([]GraphRelationSuggestion, error) {
	return []GraphRelationSuggestion{
		{Source: "wiki/concepts/alpha.md", Target: "wiki/entities/beta.md", Relation: "SUPPORTS", ConfidenceScore: 0.82, Rationale: "Alpha explicitly supports Beta."},
		{Source: "missing", Target: "wiki/entities/beta.md", Relation: "SUPPORTS", ConfidenceScore: 0.9},
		{Source: "wiki/entities/beta.md", Target: "wiki/concepts/alpha.md", Relation: "MADE_UP", ConfidenceScore: 0.9},
	}, nil
}

func TestGraphSemanticEnrichmentValidatesAndPersistsLLMRelations(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "semantic"}); err != nil {
		t.Fatal(err)
	}
	for rel, title := range map[string]string{"wiki/concepts/alpha.md": "Alpha", "wiki/entities/beta.md": "Beta"} {
		page := wiki.RenderPage(wiki.Page{Title: title, Type: "concept", Body: "# " + title + "\n"})
		if err := wiki.WriteVersionedPage(project, rel, []byte(page), "test"); err != nil {
			t.Fatal(err)
		}
	}
	enricher := &graphSemanticEnricher{agent: fakeGraphRelationAgent{}}
	count, err := enricher.Enrich(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("accepted=%d", count)
	}
	graph, err := BuildUnifiedProjectGraph(project)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, edge := range graph.Edges {
		if edge.Source == "wiki/concepts/alpha.md" && edge.Target == "wiki/entities/beta.md" && edge.Relation == "SUPPORTS" {
			found = true
			if edge.Confidence != "INFERRED" || edge.ConfidenceScore != 0.82 || edge.Props["generated_by"] != "llm" {
				t.Fatalf("edge=%+v", edge)
			}
		}
	}
	if !found {
		t.Fatalf("semantic edge missing: %+v", graph.Edges)
	}
}
