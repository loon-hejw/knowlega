package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type IngestTaskStatus string

const (
	IngestTaskPending    IngestTaskStatus = "pending"
	IngestTaskProcessing IngestTaskStatus = "processing"
	IngestTaskDone       IngestTaskStatus = "done"
	IngestTaskFailed     IngestTaskStatus = "failed"
)

type IngestTask struct {
	ID         string           `json:"id"`
	SourcePath string           `json:"source_path"`
	Title      string           `json:"title,omitempty"`
	Status     IngestTaskStatus `json:"status"`
	SHA256     string           `json:"sha256,omitempty"`
	Files      []string         `json:"files,omitempty"`
	RawPath    string           `json:"raw_path,omitempty"`
	Error      string           `json:"error,omitempty"`
	RetryCount int              `json:"retry_count"`
	AddedAt    string           `json:"added_at"`
	UpdatedAt  string           `json:"updated_at"`
}

type IngestQueue struct {
	Version int          `json:"version"`
	Tasks   []IngestTask `json:"tasks"`
}

type QueueIngestOptions struct {
	ProjectPath string
	SourcePath  string
	Title       string
}

type RunIngestQueueOptions struct {
	ProjectPath   string
	Validator     IngestQueueValidator
	SkipUnchanged bool
	MaxTasks      int
	RetryFailed   bool
	KeepDone      bool
}

type QueueValidateOptions struct {
	ProjectPath   string
	SourcePath    string
	Title         string
	SkipUnchanged bool
}

type QueueValidateResult struct {
	RawPath string
	Files   []string
	Skipped bool
	SHA256  string
}

type IngestQueueValidator func(QueueValidateOptions) (QueueValidateResult, error)

type RunIngestQueueResult struct {
	Processed int
	Done      int
	Failed    int
	Skipped   int
	Files     int
	Tasks     []IngestTask
}

type ScanSourcesResult struct {
	Queued  int
	Skipped int
	Tasks   []IngestTask
}

func QueueIngestSource(opts QueueIngestOptions) (IngestTask, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return IngestTask{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.SourcePath) == "" {
		return IngestTask{}, fmt.Errorf("source path is required")
	}
	sourceAbs, err := filepath.Abs(opts.SourcePath)
	if err != nil {
		return IngestTask{}, err
	}
	hash, err := fileSHA256(sourceAbs)
	if err != nil {
		return IngestTask{}, err
	}
	queue, err := LoadIngestQueue(opts.ProjectPath)
	if err != nil {
		return IngestTask{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := core.StableID("ingest-task", sourceAbs)
	for i := range queue.Tasks {
		task := &queue.Tasks[i]
		if task.ID == id || sameSourcePath(task.SourcePath, sourceAbs) {
			if task.Status == IngestTaskProcessing {
				return *task, nil
			}
			task.SourcePath = filepath.ToSlash(sourceAbs)
			task.Title = opts.Title
			task.SHA256 = hash
			task.Status = IngestTaskPending
			task.Error = ""
			task.Files = nil
			task.RawPath = ""
			task.UpdatedAt = now
			if task.AddedAt == "" {
				task.AddedAt = now
			}
			if err := SaveIngestQueue(opts.ProjectPath, queue); err != nil {
				return IngestTask{}, err
			}
			return *task, nil
		}
	}
	task := IngestTask{
		ID:         id,
		SourcePath: filepath.ToSlash(sourceAbs),
		Title:      opts.Title,
		Status:     IngestTaskPending,
		SHA256:     hash,
		AddedAt:    now,
		UpdatedAt:  now,
	}
	queue.Tasks = append(queue.Tasks, task)
	if err := SaveIngestQueue(opts.ProjectPath, queue); err != nil {
		return IngestTask{}, err
	}
	return task, nil
}

func ScanRawSources(opts QueueIngestOptions) (ScanSourcesResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return ScanSourcesResult{}, fmt.Errorf("project path is required")
	}
	root := filepath.Join(opts.ProjectPath, "raw", "sources")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return ScanSourcesResult{}, nil
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".txt" || ext == ".md" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return ScanSourcesResult{}, err
	}
	sort.Strings(paths)
	result := ScanSourcesResult{}
	for _, path := range paths {
		task, err := QueueIngestSource(QueueIngestOptions{
			ProjectPath: opts.ProjectPath,
			SourcePath:  path,
		})
		if err != nil {
			return ScanSourcesResult{}, err
		}
		if task.Status == IngestTaskPending {
			result.Queued++
			result.Tasks = append(result.Tasks, task)
		} else {
			result.Skipped++
		}
	}
	return result, nil
}

func RunIngestQueue(opts RunIngestQueueOptions) (RunIngestQueueResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return RunIngestQueueResult{}, fmt.Errorf("project path is required")
	}
	if opts.Validator == nil {
		return RunIngestQueueResult{}, fmt.Errorf("ingest queue validator is required")
	}
	queue, err := LoadIngestQueue(opts.ProjectPath)
	if err != nil {
		return RunIngestQueueResult{}, err
	}
	result := RunIngestQueueResult{}
	limit := opts.MaxTasks
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	for i := range queue.Tasks {
		task := &queue.Tasks[i]
		if limit > 0 && result.Processed >= limit {
			break
		}
		if task.Status != IngestTaskPending && !(opts.RetryFailed && task.Status == IngestTaskFailed) {
			continue
		}
		task.Status = IngestTaskProcessing
		task.Error = ""
		task.UpdatedAt = now()
		if err := SaveIngestQueue(opts.ProjectPath, queue); err != nil {
			return RunIngestQueueResult{}, err
		}
		validateResult, err := opts.Validator(QueueValidateOptions{
			ProjectPath:   opts.ProjectPath,
			SourcePath:    filepath.FromSlash(task.SourcePath),
			Title:         task.Title,
			SkipUnchanged: opts.SkipUnchanged,
		})
		result.Processed++
		task.UpdatedAt = now()
		if err != nil {
			task.Status = IngestTaskFailed
			task.Error = err.Error()
			task.RetryCount++
			result.Failed++
			continue
		}
		task.RawPath = validateResult.RawPath
		task.Files = append([]string(nil), validateResult.Files...)
		task.SHA256 = validateResult.SHA256
		if validateResult.Skipped {
			result.Skipped++
		}
		task.Status = IngestTaskDone
		result.Done++
		result.Files += len(validateResult.Files)
	}
	if !opts.KeepDone {
		queue.Tasks = pruneDoneTasks(queue.Tasks)
	}
	if err := SaveIngestQueue(opts.ProjectPath, queue); err != nil {
		return RunIngestQueueResult{}, err
	}
	result.Tasks = append([]IngestTask(nil), queue.Tasks...)
	return result, nil
}

func LoadIngestQueue(projectPath string) (IngestQueue, error) {
	queue := IngestQueue{Version: 1}
	data, err := os.ReadFile(ingestQueuePath(projectPath))
	if os.IsNotExist(err) {
		return queue, nil
	}
	if err != nil {
		return IngestQueue{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return queue, nil
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		return IngestQueue{}, fmt.Errorf("read ingest queue: %w", err)
	}
	if queue.Version == 0 {
		queue.Version = 1
	}
	return queue, nil
}

func SaveIngestQueue(projectPath string, queue IngestQueue) error {
	queue.Version = 1
	path := ingestQueuePath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ingestQueuePath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "ingest-queue.json")
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sameSourcePath(a, b string) bool {
	absA, errA := filepath.Abs(filepath.FromSlash(a))
	absB, errB := filepath.Abs(filepath.FromSlash(b))
	if errA == nil && errB == nil {
		return absA == absB
	}
	return filepath.ToSlash(a) == filepath.ToSlash(b)
}

func pruneDoneTasks(tasks []IngestTask) []IngestTask {
	out := tasks[:0]
	for _, task := range tasks {
		if task.Status == IngestTaskDone {
			continue
		}
		out = append(out, task)
	}
	return out
}
