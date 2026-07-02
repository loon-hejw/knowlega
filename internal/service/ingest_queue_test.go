package service

import (
	"os"
	"path/filepath"
	"testing"
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
