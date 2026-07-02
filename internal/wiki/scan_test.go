package wiki

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseWikiPageReadsFrontmatterSourcesAndAliases(t *testing.T) {
	page := ParseWikiPage("project-1", "wiki/concepts/token-validation.md", `---
type: "concept"
title: "Token Validation"
aliases:
  - "JWT check"
  - "token auth"
sources:
  - "raw/sources/auth.md"
---

# Ignored Heading

Tokens are validated by the auth service.
`)

	if page.ProjectID != "project-1" {
		t.Fatalf("project id=%s", page.ProjectID)
	}
	if page.Path != "wiki/concepts/token-validation.md" {
		t.Fatalf("path=%s", page.Path)
	}
	if page.Type != "concept" || page.Title != "Token Validation" {
		t.Fatalf("unexpected type/title: %+v", page)
	}
	if page.ID == "" {
		t.Fatal("expected stable page id")
	}
	if !strings.Contains(page.Body, "Tokens are validated") {
		t.Fatalf("body not parsed: %q", page.Body)
	}
	if !reflect.DeepEqual(page.Sources, []string{"raw/sources/auth.md"}) {
		t.Fatalf("sources=%+v", page.Sources)
	}
	aliases, ok := page.Frontmatter["aliases"].([]string)
	if !ok {
		t.Fatalf("aliases not parsed as []string: %#v", page.Frontmatter["aliases"])
	}
	if !reflect.DeepEqual(aliases, []string{"JWT check", "token auth"}) {
		t.Fatalf("aliases=%+v", aliases)
	}
}

func TestParseWikiPageFallsBackToHeadingAndFilename(t *testing.T) {
	heading := ParseWikiPage("project-1", "wiki/notes/heading.md", "# Heading Title\n\nBody")
	if heading.Title != "Heading Title" {
		t.Fatalf("heading title=%s", heading.Title)
	}
	if heading.Type != "page" {
		t.Fatalf("default type=%s", heading.Type)
	}

	filename := ParseWikiPage("project-1", "wiki/notes/file-name.md", "Body only")
	if filename.Title != "file-name" {
		t.Fatalf("filename title=%s", filename.Title)
	}
}

func TestScanWikiPagesWalksProjectWiki(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := InitProject(ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "concepts", "routing.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`---
type: "concept"
title: "Routing"
sources: ["raw/sources/routing.md"]
---

# Routing

Routing note.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	pages, err := ScanWikiPages(ScanOptions{ProjectPath: root, ProjectID: "project-1"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, page := range pages {
		if page.Path != "wiki/concepts/routing.md" {
			continue
		}
		found = true
		if page.Title != "Routing" || page.Type != "concept" {
			t.Fatalf("unexpected page: %+v", page)
		}
		if len(page.Sources) != 1 || page.Sources[0] != "raw/sources/routing.md" {
			t.Fatalf("sources=%+v", page.Sources)
		}
		if page.UpdatedAt.IsZero() {
			t.Fatal("expected updated_at from file mod time")
		}
	}
	if !found {
		t.Fatalf("scanned pages did not include routing.md: %+v", pages)
	}
}
