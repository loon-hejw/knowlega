package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteVersionedPageArchivesPreviousContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	rel := "wiki/concepts/token-validation.md"
	if err := WriteVersionedPage(root, rel, []byte("# v1\n"), "first write"); err != nil {
		t.Fatal(err)
	}
	if versions := listVersions(t, root, rel); len(versions) != 0 {
		t.Fatalf("first write should not archive, got %v", versions)
	}
	if err := WriteVersionedPage(root, rel, []byte("# v2\n"), "second write"); err != nil {
		t.Fatal(err)
	}
	versions := listVersions(t, root, rel)
	if len(versions) != 1 {
		t.Fatalf("expected one archived version, got %v", versions)
	}
	data, err := os.ReadFile(versions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# v1") || !strings.Contains(string(data), "reason: second write") {
		t.Fatalf("archived version missing old content or reason:\n%s", data)
	}
	if err := WriteVersionedPage(root, rel, []byte("# v2\n"), "same content"); err != nil {
		t.Fatal(err)
	}
	if versions := listVersions(t, root, rel); len(versions) != 1 {
		t.Fatalf("same content should not archive again, got %v", versions)
	}
}

func TestScanWikiPageVersionsParsesArchivedPages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	rel := "wiki/concepts/token-validation.md"
	if err := WriteVersionedPage(root, rel, []byte(`---
type: "concept"
title: "Token Validation"
sources:
  - "raw/sources/oauth.md"
---

# Token Validation

Old token validation notes.
`), "first write"); err != nil {
		t.Fatal(err)
	}
	if err := WriteVersionedPage(root, rel, []byte("# New\n"), "second write"); err != nil {
		t.Fatal(err)
	}

	versions, err := ScanWikiPageVersions(ScanOptions{ProjectPath: root, ProjectID: "project-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions=%+v", versions)
	}
	got := versions[0]
	if got.ProjectID != "project-1" || got.Path != rel || got.PageID == "" || got.ID == "" {
		t.Fatalf("version identity=%+v", got)
	}
	if got.Reason != "second write" || got.Frontmatter["title"] != "Token Validation" {
		t.Fatalf("version metadata=%+v", got)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "raw/sources/oauth.md" {
		t.Fatalf("version sources=%+v", got.Sources)
	}
	if !strings.Contains(got.Body, "Old token validation notes") {
		t.Fatalf("version body=%q", got.Body)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected archived_at timestamp")
	}
}

func listVersions(t *testing.T, root, rel string) []string {
	t.Helper()
	dir := filepath.Join(root, ".kbcore", "page-versions", filepath.FromSlash(strings.TrimSuffix(rel, ".md")))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	return out
}
