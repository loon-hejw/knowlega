package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type Server struct {
	mux                *http.ServeMux
	searchStore        service.SearchEvidenceStore
	graphStore         service.GraphEvidenceStore
	codeGraphStore     service.CodeGraphStore
	wikiPageStore      service.WikiPageStore
	queryLogStore      service.QueryLogStore
	queryAgent         service.QueryAgent
	embeddingProvider  service.EmbeddingProvider
	defaultProjectPath string
	defaultProjectID   string
	defaultAgent       string
}

type ServerOptions struct {
	SearchStore        service.SearchEvidenceStore
	GraphStore         service.GraphEvidenceStore
	CodeGraphStore     service.CodeGraphStore
	WikiPageStore      service.WikiPageStore
	QueryLogStore      service.QueryLogStore
	QueryAgent         service.QueryAgent
	EmbeddingProvider  service.EmbeddingProvider
	DefaultProjectPath string
	DefaultProjectID   string
	DefaultAgent       string
}

func NewServer() *Server {
	return NewServerWithOptions(ServerOptions{})
}

func NewServerWithOptions(opts ServerOptions) *Server {
	s := &Server{
		mux:                http.NewServeMux(),
		searchStore:        opts.SearchStore,
		graphStore:         opts.GraphStore,
		codeGraphStore:     opts.CodeGraphStore,
		wikiPageStore:      opts.WikiPageStore,
		queryLogStore:      opts.QueryLogStore,
		queryAgent:         opts.QueryAgent,
		embeddingProvider:  opts.EmbeddingProvider,
		defaultProjectPath: opts.DefaultProjectPath,
		defaultProjectID:   opts.DefaultProjectID,
		defaultAgent:       opts.DefaultAgent,
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"service": "knowledge-core",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})
	s.mux.HandleFunc("POST /projects/init", s.handleInit)
	s.mux.HandleFunc("POST /projects/ingest", s.handleIngest)
	s.mux.HandleFunc("GET /projects/query", s.handleQuery)
	s.mux.HandleFunc("GET /projects/lint", s.handleLint)
	s.mux.HandleFunc("POST /sources/queue", s.handleQueueSource)
	s.mux.HandleFunc("POST /sources/scan", s.handleScanSources)
	s.mux.HandleFunc("GET /queue/tasks", s.handleQueueTasks)
	s.mux.HandleFunc("POST /queue/run", s.handleRunQueue)
	s.mux.HandleFunc("POST /wiki/validate", s.handleValidateWiki)
	s.mux.HandleFunc("POST /query", s.handleQueryPost)
	s.mux.HandleFunc("GET /reviews", s.handleReviews)
	s.mux.HandleFunc("POST /reviews/resolve", s.handleResolveReview)
	s.mux.HandleFunc("POST /wiki/review", s.handleWikiReview)
	s.mux.HandleFunc("POST /wiki/sync-pg", s.handleSyncWikiPG)
	s.mux.HandleFunc("POST /code/import-graphify", s.handleCodeImportGraphify)
}

type initRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := wiki.InitProject(wiki.ProjectOptions{Path: req.Path, Name: req.Name}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": req.Path})
}

type ingestRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	SourcePath  string `json:"source_path"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.ProjectPath = s.projectPath(req.ProjectPath)
	result, err := service.IngestSource(service.IngestOptions{
		ProjectPath: req.ProjectPath,
		SourcePath:  req.SourcePath,
		Title:       req.Title,
		Kind:        req.Kind,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), req.ProjectPath, firstNonEmpty(req.ProjectID, s.defaultProjectID), aggregateWikiPaths(result.WikiPath)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	agent, err := s.queryAgentFor("")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		projectID = s.defaultProjectID
	}
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		Question:          r.URL.Query().Get("q"),
		Limit:             limit,
		Agent:             agent,
		SearchStore:       s.searchStore,
		GraphStore:        s.graphStore,
		QueryLogStore:     s.queryLogStore,
		EmbeddingProvider: s.embeddingProvider,
		Context:           r.Context(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if saveTitle := r.URL.Query().Get("save_title"); saveTitle != "" {
		if !answer.Plan.CanWriteBack {
			writeError(w, http.StatusBadRequest, fmt.Errorf("query answer is not eligible for writeback; configure an LLM query agent"))
			return
		}
		writebackTitle := saveTitle
		if writebackTitle == "auto" {
			writebackTitle = answer.SuggestedWritebackTitle
		}
		writeback, err := service.WriteQueryAnswer(service.QueryWritebackOptions{
			ProjectPath: projectPath,
			Title:       writebackTitle,
			Answer:      answer,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, aggregateWikiPaths(writeback.Path)); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"answer": answer, "writeback": writeback})
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	issues, err := service.LintWiki(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "count": len(issues)})
}

type queueSourceRequest struct {
	ProjectPath string `json:"project_path"`
	SourcePath  string `json:"source_path"`
	Title       string `json:"title"`
}

func (s *Server) handleQueueSource(w http.ResponseWriter, r *http.Request) {
	var req queueSourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	task, err := service.QueueIngestSource(service.QueueIngestOptions{
		ProjectPath: s.projectPath(req.ProjectPath),
		SourcePath:  req.SourcePath,
		Title:       req.Title,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

type projectRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
}

func (s *Server) handleScanSources(w http.ResponseWriter, r *http.Request) {
	var req projectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := service.ScanRawSources(service.QueueIngestOptions{ProjectPath: s.projectPath(req.ProjectPath)})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleQueueTasks(w http.ResponseWriter, r *http.Request) {
	queue, err := service.LoadIngestQueue(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, queue)
}

type runQueueRequest struct {
	ProjectPath   string `json:"project_path"`
	ProjectID     string `json:"project_id"`
	Agent         string `json:"agent"`
	SkipUnchanged bool   `json:"skip_unchanged"`
	Max           int    `json:"max"`
	RetryFailed   bool   `json:"retry_failed"`
	KeepDone      bool   `json:"keep_done"`
}

func (s *Server) handleRunQueue(w http.ResponseWriter, r *http.Request) {
	var req runQueueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	provider, err := s.ingestProvider(req.Agent)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	result, err := service.RunIngestQueue(service.RunIngestQueueOptions{
		ProjectPath:   projectPath,
		Validator:     compilerQueueValidator(provider),
		SkipUnchanged: req.SkipUnchanged,
		MaxTasks:      req.Max,
		RetryFailed:   req.RetryFailed,
		KeepDone:      req.KeepDone,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, firstNonEmpty(req.ProjectID, s.defaultProjectID), aggregateWikiPaths("wiki/reviews.md")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type validateWikiRequest struct {
	ProjectPath   string `json:"project_path"`
	ProjectID     string `json:"project_id"`
	SourcePath    string `json:"source_path"`
	Title         string `json:"title"`
	Agent         string `json:"agent"`
	SkipUnchanged bool   `json:"skip_unchanged"`
}

func (s *Server) handleValidateWiki(w http.ResponseWriter, r *http.Request) {
	var req validateWikiRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	provider, err := s.ingestProvider(req.Agent)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	result, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath:   projectPath,
		SourcePath:    req.SourcePath,
		Title:         req.Title,
		Provider:      provider,
		SkipUnchanged: req.SkipUnchanged,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, firstNonEmpty(req.ProjectID, s.defaultProjectID), batchWrittenWikiPaths(result)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type queryRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	Question    string `json:"q"`
	Limit       int    `json:"limit"`
	Agent       string `json:"agent"`
	SaveTitle   string `json:"save_title"`
}

func (s *Server) handleQueryPost(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	agent, err := s.queryAgentFor(req.Agent)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID)
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		Question:          req.Question,
		Limit:             req.Limit,
		Agent:             agent,
		SearchStore:       s.searchStore,
		GraphStore:        s.graphStore,
		QueryLogStore:     s.queryLogStore,
		EmbeddingProvider: s.embeddingProvider,
		Context:           r.Context(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.SaveTitle == "" {
		writeJSON(w, http.StatusOK, answer)
		return
	}
	if !answer.Plan.CanWriteBack {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query answer is not eligible for writeback; configure an LLM query agent"))
		return
	}
	writebackTitle := req.SaveTitle
	if writebackTitle == "auto" {
		writebackTitle = answer.SuggestedWritebackTitle
	}
	writeback, err := service.WriteQueryAnswer(service.QueryWritebackOptions{
		ProjectPath: projectPath,
		Title:       writebackTitle,
		Answer:      answer,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, aggregateWikiPaths(writeback.Path)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"answer": answer, "writeback": writeback})
}

func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	projectID := firstNonEmpty(r.URL.Query().Get("project_id"), s.defaultProjectID, "local")
	status := r.URL.Query().Get("status")
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: projectID})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if status != "" {
		filtered := items[:0]
		for _, item := range items {
			if item.Status == status {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": items, "count": len(items)})
}

type resolveReviewRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	ID          string `json:"id"`
	Status      string `json:"status"`
}

func (s *Server) handleResolveReview(w http.ResponseWriter, r *http.Request) {
	var req resolveReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	status := firstNonEmpty(req.Status, "resolved")
	resolvedAt := time.Time{}
	if status == "resolved" || status == "dismissed" {
		resolvedAt = time.Now().UTC()
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	item, err := wiki.UpdateReviewItemStatus(projectPath, projectID, req.ID, status, resolvedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, []string{"wiki/reviews.md"}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": item})
}

type wikiReviewRequest struct {
	ProjectPath string `json:"project_path"`
	Agent       string `json:"agent"`
}

func (s *Server) handleWikiReview(w http.ResponseWriter, r *http.Request) {
	var req wikiReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	agent, err := s.reviewAgent(req.Agent)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	issues, err := service.ReviewWiki(service.WikiReviewOptions{
		ProjectPath: s.projectPath(req.ProjectPath),
		Agent:       agent,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "count": len(issues)})
}

type syncWikiPGRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
}

func (s *Server) handleSyncWikiPG(w http.ResponseWriter, r *http.Request) {
	var req syncWikiPGRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.wikiPageStore == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("wiki page store is not configured"))
		return
	}
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID)
	if projectID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("project_id is required"))
		return
	}
	result, err := service.SyncWikiPagesToStore(r.Context(), service.WikiSyncOptions{
		ProjectPath:       s.projectPath(req.ProjectPath),
		ProjectID:         projectID,
		Store:             s.wikiPageStore,
		EmbeddingProvider: s.embeddingProvider,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type codeImportRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	RepoID      string `json:"repo_id"`
	RepoPath    string `json:"repo_path"`
	GraphPath   string `json:"graph_path"`
	ReportPath  string `json:"report_path"`
}

func (s *Server) handleCodeImportGraphify(w http.ResponseWriter, r *http.Request) {
	var req codeImportRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := service.ImportGraphifyCodeSnapshot(service.CodeImportOptions{
		ProjectPath: req.ProjectPath,
		RepoID:      req.RepoID,
		RepoPath:    req.RepoPath,
		GraphPath:   req.GraphPath,
		ReportPath:  req.ReportPath,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID)
	if err := s.syncCodeGraphImport(r.Context(), req, result, projectID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), req.ProjectPath, projectID, aggregateWikiPaths(result.Overview)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) syncCodeGraphImport(ctx context.Context, req codeImportRequest, result service.CodeImportResult, projectID string) error {
	if s.codeGraphStore == nil || projectID == "" {
		return nil
	}
	repoID := req.RepoID
	if repoID == "" {
		repoID = core.Slug(filepath.Base(req.RepoPath))
	}
	snap, err := codegraph.ImportGraphify(repoID, req.RepoPath, req.GraphPath, req.ReportPath)
	if err != nil {
		return err
	}
	_, err = service.SyncCodeGraphSnapshot(ctx, s.codeGraphStore, projectID, snap, filepath.ToSlash(filepath.Join(result.SnapshotDir, "graph.json")))
	return err
}

func (s *Server) syncWrittenWikiPages(ctx context.Context, projectPath, projectID string, paths []string) error {
	if s.wikiPageStore == nil || projectID == "" || len(paths) == 0 {
		return nil
	}
	if _, err := service.SyncWikiPagePathsToStore(ctx, service.WikiSyncOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		Store:             s.wikiPageStore,
		EmbeddingProvider: s.embeddingProvider,
	}, paths); err != nil {
		return err
	}
	if _, err := service.SyncSourceManifestToStore(ctx, service.WikiSyncOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Store:       s.wikiPageStore,
	}); err != nil {
		return err
	}
	return nil
}

func (s *Server) projectPath(value string) string {
	return firstNonEmpty(value, s.defaultProjectPath)
}

func (s *Server) queryAgentFor(agentName string) (service.QueryAgent, error) {
	agentName = firstNonEmpty(agentName, s.defaultAgent, "auto")
	switch agentName {
	case "auto":
		if s.queryAgent != nil {
			return s.queryAgent, nil
		}
		envAgent, ok, err := service.NewEnvQueryAgent()
		if err != nil {
			return nil, err
		}
		if ok {
			return envAgent, nil
		}
		return nil, nil
	case "fallback":
		return service.FallbackQueryAgent{}, nil
	case "mock":
		return service.MockQueryAgent{}, nil
	case "llm":
		if s.queryAgent != nil {
			return s.queryAgent, nil
		}
		envAgent, ok, err := service.NewEnvQueryAgent()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("llm agent requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL")
		}
		return envAgent, nil
	default:
		return nil, fmt.Errorf("unknown query agent %q", agentName)
	}
}

func (s *Server) ingestProvider(agentName string) (compiler.Provider, error) {
	agentName = firstNonEmpty(agentName, s.defaultAgent, "auto")
	switch agentName {
	case "auto":
		envProvider, ok, err := compiler.NewEnvProvider()
		if err != nil {
			return nil, err
		}
		if ok {
			return envProvider, nil
		}
		return compiler.MockProvider{}, nil
	case "mock":
		return compiler.MockProvider{}, nil
	case "llm":
		envProvider, ok, err := compiler.NewEnvProvider()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("llm agent requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL")
		}
		return envProvider, nil
	default:
		return nil, fmt.Errorf("unknown ingest agent %q", agentName)
	}
}

func (s *Server) reviewAgent(agentName string) (service.WikiReviewAgent, error) {
	agentName = firstNonEmpty(agentName, s.defaultAgent, "auto")
	switch agentName {
	case "auto", "llm":
		envAgent, ok, err := service.NewEnvWikiReviewAgent()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("wiki review requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL")
		}
		return envAgent, nil
	default:
		return nil, fmt.Errorf("unknown review agent %q", agentName)
	}
}

func compilerQueueValidator(provider compiler.Provider) service.IngestQueueValidator {
	return func(opts service.QueueValidateOptions) (service.QueueValidateResult, error) {
		result, err := compiler.ValidateLLMWiki(compiler.ValidateOptions{
			ProjectPath:   opts.ProjectPath,
			SourcePath:    opts.SourcePath,
			Title:         opts.Title,
			Provider:      provider,
			SkipUnchanged: opts.SkipUnchanged,
		})
		if err != nil {
			return service.QueueValidateResult{}, err
		}
		return service.QueueValidateResult{
			RawPath: result.RawPath,
			Files:   result.Files,
			Skipped: result.Skipped,
			SHA256:  result.SHA256,
		}, nil
	}
}

func batchWrittenWikiPaths(result compiler.BatchValidateResult) []string {
	paths := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}
	for _, item := range result.Results {
		if item.Skipped {
			continue
		}
		paths = append(paths, item.Files...)
	}
	return paths
}

func aggregateWikiPaths(paths ...string) []string {
	out := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md"}
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			out = append(out, filepath.ToSlash(path))
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode json: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
