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

type ResearchJobRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	ReviewID    string `json:"review_id,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Query       string `json:"query,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

type ResearchJob struct {
	ID           string             `json:"id"`
	ProjectPath  string             `json:"project_path"`
	ProjectID    string             `json:"project_id"`
	Status       string             `json:"status"`
	Request      ResearchJobRequest `json:"request"`
	WrittenPaths []string           `json:"written_paths,omitempty"`
	Review       *core.ReviewItem   `json:"review,omitempty"`
	Error        string             `json:"error,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	StartedAt    *time.Time         `json:"started_at,omitempty"`
	FinishedAt   *time.Time         `json:"finished_at,omitempty"`
}

type researchJobFile struct {
	Version int           `json:"version"`
	Jobs    []ResearchJob `json:"jobs"`
}

var (
	researchJobMu     sync.Mutex
	activeResearchJob = map[string]bool{}
)

func CreateResearchJob(projectPath, projectID string, req ResearchJobRequest) (ResearchJob, bool, error) {
	researchJobMu.Lock()
	defer researchJobMu.Unlock()
	if strings.TrimSpace(projectPath) == "" {
		return ResearchJob{}, false, fmt.Errorf("project path is required")
	}
	key := core.StableID("research-job-active", projectPath, req.ReviewID, req.Topic, req.Query)
	if activeResearchJob[key] {
		jobs, _ := listResearchJobsLocked(projectPath)
		for _, job := range jobs {
			if job.Status == "queued" || job.Status == "running" {
				if core.StableID("research-job-active", projectPath, job.Request.ReviewID, job.Request.Topic, job.Request.Query) == key {
					return job, true, nil
				}
			}
		}
	}
	now := time.Now().UTC()
	req.ProjectPath = projectPath
	req.ProjectID = projectID
	job := ResearchJob{
		ID:          fmt.Sprintf("research-%d", now.UnixNano()),
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Status:      "queued",
		Request:     req,
		CreatedAt:   now,
	}
	jobs, err := listResearchJobsLocked(projectPath)
	if err != nil {
		return ResearchJob{}, false, err
	}
	jobs = append([]ResearchJob{job}, jobs...)
	if len(jobs) > 100 {
		jobs = jobs[:100]
	}
	if err := saveResearchJobsLocked(projectPath, jobs); err != nil {
		return ResearchJob{}, false, err
	}
	activeResearchJob[key] = true
	return job, false, nil
}

func RunResearchJob(job ResearchJob, agent QueryAgent, research ResearchOptions) {
	key := core.StableID("research-job-active", job.ProjectPath, job.Request.ReviewID, job.Request.Topic, job.Request.Query)
	defer func() {
		researchJobMu.Lock()
		delete(activeResearchJob, key)
		researchJobMu.Unlock()
	}()
	started := time.Now().UTC()
	job.Status = "running"
	job.StartedAt = &started
	_ = updateResearchJob(job)
	review, paths, err := runResearchJobWork(context.Background(), job.Request, agent, research)
	finished := time.Now().UTC()
	job.FinishedAt = &finished
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		_ = updateResearchJob(job)
		return
	}
	job.Status = "succeeded"
	job.WrittenPaths = paths
	if review.ID != "" {
		job.Review = &review
	}
	_ = updateResearchJob(job)
}

func ListResearchJobs(projectPath string) ([]ResearchJob, error) {
	researchJobMu.Lock()
	defer researchJobMu.Unlock()
	return listResearchJobsLocked(projectPath)
}

func GetResearchJob(projectPath, id string) (ResearchJob, error) {
	jobs, err := ListResearchJobs(projectPath)
	if err != nil {
		return ResearchJob{}, err
	}
	for _, job := range jobs {
		if job.ID == id {
			return job, nil
		}
	}
	return ResearchJob{}, fmt.Errorf("research job not found: %s", id)
}

func runResearchJobWork(ctx context.Context, req ResearchJobRequest, agent QueryAgent, research ResearchOptions) (core.ReviewItem, []string, error) {
	if strings.TrimSpace(req.ReviewID) != "" {
		result, err := RunReviewAction(ReviewActionOptions{
			ProjectPath: req.ProjectPath,
			ProjectID:   req.ProjectID,
			ReviewID:    req.ReviewID,
			Action:      "deep-research",
			Agent:       agent,
			Context:     ctx,
		})
		return result.Review, result.WrittenPaths, err
	}
	title := firstNonEmptyString(req.Topic, req.Query)
	if strings.TrimSpace(title) == "" {
		return core.ReviewItem{}, nil, fmt.Errorf("topic or review_id is required")
	}
	item := core.ReviewItem{
		ID:            core.StableID(req.ProjectID, "research", title),
		ProjectID:     req.ProjectID,
		Type:          "research",
		Title:         title,
		Description:   "Ad-hoc deep research request.",
		SearchQueries: []string{firstNonEmptyString(req.Query, title)},
		Status:        "open",
		CreatedAt:     time.Now().UTC(),
	}
	if agent == nil {
		agent = FallbackQueryAgent{}
	}
	results, err := collectSearXNGResearch(ctx, item, research)
	if err != nil {
		return core.ReviewItem{}, nil, err
	}
	if len(results) == 0 {
		return core.ReviewItem{}, nil, fmt.Errorf("deep research found no sources")
	}
	docs := make([]QueryReadDocument, 0, len(results))
	for _, result := range results {
		docs = append(docs, QueryReadDocument{
			Path:    result.URL,
			Title:   result.Title,
			Kind:    "web-research",
			Content: result.Content,
		})
	}
	answer, err := agent.SynthesizeQuery(QuerySynthesisInput{
		Question: researchTopic(item),
		Docs:     docs,
		Plan: core.QueryPlan{
			Question:   researchTopic(item),
			Intent:     "deep_research",
			AnswerMode: "research_synthesis",
		},
	})
	if err != nil {
		return core.ReviewItem{}, nil, err
	}
	rel := filepath.ToSlash(filepath.Join("wiki", "syntheses", core.Slug("research-"+item.Title)+".md"))
	if err := wiki.WriteVersionedPage(req.ProjectPath, rel, []byte(researchSynthesisMarkdown(item, answer, results)), "research job"); err != nil {
		return core.ReviewItem{}, nil, err
	}
	return core.ReviewItem{}, []string{rel}, nil
}

func updateResearchJob(job ResearchJob) error {
	researchJobMu.Lock()
	defer researchJobMu.Unlock()
	jobs, err := listResearchJobsLocked(job.ProjectPath)
	if err != nil {
		return err
	}
	for i := range jobs {
		if jobs[i].ID == job.ID {
			jobs[i] = job
			return saveResearchJobsLocked(job.ProjectPath, jobs)
		}
	}
	jobs = append([]ResearchJob{job}, jobs...)
	return saveResearchJobsLocked(job.ProjectPath, jobs)
}

func listResearchJobsLocked(projectPath string) ([]ResearchJob, error) {
	data, err := os.ReadFile(researchJobsPath(projectPath))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file researchJobFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("read research jobs: %w", err)
	}
	sort.Slice(file.Jobs, func(i, j int) bool { return file.Jobs[i].CreatedAt.After(file.Jobs[j].CreatedAt) })
	return file.Jobs, nil
}

func saveResearchJobsLocked(projectPath string, jobs []ResearchJob) error {
	path := researchJobsPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(researchJobFile{Version: 1, Jobs: jobs}, "", "  ")
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

func researchJobsPath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "research-jobs.json")
}
