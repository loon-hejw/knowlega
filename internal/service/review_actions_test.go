package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestRunReviewActionCreatePageWritesDraftAndResolves(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	writeReviewFile(t, root, `# Reviews

## [2026-07-02] missing-page | Alpha Concept

- Source: `+"`raw/sources/a.md`"+`
- Status: open

### Affected Pages

- `+"`wiki/sources/a.md`"+`
### Detail

SEARCH: Alpha Concept
Create a durable concept page for Alpha.
`)
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: root, ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := RunReviewAction(ReviewActionOptions{
		ProjectPath: root,
		ProjectID:   "project",
		ReviewID:    items[0].ID,
		Action:      "create-page",
		Context:     context.Background(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Review.Status != "resolved" || result.Review.ResolvedAction != "create-page" {
		t.Fatalf("review=%+v", result.Review)
	}
	if len(result.WrittenPaths) != 1 || result.WrittenPaths[0] != "wiki/concepts/alpha-concept.md" {
		t.Fatalf("written=%+v", result.WrittenPaths)
	}
	page, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(result.WrittenPaths[0])))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "Review Context") {
		t.Fatalf("draft missing review context:\n%s", page)
	}
	manifest, err := manifestfile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if owner := manifest.PageOwners[result.WrittenPaths[0]]; owner.ManagedBy != "review" {
		t.Fatalf("page owner=%+v", owner)
	}
}

func TestSweepReviewItemsResolvesMissingPageWhenPageExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	writeReviewFile(t, root, `# Reviews

## [2026-07-02] missing-page | Alpha Concept

- Status: open

### Affected Pages

- `+"`wiki/concepts/alpha-concept.md`"+`
### Detail

Create a durable concept page for Alpha.
`)
	if err := wiki.WriteVersionedPage(root, "wiki/concepts/alpha-concept.md", []byte(`---
type: "concept"
title: "Alpha Concept"
---

# Alpha Concept
`), "test"); err != nil {
		t.Fatal(err)
	}
	result, err := SweepReviewItems(ReviewSweepOptions{
		ProjectPath: root,
		ProjectID:   "project",
		Context:     context.Background(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RuleResolved != 1 {
		t.Fatalf("result=%+v", result)
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: root, ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "resolved" || items[0].ResolvedAction != "auto-rule" {
		t.Fatalf("items=%+v", items)
	}
}

func writeReviewFile(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, "wiki", "reviews.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
