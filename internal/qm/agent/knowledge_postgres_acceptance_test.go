package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	knowlegapostgres "github.com/loon-hejw/knowlega/internal/agent/knowlega/postgres"
	knowledgeservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestXiyoujiCorpusSynchronizesCompletePostgresProjection(t *testing.T) {
	dsn := os.Getenv("KBCORE_TEST_POSTGRES_DSN")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("KBCORE_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := knowlegapostgres.NewStore(db)
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	projectID := fmt.Sprintf("xiyouji-pg-acceptance-%d", time.Now().UTC().UnixNano())
	knowledgeAgent, err := knowlega.New(knowlega.AgentOptions{
		RootDir:     t.TempDir(),
		Compiler:    compiler.MockProvider{},
		WikiStore:   store,
		SearchStore: store,
		GraphStore:  store,
		ProjectIDFor: func(knowlega.ScopeRef) string {
			return projectID
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := knowlega.ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-" + projectID, Kind: "project", Name: "西游记 PostgreSQL 验收"}
	status, err := knowledgeAgent.EnsureScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM projects WHERE id=$1", status.ProjectID)
	})
	sourceDir := filepath.Join("..", "..", "..", "tst", "xiyouji-chapters")
	compiled, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath: status.ProjectPath,
		SourcePath:  sourceDir,
		Provider:    compiler.MockProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err = knowledgeAgent.RefreshScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.SourceCount != 100 || compiled.FileCount != 300 || compiled.ReviewCount != 100 || status.SourceCount != 100 || status.State != knowledgeservice.KnowledgeWorkspaceReady {
		t.Fatalf("compiled=%+v status=%+v", compiled, status)
	}
	if summaries := countAcceptanceMarkdown(t, filepath.Join(status.ProjectPath, "wiki", "sources")); summaries != 100 {
		t.Fatalf("source summaries=%d", summaries)
	}
	manifestEntries, err := compiler.LoadSourceManifestEntries(status.ProjectPath, status.ProjectID)
	if err != nil || len(manifestEntries) != 100 {
		t.Fatalf("manifest entries=%d err=%v", len(manifestEntries), err)
	}
	for _, entry := range manifestEntries {
		rawPath := entry.OriginalRawPath
		if rawPath == "" {
			rawPath = entry.RawPath
		}
		info, statErr := os.Stat(filepath.Join(status.ProjectPath, filepath.FromSlash(rawPath)))
		if statErr != nil || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("mutable source %s info=%v err=%v", rawPath, info, statErr)
		}
	}
	overviewPath := filepath.Join(status.ProjectPath, "wiki", "overview.md")
	overview, err := os.ReadFile(overviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(status.ProjectPath, "wiki/overview.md", append(overview, []byte("\nPostgreSQL acceptance checkpoint.\n")...), "xiyouji postgres acceptance version"); err != nil {
		t.Fatal(err)
	}
	if err := knowledgeAgent.SyncWiki(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{
		"source_manifest": 100,
		"sources":         100,
		"review_items":    100,
		"wiki_pages":      int(status.WikiPageCount),
	} {
		if got := knowledgePostgresAcceptanceCount(t, db, table, status.ProjectID); got != want {
			t.Fatalf("%s rows=%d want=%d", table, got, want)
		}
	}
	if versions := knowledgePostgresAcceptanceCount(t, db, "wiki_page_versions", status.ProjectID); versions == 0 {
		t.Fatal("PostgreSQL page versions were not synchronized")
	}
	issues, err := knowledgeservice.LintWiki(status.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.Type == "broken-link" {
			t.Fatalf("broken wikilink: %+v", issue)
		}
	}
	results, err := knowledgeAgent.Search(t.Context(), ref, "灵根育孕源流出", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"raw/", "wiki/sources/", "wiki/concepts/", "wiki/entities/"} {
		path := acceptanceResultPath(results, prefix)
		if path == "" {
			t.Fatalf("PostgreSQL-backed search missing %s: %+v", prefix, results)
		}
		if _, err := knowledgeAgent.Read(ref, path); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}
}

func knowledgePostgresAcceptanceCount(t *testing.T, db *sql.DB, table, projectID string) int {
	t.Helper()
	allowed := map[string]bool{
		"wiki_pages": true, "wiki_page_versions": true, "source_manifest": true,
		"sources": true, "review_items": true,
	}
	if !allowed[table] {
		t.Fatalf("unsupported table %q", table)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id=$1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
