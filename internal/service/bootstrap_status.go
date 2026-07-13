package service

import (
	"sync"
	"time"
)

type BootstrapStatus struct {
	Status           string     `json:"status"`
	Stage            string     `json:"stage"`
	ProjectPath      string     `json:"project_path"`
	CurrentSource    string     `json:"current_source,omitempty"`
	CurrentSourceNum int        `json:"current_source_num"`
	CompletedSources int        `json:"completed_sources"`
	TotalSources     int        `json:"total_sources"`
	Files            int        `json:"files"`
	Reviews          int        `json:"reviews"`
	Attempt          int        `json:"attempt"`
	Error            string     `json:"error,omitempty"`
	NextRetryAt      *time.Time `json:"next_retry_at,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

type BootstrapTracker struct {
	mu        sync.RWMutex
	status    BootstrapStatus
	completed map[string]bool
}

func NewBootstrapTracker(projectPath string, total int) *BootstrapTracker {
	now := time.Now().UTC()
	return &BootstrapTracker{completed: make(map[string]bool), status: BootstrapStatus{
		Status: "pending", Stage: "startup", ProjectPath: projectPath,
		TotalSources: total, StartedAt: now, UpdatedAt: now,
	}}
}

func (t *BootstrapTracker) Snapshot() BootstrapStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

func (t *BootstrapTracker) Ready() bool { return t.Snapshot().Status == "succeeded" }

func (t *BootstrapTracker) SetStage(status, stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = status
	t.status.Stage = stage
	t.status.Error = ""
	t.status.NextRetryAt = nil
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) SetAttempt(attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Attempt = attempt
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) UpdateProgress(phase, source string, index, total, files, reviews int, progressError string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "running"
	t.status.Stage = phase
	t.status.CurrentSource = source
	t.status.CurrentSourceNum = index
	if total > 0 {
		t.status.TotalSources = total
	}
	if phase == "completed" || phase == "skipped" {
		if !t.completed[source] {
			t.completed[source] = true
			t.status.CompletedSources++
		}
		if phase == "completed" {
			t.status.Files += files
			t.status.Reviews += reviews
		}
	}
	if progressError != "" {
		t.status.Error = progressError
	}
	t.status.NextRetryAt = nil
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) SetRetry(err error, attempt int, next time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "retrying"
	t.status.Stage = "retry_wait"
	t.status.Attempt = attempt
	t.status.Error = err.Error()
	t.status.NextRetryAt = &next
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) Fail(err error) {
	now := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "failed"
	t.status.Stage = "failed"
	t.status.Error = err.Error()
	t.status.NextRetryAt = nil
	t.status.UpdatedAt = now
	t.status.FinishedAt = &now
}

func (t *BootstrapTracker) Succeed(files, reviews int) {
	now := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "succeeded"
	t.status.Stage = "ready"
	t.status.CompletedSources = t.status.TotalSources
	if files > t.status.Files {
		t.status.Files = files
	}
	if reviews > t.status.Reviews {
		t.status.Reviews = reviews
	}
	t.status.Error = ""
	t.status.NextRetryAt = nil
	t.status.UpdatedAt = now
	t.status.FinishedAt = &now
}
