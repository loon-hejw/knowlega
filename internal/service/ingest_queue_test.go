package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/sourcearchive"
)

func TestRunIngestQueueUsesPersistentTasks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nAlpha links beta."), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := QueueIngestSource(QueueIngestOptions{ProjectPath: root, SourcePath: source, Title: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != IngestTaskPending {
		t.Fatalf("status=%s", task.Status)
	}
	result, err := RunIngestQueue(RunIngestQueueOptions{
		ProjectPath: root,
		Validator: func(opts QueueValidateOptions) (QueueValidateResult, error) {
			if opts.Title != "Alpha" {
				t.Fatalf("title=%q", opts.Title)
			}
			return QueueValidateResult{
				RawPath: "raw/sources/source.md",
				Files:   []string{"wiki/sources/source.md", "wiki/entities/alpha.md"},
				SHA256:  "abc123",
			}, nil
		},
		KeepDone: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 || result.Done != 1 || result.Files != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	queue, err := LoadIngestQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tasks) != 1 || queue.Tasks[0].Status != IngestTaskDone || len(queue.Tasks[0].Files) != 2 {
		t.Fatalf("queue=%+v", queue.Tasks)
	}
}

func TestRunIngestQueuePrunesDoneTasksByDefault(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.md")
	if err := os.WriteFile(source, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := QueueIngestSource(QueueIngestOptions{ProjectPath: root, SourcePath: source}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunIngestQueue(RunIngestQueueOptions{
		ProjectPath: root,
		Validator: func(QueueValidateOptions) (QueueValidateResult, error) {
			return QueueValidateResult{RawPath: "raw/sources/source.md", SHA256: "abc123"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	queue, err := LoadIngestQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tasks) != 0 {
		t.Fatalf("done tasks not pruned: %+v", queue.Tasks)
	}
}

func TestScanRawSourcesQueuesSupportedRichSourcesAndReportsUnsupported(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw", "sources")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.md", "beta.txt", "gamma.pdf", "delta.docx", "skip.bin"} {
		if err := os.WriteFile(filepath.Join(raw, name), []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 4 || len(result.Tasks) != 4 {
		t.Fatalf("queued=%d tasks=%d result=%+v", result.Queued, len(result.Tasks), result)
	}
	if len(result.Unsupported) != 1 || result.Unsupported[0].Path != "raw/sources/skip.bin" {
		t.Fatalf("unsupported=%+v", result.Unsupported)
	}
}

func TestScanRawSourcesReportsDeletedWatchEvent(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw", "sources")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(raw, "alpha.md")
	if err := os.WriteFile(source, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanRawSources(QueueIngestOptions{ProjectPath: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	result, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	var deleted bool
	for _, event := range result.Events {
		if event.Kind == "deleted" && event.Path == "raw/sources/alpha.md" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("events=%+v", result.Events)
	}
}

func TestScanRawSourcesTreatsArchiveAsOneImmutableSource(t *testing.T) {
	root := t.TempDir()
	archive, err := sourcearchive.ImportReader(sourcearchive.ImportOptions{
		ProjectPath: root, Collection: "manuals", RelativePath: "guide.docx", Reader: strings.NewReader("docx-original"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourcearchive.SetExtracted(root, archive.Metadata.OriginalRawPath, []byte("# Guide\n"), "test"); err != nil {
		t.Fatal(err)
	}
	first, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if first.Queued != 1 || len(first.Tasks) != 1 || !strings.HasSuffix(first.Tasks[0].SourcePath, "/original/guide.docx") {
		t.Fatalf("first scan=%+v", first)
	}
	if len(first.Unsupported) != 0 {
		t.Fatalf("archive internals were scanned independently: %+v", first.Unsupported)
	}
	second, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if second.Queued != 0 || second.Skipped == 0 {
		t.Fatalf("unchanged archive was requeued: %+v", second)
	}
}

func TestScanRawSourcesRejectsMutatedArchiveOriginal(t *testing.T) {
	root := t.TempDir()
	archive, err := sourcearchive.ImportReader(sourcearchive.ImportOptions{ProjectPath: root, RelativePath: "alpha.md", Reader: strings.NewReader("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(archive.Metadata.OriginalRawPath)), []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ScanRawSources(QueueIngestOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 0 || len(result.Unsupported) != 1 || !strings.Contains(result.Unsupported[0].Reason, "immutable original SHA256") {
		t.Fatalf("result=%+v", result)
	}
}
