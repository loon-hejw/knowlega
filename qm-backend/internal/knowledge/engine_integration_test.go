package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/qm-backend/internal/data"
)

type fakeQueryAgent struct {
	draft  QueryDraft
	prompt QueryPrompt
}

type fakeCompilerAgent struct {
	prompt CompilePrompt
}

type directQueryAgent struct {
	fakeQueryAgent
}

func (a *directQueryAgent) Plan(_ context.Context, _ QueryPlanningPrompt) ([]QueryAction, error) {
	return []QueryAction{{Kind: "read", Path: "wiki/concepts/release.md"}, {Kind: "final"}}, nil
}

func (a *fakeCompilerAgent) Compile(_ context.Context, prompt CompilePrompt) (CompileDraft, error) {
	a.prompt = prompt
	page := func(path, title string) GeneratedPage {
		return GeneratedPage{Path: path, Content: "---\ntitle: " + title + "\ntype: synthesis\nsources:\n  - " + prompt.SourcePath + "\n---\n\n# " + title + "\n\n[[Knowledge Index]] records this source.\n"}
	}
	return CompileDraft{Pages: []GeneratedPage{page("wiki/sources/source.md", "Source Summary"), page("wiki/index.md", "Knowledge Index"), page("wiki/overview.md", "Overview"), page("wiki/log.md", "Operational Log")}, Reviews: []string{"Check one uncertain claim."}}, nil
}

func (a *fakeQueryAgent) Answer(_ context.Context, prompt QueryPrompt) (QueryDraft, error) {
	a.prompt = prompt
	return a.draft, nil
}

func TestEngineInitializesAndSearchesScopeWorkspace(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE knowledge_scopes"); err != nil {
		t.Fatal(err)
	}
	engine, err := New(t.TempDir(), data.NewKnowledgeScopeRepository(pg), data.NewKnowledgeQueryRepository(pg))
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "acme", ExternalScopeID: "group:web", Kind: "project", Name: "Web Project"}
	status, err := engine.EnsureScope(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.WikiPageCount < 4 {
		t.Fatalf("unexpected initial status %#v", status)
	}
	pagePath := filepath.Join(status.Scope.RootPath, "wiki", "concepts", "release.md")
	if err := os.WriteFile(pagePath, []byte("---\ntitle: Release Process\ntype: concept\n---\n\n# Release Process\n\nDeploy changes after review.\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	results, err := engine.Search(ctx, ref, "deploy review", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Path != "wiki/concepts/release.md" {
		t.Fatalf("unexpected results %#v", results)
	}
	document, err := engine.ReadDocument(ctx, ref, "wiki/concepts/release.md")
	if err != nil || document.Title != "Release Process" {
		t.Fatalf("document=%#v err=%v", document, err)
	}
	if _, err := engine.ReadDocument(ctx, ref, "../schema.md"); err == nil {
		t.Fatal("expected escaped document path to fail")
	}
	if _, err := engine.ReadDocument(ctx, ref, "wiki/../schema.md"); err == nil {
		t.Fatal("expected a path that leaves the wiki subtree to fail")
	}
	agent := &fakeQueryAgent{draft: QueryDraft{Answer: "Deploy after review.", CitationPaths: []string{"wiki/concepts/release.md"}}}
	query, err := engine.Query(ctx, ref, "How should we deploy?", "", 10, agent)
	if err != nil || query.Answer != "Deploy after review." || len(query.Citations) != 1 {
		t.Fatalf("query=%#v err=%v", query, err)
	}
	synthesisPath, err := engine.SaveQueryAnswer(ctx, ref, query.ID, "Deployment Guidance")
	if err != nil || synthesisPath != "wiki/syntheses/deployment-guidance.md" {
		t.Fatalf("synthesis path=%q err=%v", synthesisPath, err)
	}
	synthesis, err := engine.ReadDocument(ctx, ref, synthesisPath)
	if err != nil || !strings.Contains(synthesis.Content, "[[Release Process]]") || !strings.Contains(synthesis.Content, "## Query Plan") {
		t.Fatalf("synthesis=%#v err=%v", synthesis, err)
	}
	seen := map[string]bool{}
	for _, document := range agent.prompt.Documents {
		seen[document.Path] = true
	}
	for _, path := range []string{"purpose.md", "schema.md", "wiki/index.md", "wiki/overview.md", "wiki/concepts/release.md"} {
		if !seen[path] {
			t.Fatalf("query agent did not receive required read evidence %q", path)
		}
	}
	direct := &directQueryAgent{fakeQueryAgent: fakeQueryAgent{draft: QueryDraft{Answer: "Read directly.", CitationPaths: []string{"wiki/concepts/release.md"}}}}
	directResult, err := engine.Query(ctx, ref, "What is the release process?", "", 10, direct)
	if err != nil || len(directResult.SearchResults) != 0 || !strings.Contains(directResult.QueryPlan, "read: wiki/concepts/release.md") {
		t.Fatalf("direct query=%#v err=%v", directResult, err)
	}
	_, err = engine.Query(ctx, ref, "How should we deploy?", "", 10, &fakeQueryAgent{draft: QueryDraft{Answer: "unsupported", CitationPaths: []string{"wiki/not-read.md"}}})
	if err == nil {
		t.Fatal("expected unread citation to fail")
	}
	compiler := &fakeCompilerAgent{}
	ingested, err := engine.Ingest(ctx, ref, "release-notes.txt", []byte("Release after review."), compiler)
	if err != nil || ingested.SkippedUnchanged || len(ingested.GeneratedPaths) != 4 || len(ingested.Reviews) != 1 {
		t.Fatalf("ingested=%#v err=%v", ingested, err)
	}
	if compiler.prompt.SourcePath != ingested.RawPath {
		t.Fatalf("compiler raw path=%q result=%q", compiler.prompt.SourcePath, ingested.RawPath)
	}
	skipped, err := engine.Ingest(ctx, ref, "release-notes.txt", []byte("Release after review."), compiler)
	if err != nil || !skipped.SkippedUnchanged || skipped.RawPath != ingested.RawPath {
		t.Fatalf("skipped=%#v err=%v", skipped, err)
	}
	updated, err := engine.Ingest(ctx, ref, "release-notes.txt", []byte("Release after review and approval."), compiler)
	if err != nil || updated.SkippedUnchanged {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	archives, err := filepath.Glob(filepath.Join(status.Scope.RootPath, ".kbcore", "page-versions", "index.md.*.md"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("archives=%#v err=%v", archives, err)
	}
}
