package service

import (
	"os"
	"path/filepath"
	"testing"

	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestWriteProjectFileContentArchivesWikiEdits(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "edit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteProjectFileContent(WriteProjectFileContentOptions{
		ProjectPath: root,
		Path:        "wiki/concepts/alpha.md",
		Content:     "# Alpha\n\nFirst.",
		Reason:      "test create",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteProjectFileContent(WriteProjectFileContentOptions{
		ProjectPath: root,
		Path:        "wiki/concepts/alpha.md",
		Content:     "# Alpha\n\nSecond.",
		Reason:      "test update",
	}); err != nil {
		t.Fatal(err)
	}
	versionRoot := filepath.Join(root, ".kbcore", "page-versions", "wiki", "concepts", "alpha")
	entries, err := os.ReadDir(versionRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("versions=%d", len(entries))
	}
	manifest, err := manifestfile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if owner := manifest.PageOwners["wiki/concepts/alpha.md"]; owner.ManagedBy != "manual" {
		t.Fatalf("page owner=%+v", owner)
	}
}

func TestWriteProjectFileContentRejectsRawSourceWrites(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteProjectFileContent(WriteProjectFileContentOptions{
		ProjectPath: root,
		Path:        "raw/sources/source.md",
		Content:     "mutate",
	}); err == nil {
		t.Fatal("expected raw source write to be rejected")
	}
}
