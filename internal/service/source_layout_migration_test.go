package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestSourceLayoutMigrationDryRunAndApplyWithoutLLMReprocessing(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "migration"}); err != nil {
		t.Fatal(err)
	}
	oldRaw := "raw/sources/017b70af1737-alpha.md"
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(oldRaw)), []byte("# Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	page := wiki.RenderPage(wiki.Page{Title: "Alpha", Type: "source-summary", Sources: []string{oldRaw}, Body: "# Alpha\n"})
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "alpha.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := sourceManifestFile{Version: 1, Sources: map[string]sourceManifestFileEntry{
		"/external/alpha.md": {
			OriginalPath: "/external/alpha.md", PipelineVersion: core.SourceManifestPipelineVersion,
			SHA256: "017b70af1737a855266fc9eb9b3f88917dc972f20d2a1c7d6baa03b85260e8b8", RawPath: oldRaw,
			Title: "Alpha", Files: []string{"wiki/sources/alpha.md"}, Extraction: json.RawMessage(`{"source_ext":".md","extractor":"direct"}`),
		},
	}}
	if err := saveSourceManifestFile(root, manifest); err != nil {
		t.Fatal(err)
	}

	dryRun, err := PlanSourceLayoutMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun.DryRun || dryRun.Total != 1 || dryRun.Items[0].Stage != "planned" {
		t.Fatalf("dry run=%+v", dryRun)
	}
	if _, err := os.Stat(filepath.Join(root, dryRun.Items[0].TargetArchivePath)); !os.IsNotExist(err) {
		t.Fatalf("dry run changed filesystem: %v", err)
	}

	result, err := MigrateSourceLayout(SourceLayoutMigrationOptions{ProjectPath: root, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || result.Completed != 1 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(oldRaw))); !os.IsNotExist(err) {
		t.Fatalf("legacy source still exists: %v", err)
	}
	metadata, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(result.Items[0].TargetArchivePath), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(metadata) {
		t.Fatalf("invalid metadata: %s", metadata)
	}
	updatedManifest, err := loadSourceManifestFile(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := updatedManifest.Sources["/external/alpha.md"]
	if entry.PipelineVersion != core.SourceManifestPipelineVersion || entry.ArchivePath == "" || entry.RawPath == oldRaw {
		t.Fatalf("manifest entry=%+v", entry)
	}
	if !strings.Contains(string(entry.Extraction), `"extractor"`) || !strings.Contains(string(entry.Extraction), `"direct"`) {
		t.Fatalf("extraction metadata was lost: %s", entry.Extraction)
	}
	updatedPage, err := os.ReadFile(filepath.Join(root, "wiki", "sources", "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updatedPage), oldRaw) || !strings.Contains(string(updatedPage), entry.RawPath) {
		t.Fatalf("wiki source reference not migrated:\n%s", updatedPage)
	}
}
