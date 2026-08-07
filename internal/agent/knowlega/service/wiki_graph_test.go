package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSearchWikiGraphDocumentsUsesSourceOverlapAndLinks(t *testing.T) {
	root := t.TempDir()
	writeTestPage(t, root, "wiki/entities/alpha.md", `---
title: Alpha
type: entity
sources: [raw/sources/a.md]
---
# Alpha

Alpha links to [[Beta]].
`)
	writeTestPage(t, root, "wiki/concepts/beta.md", `---
title: Beta
type: concept
sources: [raw/sources/a.md]
---
# Beta

Beta explains shared source context.
`)
	writeTestPage(t, root, "wiki/concepts/gamma.md", `---
title: Gamma
type: concept
sources: [raw/sources/c.md]
---
# Gamma

Unrelated page.
`)
	docs, err := SearchWikiGraphDocuments(root, "", []string{"wiki/entities/alpha.md"}, 5, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("expected related graph docs")
	}
	if docs[0].Path != "wiki/concepts/beta.md" {
		t.Fatalf("top path=%s docs=%+v", docs[0].Path, docs)
	}
}

func writeTestPage(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
