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

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/sourcearchive"
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
	Processed int          `json:"processed"`
	Done      int          `json:"done"`
	Failed    int          `json:"failed"`
	Skipped   int          `json:"skipped"`
	Files     int          `json:"files"`
	Tasks     []IngestTask `json:"tasks"`
}

type ScanSourcesResult struct {
	Queued      int                       `json:"queued"`
	Skipped     int                       `json:"skipped"`
	Tasks       []IngestTask              `json:"tasks"`
	Events      []SourceWatchEvent        `json:"events,omitempty"`
	Unsupported []UnsupportedSourceImport `json:"unsupported,omitempty"`
}

type UnsupportedSourceImport struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type SourceWatchEvent struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	OldSHA256 string `json:"old_sha256,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type sourceWatchFile struct {
	Version int                         `json:"version"`
	Sources map[string]sourceWatchEntry `json:"sources"`
}

type sourceWatchEntry struct {
	SHA256     string `json:"sha256"`
	UpdatedAt  string `json:"updated_at"`
	LastStatus string `json:"last_status"`
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
	result := ScanSourcesResult{}
	watch, err := loadSourceWatch(opts.ProjectPath)
	if err != nil {
		return ScanSourcesResult{}, err
	}
	current := map[string]string{}
	currentStatus := map[string]string{}
	archiveDirs := map[string]bool{}
	archives, err := sourcearchive.Discover(opts.ProjectPath)
	if err != nil {
		return ScanSourcesResult{}, err
	}
	observe := func(abs, relSlash, hash string, supported bool, ext string, force bool, forceReason string) {
		current[relSlash] = hash
		currentStatus[relSlash] = "present"
		previous, seen := watch.Sources[relSlash]
		if seen && previous.LastStatus == "ignored" && previous.SHA256 == hash {
			result.Skipped++
			return
		}
		if !supported {
			currentStatus[relSlash] = "unsupported"
			reason := "unsupported extension " + ext
			result.Unsupported = append(result.Unsupported, UnsupportedSourceImport{Path: relSlash, Reason: reason})
			if !seen || previous.SHA256 != hash || previous.LastStatus != "unsupported" {
				result.Events = append(result.Events, SourceWatchEvent{Path: relSlash, Kind: "unsupported", SHA256: hash, Reason: reason})
			}
			return
		}
		if force {
			paths = append(paths, abs)
			result.Events = append(result.Events, SourceWatchEvent{Path: relSlash, Kind: "changed", OldSHA256: previous.SHA256, SHA256: hash, Reason: forceReason})
			return
		}
		if !seen {
			paths = append(paths, abs)
			result.Events = append(result.Events, SourceWatchEvent{Path: relSlash, Kind: "added", SHA256: hash})
		} else if previous.SHA256 != hash || previous.LastStatus == "deleted" {
			paths = append(paths, abs)
			result.Events = append(result.Events, SourceWatchEvent{Path: relSlash, Kind: "changed", OldSHA256: previous.SHA256, SHA256: hash})
		} else {
			result.Skipped++
		}
	}
	for _, archive := range archives {
		archiveDirs[filepath.Clean(archive.AbsDir)] = true
		originalAbs := filepath.Join(opts.ProjectPath, filepath.FromSlash(archive.Metadata.OriginalRawPath))
		ext := strings.ToLower(filepath.Ext(archive.Metadata.OriginalName))
		actualOriginalHash, hashErr := fileSHA256(originalAbs)
		if hashErr != nil {
			return ScanSourcesResult{}, hashErr
		}
		if actualOriginalHash != archive.Metadata.OriginalSHA256 {
			reason := "immutable original SHA256 does not match metadata"
			current[archive.Metadata.OriginalRawPath] = actualOriginalHash
			currentStatus[archive.Metadata.OriginalRawPath] = "invalid"
			result.Unsupported = append(result.Unsupported, UnsupportedSourceImport{Path: archive.Metadata.OriginalRawPath, Reason: reason})
			result.Events = append(result.Events, SourceWatchEvent{Path: archive.Metadata.OriginalRawPath, Kind: "invalid", SHA256: actualOriginalHash, Reason: reason})
			continue
		}
		force := false
		forceReason := ""
		if archive.Metadata.ContentPath != archive.Metadata.OriginalRawPath {
			contentHash, contentErr := fileSHA256(filepath.Join(opts.ProjectPath, filepath.FromSlash(archive.Metadata.ContentPath)))
			if contentErr != nil {
				return ScanSourcesResult{}, contentErr
			}
			if contentHash != archive.Metadata.ContentSHA256 {
				force = true
				forceReason = "derived content SHA256 changed; re-extraction required"
			}
		}
		observe(originalAbs, archive.Metadata.OriginalRawPath, actualOriginalHash, isSupportedIngestSourceExt(ext), ext, force, forceReason)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if archiveDirs[filepath.Clean(path)] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		rel, _ := filepath.Rel(opts.ProjectPath, path)
		relSlash := filepath.ToSlash(rel)
		hash, hashErr := fileSHA256(path)
		if hashErr != nil {
			return hashErr
		}
		observe(path, relSlash, hash, isSupportedIngestSourceExt(ext), ext, false, "")
		return nil
	})
	if err != nil {
		return ScanSourcesResult{}, err
	}
	sort.Strings(paths)
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
	for rel, previous := range watch.Sources {
		if _, ok := current[rel]; !ok && previous.LastStatus != "deleted" {
			result.Events = append(result.Events, SourceWatchEvent{Path: rel, Kind: "deleted", OldSHA256: previous.SHA256})
		}
	}
	next := sourceWatchFile{Version: 1, Sources: map[string]sourceWatchEntry{}}
	now := time.Now().UTC().Format(time.RFC3339)
	for rel, hash := range current {
		status := currentStatus[rel]
		if status == "" {
			status = "present"
		}
		if previous, ok := watch.Sources[rel]; ok && previous.LastStatus == "ignored" && previous.SHA256 == hash {
			status = "ignored"
		}
		next.Sources[rel] = sourceWatchEntry{SHA256: hash, UpdatedAt: now, LastStatus: status}
	}
	for rel, previous := range watch.Sources {
		if _, ok := current[rel]; !ok {
			previous.LastStatus = "deleted"
			previous.UpdatedAt = now
			next.Sources[rel] = previous
		}
	}
	if err := saveSourceWatch(opts.ProjectPath, next); err != nil {
		return ScanSourcesResult{}, err
	}
	return result, nil
}

func ignoreRawSource(projectPath, rawRel string) error {
	rawRel = filepath.ToSlash(strings.TrimSpace(rawRel))
	if rawRel == "" {
		return nil
	}
	if archive, ok, err := sourcearchive.FindBySourcePath(projectPath, rawRel); err != nil {
		return err
	} else if ok {
		rawRel = archive.Metadata.OriginalRawPath
	}
	abs := filepath.Join(projectPath, filepath.FromSlash(rawRel))
	hash, err := fileSHA256(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	watch, err := loadSourceWatch(projectPath)
	if err != nil {
		return err
	}
	watch.Sources[rawRel] = sourceWatchEntry{SHA256: hash, UpdatedAt: time.Now().UTC().Format(time.RFC3339), LastStatus: "ignored"}
	return saveSourceWatch(projectPath, watch)
}

func isSupportedIngestSourceExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".txt", ".md", ".pdf", ".docx":
		return true
	default:
		return false
	}
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

func loadSourceWatch(projectPath string) (sourceWatchFile, error) {
	watch := sourceWatchFile{Version: 1, Sources: map[string]sourceWatchEntry{}}
	data, err := os.ReadFile(sourceWatchPath(projectPath))
	if os.IsNotExist(err) {
		return watch, nil
	}
	if err != nil {
		return sourceWatchFile{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return watch, nil
	}
	if err := json.Unmarshal(data, &watch); err != nil {
		return sourceWatchFile{}, fmt.Errorf("read source watch: %w", err)
	}
	if watch.Sources == nil {
		watch.Sources = map[string]sourceWatchEntry{}
	}
	if watch.Version == 0 {
		watch.Version = 1
	}
	return watch, nil
}

func saveSourceWatch(projectPath string, watch sourceWatchFile) error {
	path := sourceWatchPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	watch.Version = 1
	data, err := json.MarshalIndent(watch, "", "  ")
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

func sourceWatchPath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "source-watch.json")
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
