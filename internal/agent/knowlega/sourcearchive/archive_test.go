package sourcearchive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportReaderCreatesImmutablePerSourceArchive(t *testing.T) {
	root := t.TempDir()
	first, err := ImportReader(ImportOptions{
		ProjectPath:  root,
		Collection:   "imports",
		RelativePath: "notes/Alpha Notes.md",
		Reader:       strings.NewReader("# Alpha\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := "/alpha-notes-017b70af1737"
	if !strings.HasSuffix(first.Metadata.ArchivePath, wantSuffix) {
		t.Fatalf("archive path=%q", first.Metadata.ArchivePath)
	}
	if first.Metadata.ContentPath != first.Metadata.OriginalRawPath || first.Metadata.Extractor != "direct" {
		t.Fatalf("metadata=%+v", first.Metadata)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(first.Metadata.OriginalRawPath)))
	if err != nil || string(data) != "# Alpha\n" {
		t.Fatalf("original data=%q err=%v", string(data), err)
	}

	again, err := ImportReader(ImportOptions{
		ProjectPath:  root,
		Collection:   "imports",
		RelativePath: "notes/Alpha Notes.md",
		Reader:       strings.NewReader("# Alpha\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Metadata.ArchivePath != first.Metadata.ArchivePath {
		t.Fatalf("idempotent import changed path: %s != %s", again.Metadata.ArchivePath, first.Metadata.ArchivePath)
	}

	changed, err := ImportReader(ImportOptions{
		ProjectPath:  root,
		Collection:   "imports",
		RelativePath: "notes/Alpha Notes.md",
		Reader:       strings.NewReader("# Alpha changed\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Metadata.ArchivePath == first.Metadata.ArchivePath {
		t.Fatal("different content reused the same archive")
	}
}

func TestSetExtractedPreservesOriginalAndUpdatesContentPath(t *testing.T) {
	root := t.TempDir()
	archive, err := ImportReader(ImportOptions{
		ProjectPath:  root,
		RelativePath: "manual.docx",
		Reader:       strings.NewReader("docx bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(archive.Metadata.OriginalRawPath)))
	if err != nil {
		t.Fatal(err)
	}
	archive, err = SetExtracted(root, archive.Metadata.OriginalRawPath, []byte("# Extracted\n"), "docx-xml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archive.Metadata.ContentPath, "/extracted.md") || archive.Metadata.Extractor != "docx-xml" {
		t.Fatalf("metadata=%+v", archive.Metadata)
	}
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(archive.Metadata.OriginalRawPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("original source was mutated")
	}
	found, ok, err := FindBySourcePath(root, archive.Metadata.ContentPath)
	if err != nil || !ok || found.Metadata.SourceID != archive.Metadata.SourceID {
		t.Fatalf("find archive: ok=%v archive=%+v err=%v", ok, found, err)
	}
}
