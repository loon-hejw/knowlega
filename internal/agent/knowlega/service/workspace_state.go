package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/sourcearchive"
)

const (
	KnowledgeWorkspaceEmpty      = "empty"
	KnowledgeWorkspaceQueued     = "queued"
	KnowledgeWorkspaceProcessing = "processing"
	KnowledgeWorkspaceReady      = "ready"
	KnowledgeWorkspaceFailed     = "failed"
)

// KnowledgeWorkspaceState is the durable, filesystem-derived processing state
// for one Knowledge scope. Markdown, the source manifest, and the ingest queue
// remain authoritative; callers do not need a separate PostgreSQL status row.
type KnowledgeWorkspaceState struct {
	Status           string               `json:"status"`
	SourceCount      int64                `json:"source_count"`
	WikiPageCount    int64                `json:"wiki_page_count"`
	Queue            WorkspaceQueueStatus `json:"queue"`
	LastError        string               `json:"last_error,omitempty"`
	LastSuccessfulAt *time.Time           `json:"last_successful_at,omitempty"`
}

type knowledgeMaintenanceState struct {
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	UpdatedAt        string `json:"updated_at"`
	LastSuccessfulAt string `json:"last_successful_at,omitempty"`
	OverviewPending  bool   `json:"overview_pending,omitempty"`
}

// InspectKnowledgeWorkspace derives scope readiness from the current raw/wiki
// artifacts and queue. A freshly initialized scope is empty, not ready.
func InspectKnowledgeWorkspace(projectPath string) (KnowledgeWorkspaceState, error) {
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return KnowledgeWorkspaceState{}, fmt.Errorf("project path is required")
	}

	queue, err := LoadIngestQueue(projectPath)
	if err != nil {
		return KnowledgeWorkspaceState{}, err
	}
	state := KnowledgeWorkspaceState{Queue: queueStatus(queue)}
	maintenance, maintenanceFound, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return KnowledgeWorkspaceState{}, err
	}

	manifest, err := manifestfile.Load(projectPath)
	if err != nil {
		return KnowledgeWorkspaceState{}, err
	}
	manifestPaths := map[string]bool{}
	for key, entry := range manifest.Sources {
		for _, value := range []string{key, entry.OriginalPath, entry.RawPath, entry.OriginalRawPath, entry.ContentPath} {
			for _, normalized := range workspaceSourceKeys(projectPath, value) {
				manifestPaths[normalized] = true
			}
		}
		state.LastSuccessfulAt = laterWorkspaceTime(state.LastSuccessfulAt, entry.UpdatedAt)
	}

	queuedSources := map[string]bool{}
	var latestFailure time.Time
	for _, task := range queue.Tasks {
		keys := workspaceSourceKeys(projectPath, task.SourcePath)
		known := false
		for _, key := range keys {
			if manifestPaths[key] {
				known = true
				break
			}
		}
		if !known {
			key := strings.TrimSpace(task.ID)
			if len(keys) > 0 {
				key = keys[0]
			}
			if key != "" {
				queuedSources[key] = true
			}
		}
		if task.Status == IngestTaskDone {
			state.LastSuccessfulAt = laterWorkspaceTime(state.LastSuccessfulAt, task.UpdatedAt)
		}
		if task.Status == IngestTaskFailed && strings.TrimSpace(task.Error) != "" {
			updated, parsed := parseWorkspaceTime(task.UpdatedAt)
			if !parsed || latestFailure.IsZero() || updated.After(latestFailure) {
				state.LastError = task.Error
				if parsed {
					latestFailure = updated
				}
			}
		}
	}
	if maintenanceFound {
		state.LastSuccessfulAt = laterWorkspaceTime(state.LastSuccessfulAt, maintenance.LastSuccessfulAt)
		if maintenance.Status == KnowledgeWorkspaceFailed && strings.TrimSpace(maintenance.Error) != "" {
			updated, parsed := parseWorkspaceTime(maintenance.UpdatedAt)
			if !parsed || latestFailure.IsZero() || updated.After(latestFailure) {
				state.LastError = maintenance.Error
				if parsed {
					latestFailure = updated
				}
			}
		}
	}

	state.SourceCount = int64(len(manifest.Sources) + len(queuedSources))
	rawCount, err := countLogicalRawSources(projectPath)
	if err != nil {
		return KnowledgeWorkspaceState{}, err
	}
	// Before the first scan there may be raw files but no queue or manifest.
	// Once a manifest exists it is the logical source count and orphan mirrors
	// must not inflate it.
	if len(manifest.Sources) == 0 && rawCount > state.SourceCount {
		state.SourceCount = rawCount
	}
	state.WikiPageCount, err = countWorkspaceMarkdown(filepath.Join(projectPath, "wiki"))
	if err != nil {
		return KnowledgeWorkspaceState{}, err
	}

	switch {
	case state.Queue.Processing > 0 || maintenance.Status == KnowledgeWorkspaceProcessing:
		state.Status = KnowledgeWorkspaceProcessing
	case state.Queue.Failed > 0:
		state.Status = KnowledgeWorkspaceFailed
	case state.Queue.Pending > 0:
		state.Status = KnowledgeWorkspaceQueued
	case maintenance.Status == KnowledgeWorkspaceFailed:
		state.Status = KnowledgeWorkspaceFailed
	case len(manifest.Sources) > 0:
		state.Status = KnowledgeWorkspaceReady
	case rawCount > 0:
		state.Status = KnowledgeWorkspaceQueued
	default:
		state.Status = KnowledgeWorkspaceEmpty
	}
	return state, nil
}

func beginKnowledgeMaintenance(projectPath string) error {
	current, _, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return err
	}
	current.Status = KnowledgeWorkspaceProcessing
	current.Error = ""
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return saveKnowledgeMaintenanceState(projectPath, current)
}

func finishKnowledgeMaintenance(projectPath string, success bool, message string) error {
	current, _, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	current.UpdatedAt = now
	if success {
		current.Status = KnowledgeWorkspaceReady
		current.Error = ""
		current.LastSuccessfulAt = now
	} else {
		current.Status = KnowledgeWorkspaceFailed
		current.Error = strings.TrimSpace(message)
		if current.Error == "" {
			current.Error = "knowledge maintenance failed"
		}
	}
	return saveKnowledgeMaintenanceState(projectPath, current)
}

func RecoverInterruptedKnowledgeMaintenance(projectPath string) (bool, error) {
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return false, fmt.Errorf("project path is required")
	}
	current, found, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return false, err
	}
	if !found || current.Status != KnowledgeWorkspaceProcessing {
		return false, nil
	}
	current.Status = KnowledgeWorkspaceReady
	current.Error = ""
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return true, saveKnowledgeMaintenanceState(projectPath, current)
}

func knowledgeOverviewRefreshPending(projectPath string) (bool, error) {
	current, _, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return false, err
	}
	return current.OverviewPending, nil
}

func setKnowledgeOverviewRefreshPending(projectPath string, pending bool) error {
	current, _, err := loadKnowledgeMaintenanceState(projectPath)
	if err != nil {
		return err
	}
	current.OverviewPending = pending
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return saveKnowledgeMaintenanceState(projectPath, current)
}

func loadKnowledgeMaintenanceState(projectPath string) (knowledgeMaintenanceState, bool, error) {
	path := filepath.Join(projectPath, ".kbcore", "maintenance-status.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return knowledgeMaintenanceState{}, false, nil
	}
	if err != nil {
		return knowledgeMaintenanceState{}, false, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return knowledgeMaintenanceState{}, false, nil
	}
	var state knowledgeMaintenanceState
	if err := json.Unmarshal(data, &state); err != nil {
		return knowledgeMaintenanceState{}, false, fmt.Errorf("read maintenance status: %w", err)
	}
	return state, true, nil
}

func saveKnowledgeMaintenanceState(projectPath string, state knowledgeMaintenanceState) error {
	path := filepath.Join(projectPath, ".kbcore", "maintenance-status.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func workspaceSourceKeys(projectPath, value string) []string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(candidate string) {
		candidate = strings.ToLower(filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(candidate), "./")))
		if candidate != "" {
			seen[candidate] = true
		}
	}
	add(value)
	if filepath.IsAbs(filepath.FromSlash(value)) {
		if rel, err := filepath.Rel(projectPath, filepath.FromSlash(value)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			add(rel)
		}
	} else if abs, err := filepath.Abs(filepath.Join(projectPath, filepath.FromSlash(value))); err == nil {
		add(abs)
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	return keys
}

func countLogicalRawSources(projectPath string) (int64, error) {
	root := filepath.Join(projectPath, "raw", "sources")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	archives, err := sourcearchive.Discover(projectPath)
	if err != nil {
		return 0, err
	}
	archiveDirs := map[string]bool{}
	for _, archive := range archives {
		archiveDirs[filepath.Clean(archive.AbsDir)] = true
	}
	count := int64(len(archives))
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && archiveDirs[filepath.Clean(path)] {
				return filepath.SkipDir
			}
			return nil
		}
		if isSupportedIngestSourceExt(strings.ToLower(filepath.Ext(path))) {
			count++
		}
		return nil
	})
	return count, err
}

func countWorkspaceMarkdown(root string) (int64, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var count int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			count++
		}
		return nil
	})
	return count, err
}

func laterWorkspaceTime(current *time.Time, raw string) *time.Time {
	parsed, ok := parseWorkspaceTime(raw)
	if !ok || current != nil && !parsed.After(*current) {
		return current
	}
	value := parsed.UTC()
	return &value
}

func parseWorkspaceTime(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
