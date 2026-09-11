//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

// Run with a disposable pgvector-enabled database:
// KBCORE_TEST_POSTGRES_DSN=... go test -tags=integration ./internal/postgres
func TestStoreTransactionManifestReviewAndPruneIntegration(t *testing.T) {
	dsn := os.Getenv("KBCORE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("KBCORE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	projectID := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	if err := store.UpsertProject(ctx, core.Project{ID: projectID, Name: projectID, RootPath: "/tmp/" + projectID}); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DELETE FROM projects WHERE id=$1`, projectID)
	page := core.WikiPage{ID: core.StableID(projectID, "wiki/entities/a.md"), ProjectID: projectID, Path: "wiki/entities/a.md", Type: "entity", Title: "A", Body: "body", Frontmatter: map[string]any{"type": "entity"}}
	injected := errors.New("rollback")
	if err := store.WithWikiPageStoreTx(ctx, func(txStore core.WikiPageStore) error {
		if err := txStore.UpsertWikiPages(ctx, []core.WikiPage{page}); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("transaction error=%v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM wiki_pages WHERE project_id=$1`, projectID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	if err := store.WithWikiPageStoreTx(ctx, func(txStore core.WikiPageStore) error {
		return txStore.UpsertWikiPages(ctx, []core.WikiPage{page})
	}); err != nil {
		t.Fatal(err)
	}
	version := core.WikiPageVersion{
		ID: core.StableID(projectID, "version", page.Path), ProjectID: projectID,
		PageID: page.ID, Path: page.Path, Body: "older body",
		Frontmatter: map[string]any{"type": "entity"}, Reason: "integration",
	}
	if err := store.InsertWikiPageVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	entry := core.SourceManifestEntry{ID: core.StableID(projectID, "manifest", "a"), ProjectID: projectID, OriginalPath: "a", PipelineVersion: 3, SHA256: "sha", RawPath: "raw/sources/a.md", Title: "A", Files: []string{page.Path}, GenerationContractSHA256: "contract", NewPageBudget: 3, NewPageCount: 1, CreatedPages: []string{page.Path}}
	if err := store.UpsertSourceManifestEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	source := core.Source{ID: core.StableID(projectID, entry.RawPath), ProjectID: projectID, Path: entry.RawPath, Kind: "source", Title: entry.Title, SHA256: entry.SHA256, Immutable: true, OriginalPath: entry.OriginalPath}
	if err := store.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	review := core.ReviewItem{ID: core.StableID(projectID, "review", "a"), ProjectID: projectID, Type: "missing-page", Title: "A", Status: "open", SourcePath: entry.RawPath}
	if err := store.UpsertReviewItem(ctx, review); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM wiki_pages WHERE project_id=$1`, projectID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("page count=%d err=%v", count, err)
	}
	for table, want := range map[string]int{"wiki_page_versions": 1, "source_manifest": 1, "sources": 1, "review_items": 1} {
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE project_id=$1", table)
		if err := db.QueryRowContext(ctx, query, projectID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if err := store.DeleteWikiPagesNotIn(ctx, projectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM wiki_pages WHERE project_id=$1`, projectID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pruned count=%d err=%v", count, err)
	}
	if err := store.DeleteWikiPageVersionsNotIn(ctx, projectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSourceManifestEntriesNotIn(ctx, projectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSourcesNotIn(ctx, projectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteReviewItemsNotIn(ctx, projectID, nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"wiki_page_versions", "source_manifest", "sources", "review_items"} {
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE project_id=$1", table)
		if err := db.QueryRowContext(ctx, query, projectID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("pruned %s count=%d err=%v", table, count, err)
		}
	}
}
