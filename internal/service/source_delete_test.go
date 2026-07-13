package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/sourcearchive"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestDeleteSourceCleansManifestPagesLinksAndReviews(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "raw", "sources", "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := sourceManifestFile{Version: 1, Sources: map[string]sourceManifestFileEntry{
		"/original/a.md": {
			OriginalPath: "/original/a.md",
			RawPath:      "raw/sources/a.md",
			Title:        "Alpha",
			SHA256:       "sha",
			Files:        []string{"wiki/sources/a.md", "wiki/concepts/alpha.md", "wiki/entities/shared.md"},
			UpdatedAt:    "2026-01-01T00:00:00Z",
		},
	}}
	if err := saveSourceManifestFile(root, manifest); err != nil {
		t.Fatal(err)
	}
	mustWriteVersioned(t, root, "wiki/sources/a.md", `---
type: "source-summary"
title: "Alpha Source"
sources:
  - "raw/sources/a.md"
---

# Alpha Source
`)
	mustWriteVersioned(t, root, "wiki/concepts/alpha.md", `---
type: "concept"
title: "Alpha"
sources:
  - "raw/sources/a.md"
---

# Alpha
`)
	mustWriteVersioned(t, root, "wiki/entities/shared.md", `---
type: "entity"
title: "Shared"
sources:
  - "raw/sources/a.md"
  - "raw/sources/b.md"
---

# Shared
`)
	mustWriteVersioned(t, root, "wiki/concepts/ref.md", `---
type: "concept"
title: "Ref"
sources:
  - "raw/sources/b.md"
---

# Ref

See [[alpha]].
`)
	if err := os.WriteFile(filepath.Join(root, "wiki", "index.md"), []byte("- [[a|Alpha]] - `wiki/sources/a.md`\n- [[alpha|Alpha]] - `wiki/concepts/alpha.md`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "reviews.md"), []byte("# Reviews\n\n## [2026-01-01] missing-page | Alpha Review\n\n- Source: `raw/sources/a.md`\n- Status: open\n\n### Affected Pages\n\n- `wiki/concepts/alpha.md`\n\n### Detail\n\nNeeds review.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	preview, err := DeleteSource(DeleteSourceOptions{ProjectPath: root, ProjectID: "project", SourcePath: "raw/sources/a.md", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.DeletedPages) != 2 || len(preview.UpdatedPages) != 1 || len(preview.CleanedPages) != 2 || len(preview.ResolvedReviews) != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "concepts", "alpha.md")); err != nil {
		t.Fatalf("dry-run removed page: %v", err)
	}

	result, err := DeleteSource(DeleteSourceOptions{ProjectPath: root, ProjectID: "project", SourcePath: "raw/sources/a.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ManifestUpdated {
		t.Fatalf("manifest not updated: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "concepts", "alpha.md")); !os.IsNotExist(err) {
		t.Fatalf("expected deleted page, stat err=%v", err)
	}
	shared, err := os.ReadFile(filepath.Join(root, "wiki", "entities", "shared.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shared), "raw/sources/a.md") || !strings.Contains(string(shared), "raw/sources/b.md") {
		t.Fatalf("shared sources not updated:\n%s", shared)
	}
	ref, err := os.ReadFile(filepath.Join(root, "wiki", "concepts", "ref.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ref), "[[alpha]]") {
		t.Fatalf("deleted wikilink not cleaned:\n%s", ref)
	}
	reviews, err := os.ReadFile(filepath.Join(root, "wiki", "reviews.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviews), "- Status: dismissed") || !strings.Contains(string(reviews), "- Resolved action: source-delete") {
		t.Fatalf("review not dismissed:\n%s", reviews)
	}
	if _, err := os.Stat(filepath.Join(root, "raw", "sources", "a.md")); err != nil {
		t.Fatalf("immutable raw source should be retained: %v", err)
	}
	scan, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Queued != 0 {
		t.Fatalf("deleted source was requeued: %+v", scan)
	}
	versions, err := wiki.ScanWikiPageVersions(wiki.ScanOptions{ProjectPath: root, ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("expected deleted page archive")
	}
}

func mustWriteVersioned(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := wiki.WriteVersionedPage(root, rel, []byte(content), "test"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSourceRemovesWholeArchiveWhenRequested(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "archive-delete"}); err != nil {
		t.Fatal(err)
	}
	archive, err := sourcearchive.ImportReader(sourcearchive.ImportOptions{ProjectPath: root, RelativePath: "alpha.md", Reader: strings.NewReader("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	manifest := sourceManifestFile{Version: 1, Sources: map[string]sourceManifestFileEntry{
		"alpha": {
			OriginalPath: "alpha", SHA256: archive.Metadata.OriginalSHA256, RawPath: archive.Metadata.ContentPath,
			ArchivePath: archive.Metadata.ArchivePath, OriginalRawPath: archive.Metadata.OriginalRawPath,
			ContentPath: archive.Metadata.ContentPath, Title: "Alpha",
		},
	}}
	if err := saveSourceManifestFile(root, manifest); err != nil {
		t.Fatal(err)
	}
	result, err := DeleteSource(DeleteSourceOptions{ProjectPath: root, SourcePath: archive.Metadata.ContentPath, DeleteRaw: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DeletedRaw || result.ArchivePath != archive.Metadata.ArchivePath {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(archive.AbsDir); !os.IsNotExist(err) {
		t.Fatalf("archive directory still exists: %v", err)
	}
}
