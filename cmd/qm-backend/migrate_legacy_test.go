package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestImportLegacyKnowledgeProjectRegistersQMAndPostgresState(t *testing.T) {
	databaseURL := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, databaseURL)
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE project_file_memberships,file_artifacts,knowledge_scopes,projects,durable_map_versions,deactivated_principals,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatal(err)
	}
	knowledgeStore, closeKnowledgeStore, err := openKnowledgeStore(ctx, pg, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeKnowledgeStore)
	knowledgeDB, err := sql.Open("pgx", withKnowledgeSearchPath(databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = knowledgeDB.Close() })
	if err := knowledgeDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.QM.OrgID = "acme"
	cfg.QM.FileStore.Mode = "local"
	cfg.QM.FileStore.LocalDir = filepath.Join(t.TempDir(), "files")
	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}}
	if err := data.NewDirectoryRepository(pg, cfg.QM.OrgID).Sync(ctx, data.DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	agent, err := knowlega.New(knowlega.AgentOptions{
		RootDir: t.TempDir(), Compiler: compiler.MockProvider{}, WikiStore: knowledgeStore, SearchStore: knowledgeStore, GraphStore: knowledgeStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	legacyRoot := filepath.Join(t.TempDir(), "legacy")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: legacyRoot, Name: "Legacy"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "legacy-release.md")
	sourceContent := []byte("# Legacy Release\n\nDeploy only after a signed review.\n")
	if err := os.WriteFile(source, sourceContent, 0o644); err != nil {
		t.Fatal(err)
	}
	validated, err := compiler.ValidateLLMWiki(compiler.ValidateOptions{ProjectPath: legacyRoot, SourcePath: source, Provider: compiler.MockProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(validated.Files) != 3 || validated.ReviewCount != 1 {
		t.Fatalf("legacy validation=%+v", validated)
	}
	entityPath := "wiki/entities/legacy-release-entity.md"
	entity, err := os.ReadFile(filepath.Join(legacyRoot, filepath.FromSlash(entityPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(legacyRoot, entityPath, append(entity, []byte("\nImported historical note.\n")...), "legacy acceptance history"); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	result, err := importLegacyKnowledgeProject(ctx, legacyKnowledgeImportOptions{
		Config: cfg, Postgres: pg, Agent: agent, SourcePath: legacyRoot, ProjectName: "Imported Legacy", OwnerID: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Sources != 1 || result.WikiPages < 7 {
		t.Fatalf("import result=%+v", result)
	}
	status, err := agent.Status(knowlega.ScopeRef{OrgID: cfg.QM.OrgID, ExternalScopeID: result.ScopeID, Kind: "project", Name: "Imported Legacy"})
	if err != nil || status.State != "ready" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	scope, err := data.NewKnowledgeScopeRepository(pg).Get(ctx, cfg.QM.OrgID, result.ScopeID, "project")
	if err != nil || scope.Status != "ready" || scope.ProjectID != status.ProjectID {
		t.Fatalf("scope=%+v err=%v", scope, err)
	}
	binding, err := knowledgeStore.GetScopeBinding(ctx, "qm", result.ScopeID)
	if err != nil || binding.Status != "ready" || binding.ProjectID != status.ProjectID {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	memberships, err := data.NewProjectFileMembershipRepository(pg).ListByProject(ctx, result.ProjectID)
	if err != nil || len(memberships) != 1 || memberships[0].Status != "ready" || memberships[0].RawPath == nil {
		t.Fatalf("memberships=%+v err=%v", memberships, err)
	}
	artifact, err := data.NewFileArtifactRepository(pg).Get(ctx, memberships[0].FileID)
	if err != nil || artifact == nil || artifact.SHA256 == "" || artifact.BlobKey == nil {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	manifest, err := manifestfile.Load(status.ProjectPath)
	if err != nil || len(manifest.Sources) != 1 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	for _, entry := range manifest.Sources {
		if entry.QMFileID != artifact.ID || entry.QMProjectID != result.ProjectID || entry.QMScopeID != result.ScopeID || entry.QMSourceSHA256 != artifact.SHA256 {
			t.Fatalf("manifest provenance=%+v artifact=%+v", entry, artifact)
		}
	}
	for table, want := range map[string]int{"source_manifest": 1, "sources": 1, "review_items": 1} {
		if got := legacyKnowledgeRowCount(t, knowledgeDB, table, status.ProjectID); got != want {
			t.Fatalf("%s rows=%d want=%d", table, got, want)
		}
	}
	if pages := legacyKnowledgeRowCount(t, knowledgeDB, "wiki_pages", status.ProjectID); pages != int(result.WikiPages) {
		t.Fatalf("wiki pages=%d want=%d", pages, result.WikiPages)
	}
	if versions := legacyKnowledgeRowCount(t, knowledgeDB, "wiki_page_versions", status.ProjectID); versions == 0 {
		t.Fatal("legacy page versions were not imported")
	}

	retried, err := importLegacyKnowledgeProject(ctx, legacyKnowledgeImportOptions{
		Config: cfg, Postgres: pg, Agent: agent, SourcePath: legacyRoot, ProjectName: "Imported Legacy", OwnerID: "alice",
	})
	if err != nil || retried.ProjectID != result.ProjectID || retried.Files != 1 {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	retryMemberships, err := data.NewProjectFileMembershipRepository(pg).ListByProject(ctx, result.ProjectID)
	if err != nil || len(retryMemberships) != 1 || retryMemberships[0].FileID != memberships[0].FileID || retryMemberships[0].Status != "ready" {
		t.Fatalf("retry memberships=%+v err=%v", retryMemberships, err)
	}
	sourceAfter, err := os.ReadFile(source)
	if err != nil || string(sourceAfter) != string(sourceBefore) {
		t.Fatalf("legacy source was mutated: before=%q after=%q err=%v", sourceBefore, sourceAfter, err)
	}
}

func legacyKnowledgeRowCount(t *testing.T, db *sql.DB, table, projectID string) int {
	t.Helper()
	allowed := map[string]bool{"wiki_pages": true, "wiki_page_versions": true, "source_manifest": true, "sources": true, "review_items": true}
	if !allowed[table] {
		t.Fatalf("unsupported knowledge table %q", table)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id=$1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCopyLegacyKnowledgeProjectIsSafeAndIdempotent(t *testing.T) {
	source := filepath.Join(t.TempDir(), "legacy")
	target := filepath.Join(t.TempDir(), "target")
	mustWriteLegacyTestFile(t, filepath.Join(source, "purpose.md"), "legacy purpose\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "schema.md"), "legacy schema\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "wiki", "index.md"), "# Legacy index\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "raw", "sources", "chapter-001.txt"), "chapter one\n")
	if err := manifestfile.Save(source, manifestfile.File{Version: 1, Sources: map[string]manifestfile.Entry{
		"chapter-001.txt": {OriginalPath: "chapter-001.txt", SHA256: "sha", RawPath: "raw/sources/chapter-001.txt", ContentPath: "raw/sources/chapter-001.txt", Title: "Chapter 1", Files: []string{"wiki/index.md"}},
	}}); err != nil {
		t.Fatal(err)
	}
	mustWriteLegacyTestFile(t, filepath.Join(target, "purpose.md"), "empty purpose\n")
	mustWriteLegacyTestFile(t, filepath.Join(target, "schema.md"), "empty schema\n")
	mustWriteLegacyTestFile(t, filepath.Join(target, "wiki", "index.md"), "# Empty\n")
	if err := manifestfile.Save(target, manifestfile.File{}); err != nil {
		t.Fatal(err)
	}

	if err := copyLegacyKnowledgeProject(source, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "wiki", "index.md"))
	if err != nil || string(data) != "# Legacy index\n" {
		t.Fatalf("copied index=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(target, ".kbcore", "qm-legacy-import.json")); err != nil {
		t.Fatal(err)
	}
	rawInfo, err := os.Stat(filepath.Join(target, "raw", "sources", "chapter-001.txt"))
	if err != nil || rawInfo.Mode().Perm()&0o222 != 0 {
		t.Fatalf("imported raw source is writable: info=%v err=%v", rawInfo, err)
	}
	// A retry recognizes the marker and never overlays later target edits.
	mustWriteLegacyTestFile(t, filepath.Join(target, "wiki", "index.md"), "# Edited after import\n")
	if err := copyLegacyKnowledgeProject(source, target); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(target, "wiki", "index.md"))
	if err != nil || string(data) != "# Edited after import\n" {
		t.Fatalf("idempotent index=%q err=%v", data, err)
	}
}

func TestConvergeLegacyKnowledgeProjectUpgradesV1MarkerAndSingularLayout(t *testing.T) {
	source := filepath.Join(t.TempDir(), "legacy")
	target := filepath.Join(t.TempDir(), "target")
	mustWriteLegacyTestFile(t, filepath.Join(source, "purpose.md"), "legacy purpose\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "schema.md"), "legacy schema\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "wiki", "index.md"), "# Legacy index\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "wiki", "entity", "tang-taizong.md"), "---\ntype: entity\ntitle: Tang Taizong\naliases: [唐太宗, 李世民]\nsources: [raw/sources/chapter-001.txt]\n---\n# Tang Taizong\n")
	mustWriteLegacyTestFile(t, filepath.Join(source, "raw", "sources", "chapter-001.txt"), "chapter one\n")
	if err := manifestfile.Save(source, manifestfile.File{Version: 1, Sources: map[string]manifestfile.Entry{
		"chapter-001.txt": {RawPath: "raw/sources/chapter-001.txt", ContentPath: "raw/sources/chapter-001.txt", Files: []string{"wiki/entity/tang-taizong.md"}},
	}}); err != nil {
		t.Fatal(err)
	}
	mustWriteLegacyTestFile(t, filepath.Join(target, "purpose.md"), "empty\n")
	mustWriteLegacyTestFile(t, filepath.Join(target, "schema.md"), "empty\n")
	mustWriteLegacyTestFile(t, filepath.Join(target, "wiki", "index.md"), "# Empty\n")
	if err := manifestfile.Save(target, manifestfile.File{}); err != nil {
		t.Fatal(err)
	}
	if err := copyLegacyKnowledgeProject(source, target); err != nil {
		t.Fatal(err)
	}
	if err := convergeLegacyKnowledgeProject(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "wiki", "entities", "tang-taizong.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "wiki", "entity", "tang-taizong.md")); !os.IsNotExist(err) {
		t.Fatalf("legacy page still exists: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(target, ".kbcore", "qm-legacy-import.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker legacyImportMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Version != 5 || marker.ConvergedAt == "" {
		t.Fatalf("marker=%s", raw)
	}
}

func TestCopyLegacyKnowledgeProjectRejectsNonEmptyTarget(t *testing.T) {
	source := filepath.Join(t.TempDir(), "legacy")
	target := filepath.Join(t.TempDir(), "target")
	for _, item := range []struct{ path, body string }{
		{filepath.Join(source, "purpose.md"), "purpose"},
		{filepath.Join(source, "schema.md"), "schema"},
		{filepath.Join(source, "wiki", "index.md"), "index"},
		{filepath.Join(source, "raw", "source.txt"), "source"},
	} {
		mustWriteLegacyTestFile(t, item.path, item.body)
	}
	if err := manifestfile.Save(source, manifestfile.File{Sources: map[string]manifestfile.Entry{"source": {RawPath: "raw/source.txt"}}}); err != nil {
		t.Fatal(err)
	}
	mustWriteLegacyTestFile(t, filepath.Join(target, "wiki", "human.md"), "do not overwrite")
	if err := copyLegacyKnowledgeProject(source, target); err == nil {
		t.Fatal("expected non-empty target rejection")
	}
}

func mustWriteLegacyTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
