package service

import (
	"sort"
	"sync"
	"time"
)

type BootstrapStatus struct {
	Status                string                  `json:"status"`
	Stage                 string                  `json:"stage"`
	ProjectPath           string                  `json:"project_path"`
	CurrentSource         string                  `json:"current_source,omitempty"`
	CurrentSourceNum      int                     `json:"current_source_num"`
	CompletedSources      int                     `json:"completed_sources"`
	RestoredSources       int                     `json:"restored_sources"`
	CompletedThisRun      int                     `json:"completed_this_run"`
	PendingSources        int                     `json:"pending_sources"`
	ActiveSources         []BootstrapActiveSource `json:"active_sources,omitempty"`
	RequeuedSources       int                     `json:"requeued_sources"`
	ConflictRequeues      int                     `json:"conflict_requeues"`
	FailureRequeues       int                     `json:"failure_requeues"`
	ImpactRequeues        int                     `json:"impact_requeues"`
	RequeueRate           float64                 `json:"requeue_rate"`
	ConflictRate          float64                 `json:"conflict_rate"`
	ImpactChecks          int                     `json:"impact_checks"`
	QueuedConflictSources int                     `json:"queued_conflict_sources"`
	LLMCalls              int                     `json:"llm_calls"`
	LLMInFlight           int                     `json:"llm_in_flight"`
	OldestLLMCallMS       int64                   `json:"oldest_llm_call_ms"`
	LLMFailures           int                     `json:"llm_failures"`
	LLMDurationMS         int64                   `json:"llm_duration_ms"`
	AverageSourceMS       int64                   `json:"average_source_duration_ms"`
	MaxSourceMS           int64                   `json:"max_source_duration_ms"`
	SourceDurations       map[string]int64        `json:"source_durations_ms,omitempty"`
	TotalSources          int                     `json:"total_sources"`
	Files                 int                     `json:"files"`
	Reviews               int                     `json:"reviews"`
	Attempt               int                     `json:"attempt"`
	Error                 string                  `json:"error,omitempty"`
	NextRetryAt           *time.Time              `json:"next_retry_at,omitempty"`
	StartedAt             time.Time               `json:"started_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
	FinishedAt            *time.Time              `json:"finished_at,omitempty"`
}

type BootstrapActiveSource struct {
	Path    string `json:"path"`
	Number  int    `json:"number"`
	Phase   string `json:"phase"`
	Attempt int    `json:"attempt"`
}

type BootstrapTracker struct {
	mu              sync.RWMutex
	status          BootstrapStatus
	completed       map[string]bool
	active          map[string]BootstrapActiveSource
	fileCounts      map[string]int
	reviewCounts    map[string]int
	sourceTimes     map[string]time.Duration
	queuedConflicts map[string]bool
	llmInFlight     map[string]time.Time
}

func NewBootstrapTracker(projectPath string, total int) *BootstrapTracker {
	now := time.Now().UTC()
	return &BootstrapTracker{completed: make(map[string]bool), active: make(map[string]BootstrapActiveSource), fileCounts: make(map[string]int), reviewCounts: make(map[string]int), sourceTimes: make(map[string]time.Duration), queuedConflicts: make(map[string]bool), llmInFlight: make(map[string]time.Time), status: BootstrapStatus{
		Status: "pending", Stage: "startup", ProjectPath: projectPath,
		TotalSources: total, PendingSources: total, StartedAt: now, UpdatedAt: now,
	}}
}

func (t *BootstrapTracker) SeedCompleted(source string, files, reviews int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if source == "" || t.completed[source] {
		return
	}
	t.completed[source] = true
	t.fileCounts[source], t.reviewCounts[source] = files, reviews
	t.status.CompletedSources++
	t.status.RestoredSources++
	t.status.Files += files
	t.status.Reviews += reviews
	t.status.PendingSources = t.status.TotalSources - t.status.CompletedSources
	if t.status.PendingSources < 0 {
		t.status.PendingSources = 0
	}
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) SeedQueuedConflict(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if source != "" {
		t.queuedConflicts[source] = true
		t.status.QueuedConflictSources = len(t.queuedConflicts)
	}
}

func (t *BootstrapTracker) Snapshot() BootstrapStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	status := t.status
	if status.TotalSources > 0 {
		status.RequeueRate = float64(status.RequeuedSources) / float64(status.TotalSources)
	}
	outcomes := status.CompletedSources + status.RequeuedSources
	if outcomes > 0 {
		status.ConflictRate = float64(status.ConflictRequeues) / float64(outcomes)
	}
	var total time.Duration
	var max time.Duration
	status.SourceDurations = make(map[string]int64, len(t.sourceTimes))
	for source, duration := range t.sourceTimes {
		total += duration
		status.SourceDurations[source] = duration.Milliseconds()
		if duration > max {
			max = duration
		}
	}
	if len(t.sourceTimes) > 0 {
		status.AverageSourceMS = (total / time.Duration(len(t.sourceTimes))).Milliseconds()
		status.MaxSourceMS = max.Milliseconds()
	}
	status.ActiveSources = append([]BootstrapActiveSource(nil), t.status.ActiveSources...)
	status.LLMInFlight = len(t.llmInFlight)
	status.OldestLLMCallMS = 0
	for _, started := range t.llmInFlight {
		if age := time.Since(started).Milliseconds(); age > status.OldestLLMCallMS {
			status.OldestLLMCallMS = age
		}
	}
	return status
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
	t.UpdateTaskProgress(phase, source, index, total, files, reviews, 0, progressError)
}

func (t *BootstrapTracker) UpdateTaskProgress(phase, source string, index, total, files, reviews, attempt int, progressError string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "running"
	t.status.Stage = phase
	t.status.CurrentSource = source
	t.status.CurrentSourceNum = index
	if total > 0 {
		t.status.TotalSources = total
	}
	switch phase {
	case "completed", "skipped":
		delete(t.queuedConflicts, source)
		delete(t.active, source)
		if !t.completed[source] {
			t.completed[source] = true
			t.status.CompletedSources++
			if phase == "completed" {
				t.status.CompletedThisRun++
				t.fileCounts[source] = files
				t.reviewCounts[source] = reviews
				t.status.Files += files
				t.status.Reviews += reviews
			}
		}
	case "impact_requeued":
		delete(t.active, source)
		if t.completed[source] {
			delete(t.completed, source)
			t.status.Files -= t.fileCounts[source]
			t.status.Reviews -= t.reviewCounts[source]
			delete(t.fileCounts, source)
			delete(t.reviewCounts, source)
			if t.status.CompletedSources > 0 {
				t.status.CompletedSources--
			}
		}
		t.status.RequeuedSources++
		t.status.ImpactRequeues++
	case "conflict_requeued":
		delete(t.active, source)
		t.queuedConflicts[source] = true
		t.status.RequeuedSources++
		t.status.ConflictRequeues++
	case "failed_conflict_requeued", "transient_conflict_requeued":
		delete(t.active, source)
		t.queuedConflicts[source] = true
		t.status.RequeuedSources++
		t.status.FailureRequeues++
	case "failed_requeued", "transient_requeued":
		delete(t.active, source)
		t.status.RequeuedSources++
		t.status.FailureRequeues++
	case "impact_review":
		t.status.ImpactChecks++
	default:
		if source != "" {
			delete(t.queuedConflicts, source)
			t.active[source] = BootstrapActiveSource{Path: source, Number: index, Phase: phase, Attempt: attempt}
		}
	}
	t.status.ActiveSources = t.status.ActiveSources[:0]
	t.status.QueuedConflictSources = len(t.queuedConflicts)
	for _, active := range t.active {
		t.status.ActiveSources = append(t.status.ActiveSources, active)
	}
	sort.Slice(t.status.ActiveSources, func(i, j int) bool { return t.status.ActiveSources[i].Number < t.status.ActiveSources[j].Number })
	t.status.PendingSources = t.status.TotalSources - t.status.CompletedSources - len(t.active)
	if t.status.PendingSources < 0 {
		t.status.PendingSources = 0
	}
	if progressError != "" {
		t.status.Error = progressError
	} else if phase != "impact_review" {
		t.status.Error = ""
	}
	t.status.NextRetryAt = nil
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) RecordLLMCall(duration time.Duration, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.LLMCalls++
	t.status.LLMDurationMS += duration.Milliseconds()
	if failed {
		t.status.LLMFailures++
	}
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) StartLLMCall(id string, startedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.llmInFlight[id] = startedAt
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) FinishLLMCall(id string, duration time.Duration, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.llmInFlight, id)
	t.status.LLMCalls++
	t.status.LLMDurationMS += duration.Milliseconds()
	if failed {
		t.status.LLMFailures++
	}
	t.status.UpdatedAt = time.Now().UTC()
}

func (t *BootstrapTracker) RecordSourceDuration(source string, duration time.Duration) {
	if source == "" || duration <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sourceTimes[source] += duration
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
	t.active = make(map[string]BootstrapActiveSource)
	t.status.ActiveSources = nil
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
	t.active = make(map[string]BootstrapActiveSource)
	t.status.ActiveSources = nil
}

func (t *BootstrapTracker) Succeed(files, reviews int) {
	now := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Status = "succeeded"
	t.status.Stage = "ready"
	t.status.CompletedSources = t.status.TotalSources
	t.status.PendingSources = 0
	t.status.ActiveSources = nil
	t.active = make(map[string]BootstrapActiveSource)
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
