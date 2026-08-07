package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadSourcesWritesUnderRawSourcesAndQueuesSupportedFiles(t *testing.T) {
	root := t.TempDir()
	result, err := UploadSources(UploadSourcesOptions{
		ProjectPath: root,
		TargetDir:   "imports",
		Queue:       true,
		Files: []UploadSourceInput{
			{RelativePath: "notes/alpha.md", Reader: strings.NewReader("# Alpha\n")},
			{RelativePath: "assets/image.png", Reader: strings.NewReader("png")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Uploaded) != 2 || result.Queued != 1 {
		t.Fatalf("unexpected result=%+v", result)
	}
	if !strings.HasPrefix(result.Uploaded[0].Path, "raw/sources/imports/notes/alpha-") || !strings.HasSuffix(result.Uploaded[0].Path, "/original/alpha.md") {
		t.Fatalf("uploaded path=%q", result.Uploaded[0].Path)
	}
	if len(result.Unsupported) != 1 || !strings.HasSuffix(result.Unsupported[0].Path, "/original/image.png") {
		t.Fatalf("unsupported=%+v", result.Unsupported)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(result.Uploaded[0].OriginalPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "# Alpha\n" {
		t.Fatalf("written=%q", string(written))
	}
	queue, err := LoadIngestQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tasks) != 1 || !strings.HasSuffix(queue.Tasks[0].SourcePath, "/original/alpha.md") {
		t.Fatalf("queue=%+v", queue.Tasks)
	}
}

func TestUploadSourcesRejectsPathEscapes(t *testing.T) {
	root := t.TempDir()
	cases := []UploadSourcesOptions{
		{
			ProjectPath: root,
			TargetDir:   "../outside",
			Files:       []UploadSourceInput{{RelativePath: "alpha.md", Reader: strings.NewReader("alpha")}},
		},
		{
			ProjectPath: root,
			Files:       []UploadSourceInput{{RelativePath: "../alpha.md", Reader: strings.NewReader("alpha")}},
		},
		{
			ProjectPath: root,
			Files:       []UploadSourceInput{{RelativePath: "/tmp/alpha.md", Reader: strings.NewReader("alpha")}},
		},
	}
	for _, tc := range cases {
		if _, err := UploadSources(tc); err == nil {
			t.Fatalf("expected path escape rejection for %+v", tc)
		}
	}
}
