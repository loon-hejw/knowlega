package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type WorkspaceStatusOptions struct {
	ProjectPath         string
	ProjectID           string
	Agent               string
	PGConfigured        bool
	EmbeddingConfigured bool
}

type WorkspaceStatus struct {
	OK                  bool                  `json:"ok"`
	Service             string                `json:"service"`
	Time                string                `json:"time"`
	ProjectPath         string                `json:"project_path"`
	ProjectID           string                `json:"project_id"`
	Agent               string                `json:"agent"`
	Files               []WorkspaceFileStatus `json:"files"`
	Queue               WorkspaceQueueStatus  `json:"queue"`
	Reviews             WorkspaceReviewStatus `json:"reviews"`
	Lint                WorkspaceLintStatus   `json:"lint"`
	PGConfigured        bool                  `json:"pg_configured"`
	EmbeddingConfigured bool                  `json:"embedding_configured"`
	Bootstrap           *BootstrapStatus      `json:"bootstrap,omitempty"`
}

type WorkspaceFileStatus struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type WorkspaceQueueStatus struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Total      int `json:"total"`
}

type WorkspaceReviewStatus struct {
	Open      int `json:"open"`
	Resolved  int `json:"resolved"`
	Dismissed int `json:"dismissed"`
	Total     int `json:"total"`
}

type WorkspaceLintStatus struct {
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
}

type MaintainWikiOptions struct {
	ProjectPath       string
	ProjectID         string
	QueueValidator    IngestQueueValidator
	SkipUnchanged     bool
	RetryFailed       bool
	KeepDone          bool
	RunLLMReview      bool
	ReviewAgent       WikiReviewAgent
	ReviewAgentError  error
	SweepAgent        QueryAgent
	RunPGSync         bool
	WikiStore         WikiPageStore
	EmbeddingProvider EmbeddingProvider
	Context           context.Context
	OnStep            func(MaintainWikiResult, MaintainWikiStep)
}

type MaintainWikiResult struct {
	Status     string             `json:"status"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Steps      []MaintainWikiStep `json:"steps"`
}

type MaintainWikiStep struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Summary    map[string]any `json:"summary,omitempty"`
	Detail     any            `json:"detail,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type WorkspaceMaintainRequest struct {
	ProjectPath   string `json:"project_path"`
	ProjectID     string `json:"project_id"`
	Agent         string `json:"agent"`
	SkipUnchanged bool   `json:"skip_unchanged"`
	RetryFailed   bool   `json:"retry_failed"`
	KeepDone      bool   `json:"keep_done"`
	RunLLMReview  bool   `json:"run_llm_review"`
	RunPGSync     bool   `json:"run_pg_sync"`
}

type WorkspaceJob struct {
	ID          string                   `json:"id"`
	ProjectPath string                   `json:"project_path"`
	ProjectID   string                   `json:"project_id"`
	Kind        string                   `json:"kind"`
	Status      string                   `json:"status"`
	Request     WorkspaceMaintainRequest `json:"request"`
	Result      *MaintainWikiResult      `json:"result,omitempty"`
	Error       string                   `json:"error,omitempty"`
	CreatedAt   time.Time                `json:"created_at"`
	StartedAt   *time.Time               `json:"started_at,omitempty"`
	FinishedAt  *time.Time               `json:"finished_at,omitempty"`
}

type workspaceJobFile struct {
	Version int            `json:"version"`
	Jobs    []WorkspaceJob `json:"jobs"`
}

var (
	workspaceJobMu     sync.Mutex
	activeWorkspaceJob = map[string]bool{}
)

func WorkspaceStatusForProject(opts WorkspaceStatusOptions) (WorkspaceStatus, error) {
	projectPath := strings.TrimSpace(opts.ProjectPath)
	status := WorkspaceStatus{
		OK:                  projectPath != "",
		Service:             "knowledge-core",
		Time:                time.Now().UTC().Format(time.RFC3339),
		ProjectPath:         projectPath,
		ProjectID:           opts.ProjectID,
		Agent:               opts.Agent,
		PGConfigured:        opts.PGConfigured,
		EmbeddingConfigured: opts.EmbeddingConfigured,
	}
	if projectPath == "" {
		status.Lint.Error = "project path is required"
		return status, nil
	}
	for _, rel := range []string{"purpose.md", "schema.md", "wiki/index.md", "wiki/overview.md", "wiki/reviews.md"} {
		exists := fileExists(filepath.Join(projectPath, filepath.FromSlash(rel)))
		status.Files = append(status.Files, WorkspaceFileStatus{Path: rel, Exists: exists})
		if !exists {
			status.OK = false
		}
	}
	queue, err := LoadIngestQueue(projectPath)
	if err != nil {
		status.OK = false
	} else {
		status.Queue = queueStatus(queue)
	}
	reviews, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: firstNonEmptyString(opts.ProjectID, "local")})
	if err != nil {
		status.OK = false
	} else {
		status.Reviews = reviewStatus(reviews)
	}
	issues, err := LintWiki(projectPath)
	if err != nil {
		status.OK = false
		status.Lint.Error = err.Error()
	} else {
		status.Lint.Count = len(issues)
	}
	return status, nil
}

func MaintainWiki(opts MaintainWikiOptions) (result MaintainWikiResult, err error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return MaintainWikiResult{}, fmt.Errorf("project path is required")
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	result = MaintainWikiResult{
		Status:    "ok",
		StartedAt: time.Now().UTC(),
	}
	defer func() {
		result.FinishedAt = time.Now().UTC()
	}()

	addStep := func(step MaintainWikiStep) bool {
		result.Steps = append(result.Steps, step)
		if step.Status == "failed" {
			result.Status = "failed"
		}
		if opts.OnStep != nil {
			opts.OnStep(result, step)
		}
		if step.Status == "failed" {
			return false
		}
		return true
	}

	if !addStep(runMaintainStep("scan_sources", func() (map[string]any, any, error) {
		scanned, err := ScanRawSources(QueueIngestOptions{ProjectPath: opts.ProjectPath})
		return map[string]any{"queued": scanned.Queued, "skipped": scanned.Skipped, "tasks": len(scanned.Tasks)}, scanned, err
	})) {
		_ = appendMaintainLog(opts.ProjectPath, result)
		return result, nil
	}

	if !addStep(runMaintainStep("run_ingest_queue", func() (map[string]any, any, error) {
		if opts.QueueValidator == nil {
			return nil, nil, fmt.Errorf("ingest queue validator is required")
		}
		queueResult, err := RunIngestQueue(RunIngestQueueOptions{
			ProjectPath:   opts.ProjectPath,
			Validator:     opts.QueueValidator,
			SkipUnchanged: opts.SkipUnchanged,
			RetryFailed:   opts.RetryFailed,
			KeepDone:      opts.KeepDone,
		})
		return map[string]any{
			"processed": queueResult.Processed,
			"done":      queueResult.Done,
			"failed":    queueResult.Failed,
			"skipped":   queueResult.Skipped,
			"files":     queueResult.Files,
		}, queueResult, err
	})) {
		_ = appendMaintainLog(opts.ProjectPath, result)
		return result, nil
	}

	if !addStep(runMaintainStep("structural_lint", func() (map[string]any, any, error) {
		issues, err := LintWiki(opts.ProjectPath)
		return map[string]any{"issues": len(issues)}, map[string]any{"issues": issues}, err
	})) {
		_ = appendMaintainLog(opts.ProjectPath, result)
		return result, nil
	}

	if opts.RunLLMReview {
		if !addStep(runMaintainStep("llm_review", func() (map[string]any, any, error) {
			if opts.ReviewAgentError != nil {
				return nil, nil, opts.ReviewAgentError
			}
			if opts.ReviewAgent == nil {
				return nil, nil, fmt.Errorf("wiki review agent is required")
			}
			issues, err := ReviewWiki(WikiReviewOptions{ProjectPath: opts.ProjectPath, Agent: opts.ReviewAgent})
			return map[string]any{"issues": len(issues)}, map[string]any{"issues": issues}, err
		})) {
			_ = appendMaintainLog(opts.ProjectPath, result)
			return result, nil
		}
	} else {
		addStep(skipMaintainStep("llm_review", "disabled"))
	}

	if !addStep(runMaintainStep("sweep_reviews", func() (map[string]any, any, error) {
		sweep, err := SweepReviewItems(ReviewSweepOptions{
			ProjectPath: opts.ProjectPath,
			ProjectID:   firstNonEmptyString(opts.ProjectID, "local"),
			Agent:       opts.SweepAgent,
			Context:     opts.Context,
		})
		return map[string]any{"rule_resolved": sweep.RuleResolved, "llm_resolved": sweep.LLMResolved, "reviews": len(sweep.Reviews)}, sweep, err
	})) {
		_ = appendMaintainLog(opts.ProjectPath, result)
		return result, nil
	}

	if opts.RunPGSync {
		if !addStep(runMaintainStep("sync_pg", func() (map[string]any, any, error) {
			if opts.WikiStore == nil {
				return nil, nil, fmt.Errorf("wiki page store is not configured")
			}
			if strings.TrimSpace(opts.ProjectID) == "" {
				return nil, nil, fmt.Errorf("project id is required for PG sync")
			}
			syncResult, err := SyncWikiPagesToStore(opts.Context, WikiSyncOptions{
				ProjectPath:       opts.ProjectPath,
				ProjectID:         opts.ProjectID,
				Store:             opts.WikiStore,
				EmbeddingProvider: opts.EmbeddingProvider,
			})
			return map[string]any{
				"pages":                   syncResult.Pages,
				"versions":                syncResult.Versions,
				"reviews":                 syncResult.Reviews,
				"sources":                 syncResult.Sources,
				"source_manifest_entries": syncResult.SourceManifestEntries,
				"embeddings":              syncResult.Embeddings,
			}, syncResult, err
		})) {
			_ = appendMaintainLog(opts.ProjectPath, result)
			return result, nil
		}
	} else {
		addStep(skipMaintainStep("sync_pg", "disabled"))
	}

	result.FinishedAt = time.Now().UTC()
	_ = appendMaintainLog(opts.ProjectPath, result)
	return result, nil
}

func runMaintainStep(name string, fn func() (map[string]any, any, error)) MaintainWikiStep {
	step := MaintainWikiStep{Name: name, Status: "ok", StartedAt: time.Now().UTC()}
	summary, detail, err := fn()
	step.FinishedAt = time.Now().UTC()
	step.Summary = summary
	step.Detail = detail
	if err != nil {
		step.Status = "failed"
		step.Error = err.Error()
	}
	return step
}

func skipMaintainStep(name, reason string) MaintainWikiStep {
	now := time.Now().UTC()
	return MaintainWikiStep{
		Name:       name,
		Status:     "skipped",
		StartedAt:  now,
		FinishedAt: now,
		Summary:    map[string]any{"reason": reason},
	}
}

func appendMaintainLog(projectPath string, result MaintainWikiResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Maintenance status: `%s`.\n\n", result.Status)
	for _, step := range result.Steps {
		fmt.Fprintf(&b, "- %s: %s", step.Name, step.Status)
		if step.Error != "" {
			fmt.Fprintf(&b, " (%s)", step.Error)
		}
		b.WriteString("\n")
	}
	return appendLog(projectPath, "maintain", "workspace maintenance", b.String())
}

func CreateWorkspaceMaintainJob(projectPath, projectID string, req WorkspaceMaintainRequest) (WorkspaceJob, bool, error) {
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return WorkspaceJob{}, false, fmt.Errorf("project path is required")
	}
	workspaceJobMu.Lock()
	defer workspaceJobMu.Unlock()
	jobs, err := loadWorkspaceJobsLocked(projectPath)
	if err != nil {
		return WorkspaceJob{}, false, err
	}
	now := time.Now().UTC()
	changed := recoverWorkspaceJobsLocked(&jobs, now)
	for _, job := range jobs.Jobs {
		if job.Kind == "workspace-maintain" && (job.Status == "queued" || job.Status == "running") {
			if changed {
				if err := saveWorkspaceJobsLocked(projectPath, jobs); err != nil {
					return WorkspaceJob{}, false, err
				}
			}
			return job, true, nil
		}
	}
	req.ProjectPath = projectPath
	req.ProjectID = projectID
	job := WorkspaceJob{
		ID:          fmt.Sprintf("workspace-job-%d-%s", now.UnixNano(), core.StableID(projectPath, projectID, now.Format(time.RFC3339Nano))[:8]),
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Kind:        "workspace-maintain",
		Status:      "queued",
		Request:     req,
		CreatedAt:   now,
	}
	activeWorkspaceJob[workspaceJobActiveKey(projectPath, job.ID)] = true
	jobs.Jobs = append([]WorkspaceJob{job}, jobs.Jobs...)
	pruneWorkspaceJobsLocked(&jobs, 50)
	if err := saveWorkspaceJobsLocked(projectPath, jobs); err != nil {
		return WorkspaceJob{}, false, err
	}
	return job, false, nil
}

func RunWorkspaceMaintainJob(job WorkspaceJob, opts MaintainWikiOptions) {
	workspaceJobMu.Lock()
	activeWorkspaceJob[workspaceJobActiveKey(job.ProjectPath, job.ID)] = true
	workspaceJobMu.Unlock()
	defer func() {
		workspaceJobMu.Lock()
		delete(activeWorkspaceJob, workspaceJobActiveKey(job.ProjectPath, job.ID))
		workspaceJobMu.Unlock()
	}()
	now := time.Now().UTC()
	_ = updateWorkspaceJob(job.ProjectPath, job.ID, func(current *WorkspaceJob) {
		current.Status = "running"
		current.StartedAt = &now
		current.Error = ""
	})
	opts.Context = context.Background()
	opts.OnStep = func(result MaintainWikiResult, _ MaintainWikiStep) {
		progress := result
		_ = updateWorkspaceJob(job.ProjectPath, job.ID, func(current *WorkspaceJob) {
			current.Status = "running"
			current.Result = &progress
		})
	}
	result, err := MaintainWiki(opts)
	finished := time.Now().UTC()
	_ = updateWorkspaceJob(job.ProjectPath, job.ID, func(current *WorkspaceJob) {
		current.Result = &result
		current.FinishedAt = &finished
		if err != nil {
			current.Status = "failed"
			current.Error = err.Error()
			return
		}
		if result.Status == "failed" {
			current.Status = "failed"
			current.Error = firstFailedStepError(result)
			return
		}
		current.Status = "succeeded"
		current.Error = ""
	})
}

func RecoverWorkspaceJobs(projectPath string) error {
	workspaceJobMu.Lock()
	defer workspaceJobMu.Unlock()
	jobs, err := loadWorkspaceJobsLocked(projectPath)
	if err != nil {
		return err
	}
	if recoverWorkspaceJobsLocked(&jobs, time.Now().UTC()) {
		return saveWorkspaceJobsLocked(projectPath, jobs)
	}
	return nil
}

func ListWorkspaceJobs(projectPath string) ([]WorkspaceJob, error) {
	workspaceJobMu.Lock()
	defer workspaceJobMu.Unlock()
	jobs, err := loadWorkspaceJobsLocked(projectPath)
	if err != nil {
		return nil, err
	}
	changed := recoverWorkspaceJobsLocked(&jobs, time.Now().UTC())
	sort.SliceStable(jobs.Jobs, func(i, j int) bool {
		return jobs.Jobs[i].CreatedAt.After(jobs.Jobs[j].CreatedAt)
	})
	if changed {
		if err := saveWorkspaceJobsLocked(projectPath, jobs); err != nil {
			return nil, err
		}
	}
	return append([]WorkspaceJob(nil), jobs.Jobs...), nil
}

func GetWorkspaceJob(projectPath, id string) (WorkspaceJob, error) {
	jobs, err := ListWorkspaceJobs(projectPath)
	if err != nil {
		return WorkspaceJob{}, err
	}
	for _, job := range jobs {
		if job.ID == id {
			return job, nil
		}
	}
	return WorkspaceJob{}, fmt.Errorf("workspace job not found: %s", id)
}

func updateWorkspaceJob(projectPath, id string, update func(*WorkspaceJob)) error {
	workspaceJobMu.Lock()
	defer workspaceJobMu.Unlock()
	jobs, err := loadWorkspaceJobsLocked(projectPath)
	if err != nil {
		return err
	}
	for i := range jobs.Jobs {
		if jobs.Jobs[i].ID == id {
			update(&jobs.Jobs[i])
			return saveWorkspaceJobsLocked(projectPath, jobs)
		}
	}
	return fmt.Errorf("workspace job not found: %s", id)
}

func loadWorkspaceJobsLocked(projectPath string) (workspaceJobFile, error) {
	if strings.TrimSpace(projectPath) == "" {
		return workspaceJobFile{}, fmt.Errorf("project path is required")
	}
	jobs := workspaceJobFile{Version: 1}
	data, err := os.ReadFile(workspaceJobsPath(projectPath))
	if os.IsNotExist(err) {
		return jobs, nil
	}
	if err != nil {
		return workspaceJobFile{}, fmt.Errorf("read workspace jobs: %w", err)
	}
	if err := json.Unmarshal(data, &jobs); err != nil {
		return workspaceJobFile{}, fmt.Errorf("parse workspace jobs: %w", err)
	}
	if jobs.Version == 0 {
		jobs.Version = 1
	}
	return jobs, nil
}

func saveWorkspaceJobsLocked(projectPath string, jobs workspaceJobFile) error {
	dir := filepath.Join(projectPath, ".kbcore")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(workspaceJobsPath(projectPath), append(data, '\n'))
}

func recoverWorkspaceJobsLocked(jobs *workspaceJobFile, now time.Time) bool {
	changed := false
	for i := range jobs.Jobs {
		if jobs.Jobs[i].Status == "queued" || jobs.Jobs[i].Status == "running" {
			if activeWorkspaceJob[workspaceJobActiveKey(jobs.Jobs[i].ProjectPath, jobs.Jobs[i].ID)] {
				continue
			}
			jobs.Jobs[i].Status = "failed"
			jobs.Jobs[i].Error = "server stopped before job completed"
			jobs.Jobs[i].FinishedAt = &now
			changed = true
		}
	}
	return changed
}

func workspaceJobActiveKey(projectPath, id string) string {
	return projectPath + "\x00" + id
}

func pruneWorkspaceJobsLocked(jobs *workspaceJobFile, max int) {
	if max <= 0 || len(jobs.Jobs) <= max {
		return
	}
	sort.SliceStable(jobs.Jobs, func(i, j int) bool {
		return jobs.Jobs[i].CreatedAt.After(jobs.Jobs[j].CreatedAt)
	})
	jobs.Jobs = append([]WorkspaceJob(nil), jobs.Jobs[:max]...)
}

func workspaceJobsPath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "workspace-jobs.json")
}

func firstFailedStepError(result MaintainWikiResult) string {
	for _, step := range result.Steps {
		if step.Status == "failed" && step.Error != "" {
			return step.Error
		}
	}
	return "maintenance failed"
}

func queueStatus(queue IngestQueue) WorkspaceQueueStatus {
	var status WorkspaceQueueStatus
	for _, task := range queue.Tasks {
		status.Total++
		switch task.Status {
		case IngestTaskPending:
			status.Pending++
		case IngestTaskProcessing:
			status.Processing++
		case IngestTaskDone:
			status.Done++
		case IngestTaskFailed:
			status.Failed++
		}
	}
	return status
}

func reviewStatus(items []core.ReviewItem) WorkspaceReviewStatus {
	var status WorkspaceReviewStatus
	for _, item := range items {
		status.Total++
		switch item.Status {
		case "open":
			status.Open++
		case "resolved":
			status.Resolved++
		case "dismissed":
			status.Dismissed++
		}
	}
	return status
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
