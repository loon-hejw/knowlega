package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestQuerySaveTitleRejectsOfflineFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "concepts", "oauth.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`# OAuth Token Validation

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&q=token+validation&save_title=Token+Validation", nil)
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not eligible for writeback") {
		t.Fatalf("unexpected body=%s", rr.Body.String())
	}
}

func TestQuerySaveTitleSyncsWrittenSynthesisWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "oauth.md"), []byte(`---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	agent := fakeAPIQueryAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/sources/oauth.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&project_id=project-1&q=token+auth&save_title=Token+Auth", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store, QueryAgent: agent}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/syntheses/token-auth.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected query writeback pages to sync, got %+v", store.pages)
	}
}

func TestQuerySaveTitleAutoUsesAgentSuggestedWritebackTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "oauth.md"), []byte(`---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	agent := &fakeAPIActionAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "read", Path: "wiki/sources/oauth.md", Rationale: "read evidence"},
			{Action: "writeback", Title: "Token Auth Auto", Answer: "Token validation calls AuthService [wiki/sources/oauth.md].", Rationale: "save reusable answer"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&project_id=project-1&q=token+auth&save_title=auto", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store, QueryAgent: agent}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/syntheses/token-auth-auto.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected auto-titled query writeback pages to sync, got %+v", store.pages)
	}
}

func TestIngestSyncsWikiAndSourceManifestWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "oauth.md")
	if err := os.WriteFile(sourcePath, []byte("# OAuth\n\nToken validation."), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	body, _ := json.Marshal(map[string]string{
		"project_path": root,
		"project_id":   "project-1",
		"source_path":  sourcePath,
		"title":        "OAuth Notes",
	})
	req := httptest.NewRequest(http.MethodPost, "/projects/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/sources/oauth.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected written wiki pages to sync, got %+v", store.pages)
	}
	if len(store.sourceManifestEntries) != 1 {
		t.Fatalf("expected source manifest sync, got %+v", store.sourceManifestEntries)
	}
	if len(store.sources) != 1 || store.sources[0].Path != store.sourceManifestEntries[0].RawPath {
		t.Fatalf("expected raw source provenance sync, sources=%+v manifest=%+v", store.sources, store.sourceManifestEntries)
	}
}

type fakeAPIQueryAgent struct {
	plan core.QueryPlan
}

func (f fakeAPIQueryAgent) PlanQuery(service.QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (fakeAPIQueryAgent) SynthesizeQuery(service.QuerySynthesisInput) (string, error) {
	return "Token validation calls AuthService [wiki/sources/oauth.md].", nil
}

type fakeAPIActionAgent struct {
	plan    core.QueryPlan
	actions []core.QueryAction
	index   int
}

func (f *fakeAPIActionAgent) PlanQuery(service.QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (f *fakeAPIActionAgent) NextQueryAction(service.QueryActionInput) (core.QueryAction, error) {
	if f.index >= len(f.actions) {
		return core.QueryAction{Action: "final", Answer: "done"}, nil
	}
	action := f.actions[f.index]
	f.index++
	return action, nil
}

func (*fakeAPIActionAgent) SynthesizeQuery(service.QuerySynthesisInput) (string, error) {
	return "synthesized fallback", nil
}

func TestCodeImportGraphifySyncsGraphAndWikiWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	graphPath := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(graphPath, []byte(`{
  "nodes": [
    {"id":"file:main.go","kind":"file","label":"main.go","source_file":"main.go"},
    {"id":"fn:run","kind":"function","label":"run","source_file":"main.go"}
  ],
  "edges": [
    {"source":"file:main.go","target":"fn:run","relation":"defines","confidence":"EXTRACTED","weight":1}
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wikiStore := &recordingWikiStore{}
	graphStore := &recordingCodeGraphStore{}
	body, _ := json.Marshal(map[string]string{
		"project_path": root,
		"project_id":   "project-1",
		"repo_id":      "demo-repo",
		"repo_path":    "/tmp/demo-repo",
		"graph_path":   graphPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/code/import-graphify", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: wikiStore, CodeGraphStore: graphStore}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(graphStore.repos) != 1 || len(graphStore.nodes) != 2 || len(graphStore.edges) != 1 {
		t.Fatalf("expected graph sync, repos=%+v nodes=%+v edges=%+v", graphStore.repos, graphStore.nodes, graphStore.edges)
	}
	if graphStore.deletedProjectID != "project-1" || graphStore.deletedRepoID == "" {
		t.Fatalf("expected stale graph facts to be deleted first, store=%+v", graphStore)
	}
	if !hasWikiPage(wikiStore.pages, "wiki/code/demo-repo/overview.md") || !hasWikiPage(wikiStore.pages, "wiki/index.md") || !hasWikiPage(wikiStore.pages, "wiki/log.md") {
		t.Fatalf("expected code overview and aggregate wiki pages to sync, got %+v", wikiStore.pages)
	}
}

type recordingWikiStore struct {
	pages                 []core.WikiPage
	sources               []core.Source
	sourceManifestEntries []core.SourceManifestEntry
}

func (s *recordingWikiStore) UpsertWikiPages(_ context.Context, pages []core.WikiPage) error {
	s.pages = append(s.pages, pages...)
	return nil
}

func (s *recordingWikiStore) DeleteWikiPagesNotIn(context.Context, string, []string) error {
	return nil
}

func (s *recordingWikiStore) WikiPageEmbeddingStatus(context.Context, string, string) (core.WikiPageEmbeddingStatus, error) {
	return core.WikiPageEmbeddingStatus{}, nil
}

func (s *recordingWikiStore) UpsertWikiPageEmbedding(context.Context, string, string, []float32, core.WikiPageEmbeddingMetadata) error {
	return nil
}

func (s *recordingWikiStore) UpsertSource(_ context.Context, source core.Source) error {
	s.sources = append(s.sources, source)
	return nil
}

func (s *recordingWikiStore) UpsertSourceManifestEntry(_ context.Context, entry core.SourceManifestEntry) error {
	s.sourceManifestEntries = append(s.sourceManifestEntries, entry)
	return nil
}

func hasWikiPage(pages []core.WikiPage, path string) bool {
	for _, page := range pages {
		if page.Path == path {
			return true
		}
	}
	return false
}

var _ service.WikiPageStore = (*recordingWikiStore)(nil)

type recordingCodeGraphStore struct {
	repos            []core.CodeRepo
	nodes            []core.GraphNode
	edges            []core.GraphEdge
	deletedProjectID string
	deletedRepoID    string
}

func (s *recordingCodeGraphStore) UpsertCodeRepo(_ context.Context, repo core.CodeRepo) error {
	s.repos = append(s.repos, repo)
	return nil
}

func (s *recordingCodeGraphStore) DeleteGraphFacts(_ context.Context, projectID, repoID string) error {
	s.deletedProjectID = projectID
	s.deletedRepoID = repoID
	return nil
}

func (s *recordingCodeGraphStore) AddGraphNode(_ context.Context, node core.GraphNode) error {
	s.nodes = append(s.nodes, node)
	return nil
}

func (s *recordingCodeGraphStore) AddGraphEdge(_ context.Context, edge core.GraphEdge) error {
	s.edges = append(s.edges, edge)
	return nil
}

var _ service.CodeGraphStore = (*recordingCodeGraphStore)(nil)
