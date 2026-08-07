package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/codegraph"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type Server struct {
	mux                *http.ServeMux
	searchStore        service.SearchEvidenceStore
	graphStore         service.GraphEvidenceStore
	codeGraphStore     service.CodeGraphStore
	wikiPageStore      service.WikiPageStore
	queryLogStore      service.QueryLogStore
	queryAgent         service.QueryAgent
	ingestLLMProvider  compiler.Provider
	reviewLLMAgent     service.WikiReviewAgent
	embeddingProvider  service.EmbeddingProvider
	researchOptions    service.ResearchOptions
	requestLogger      *log.Logger
	apiToken           string
	apiRequireToken    bool
	defaultProjectPath string
	defaultProjectID   string
	defaultAgent       string
	bootstrap          *service.BootstrapTracker
	graphManager       *service.GraphManager
	queryRuntime       service.QueryRuntimeOptions
}

type ServerOptions struct {
	SearchStore        service.SearchEvidenceStore
	GraphStore         service.GraphEvidenceStore
	CodeGraphStore     service.CodeGraphStore
	WikiPageStore      service.WikiPageStore
	QueryLogStore      service.QueryLogStore
	QueryAgent         service.QueryAgent
	IngestProvider     compiler.Provider
	ReviewAgent        service.WikiReviewAgent
	EmbeddingProvider  service.EmbeddingProvider
	ResearchOptions    service.ResearchOptions
	RequestLogger      *log.Logger
	APIToken           string
	APIRequireToken    bool
	DefaultProjectPath string
	DefaultProjectID   string
	DefaultAgent       string
	Bootstrap          *service.BootstrapTracker
	GraphManager       *service.GraphManager
	QueryRuntime       service.QueryRuntimeOptions
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
		ingestLLMProvider:  opts.IngestProvider,
		reviewLLMAgent:     opts.ReviewAgent,
		embeddingProvider:  opts.EmbeddingProvider,
		researchOptions:    opts.ResearchOptions,
		requestLogger:      opts.RequestLogger,
		apiToken:           strings.TrimSpace(opts.APIToken),
		apiRequireToken:    opts.APIRequireToken,
		defaultProjectPath: opts.DefaultProjectPath,
		defaultProjectID:   opts.DefaultProjectID,
		defaultAgent:       opts.DefaultAgent,
		bootstrap:          opts.Bootstrap,
		graphManager:       opts.GraphManager,
		queryRuntime:       opts.QueryRuntime,
	}
	s.routes()
	if s.defaultProjectPath != "" {
		_ = service.RecoverWorkspaceJobs(s.defaultProjectPath)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
		return
	}
	if s.bootstrap != nil && !s.bootstrap.Ready() && !bootstrapRouteAvailable(r) {
		status := s.bootstrap.Snapshot()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "project bootstrap is not ready", "bootstrap": status,
		})
		return
	}
	if s.requestLogger == nil {
		s.mux.ServeHTTP(w, r)
		return
	}
	start := time.Now()
	recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(recorder, r)
	// The frontend polls this endpoint while bootstrap is running. Keep the
	// high-frequency progress check out of the default request log; callers can
	// still inspect the returned status or enable their own access logging.
	if r.URL.Path == "/workspace/status" {
		return
	}
	s.requestLogger.Printf(
		"%s %s status=%d bytes=%d duration=%s remote=%s",
		r.Method,
		r.URL.RequestURI(),
		recorder.status,
		recorder.bytes,
		time.Since(start).Round(time.Microsecond),
		r.RemoteAddr,
	)
}

func bootstrapRouteAvailable(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/webhooks/code/") {
		return true
	}
	if r.URL.Path == "/health" || r.URL.Path == "/workspace/status" {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/projects/files", "/projects/files/content", "/projects/graph", "/projects/graph/insights", "/projects/graph/node", "/projects/graph/repos", "/projects/graph/jobs",
		"/projects/sources", "/queue/tasks", "/reviews", "/research/jobs", "/chats":
		return true
	default:
		return strings.HasPrefix(r.URL.Path, "/workspace/jobs/") ||
			strings.HasPrefix(r.URL.Path, "/research/jobs/") ||
			strings.HasPrefix(r.URL.Path, "/chats/")
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	written, err := r.ResponseWriter.Write(data)
	r.bytes += written
	return written, err
}

func (r *responseRecorder) Flush() {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if r.URL.Path == "/health" {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/webhooks/code/") {
		return true
	}
	token := s.apiToken
	if token == "" {
		return true
	}
	if !s.apiRequireToken && isLoopbackRemote(r.RemoteAddr) {
		return true
	}
	return strings.TrimSpace(r.Header.Get("Authorization")) == "Bearer "+token
}

func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]any{
			"ok":      true,
			"service": "knowledge-core",
			"time":    time.Now().UTC().Format(time.RFC3339),
			"ready":   s.bootstrap == nil || s.bootstrap.Ready(),
		}
		if s.bootstrap != nil {
			response["bootstrap"] = s.bootstrap.Snapshot()
		}
		writeJSON(w, http.StatusOK, response)
	})
	s.mux.HandleFunc("GET /workspace/status", s.handleWorkspaceStatus)
	s.mux.HandleFunc("POST /workspace/maintain", s.handleWorkspaceMaintain)
	s.mux.HandleFunc("GET /workspace/jobs", s.handleWorkspaceJobs)
	s.mux.HandleFunc("GET /workspace/jobs/{id}", s.handleWorkspaceJob)
	s.mux.HandleFunc("GET /projects/files", s.handleProjectFiles)
	s.mux.HandleFunc("GET /projects/files/content", s.handleProjectFileContent)
	s.mux.HandleFunc("PUT /projects/files/content", s.handleWriteProjectFileContent)
	s.mux.HandleFunc("POST /projects/search", s.handleProjectSearch)
	s.mux.HandleFunc("GET /projects/graph", s.handleProjectGraph)
	s.mux.HandleFunc("POST /projects/graph/query", s.handleProjectGraphQuery)
	s.mux.HandleFunc("GET /projects/graph/node", s.handleProjectGraphNode)
	s.mux.HandleFunc("GET /projects/graph/insights", s.handleProjectGraphInsights)
	s.mux.HandleFunc("GET /projects/graph/repos", s.handleGraphRepositories)
	s.mux.HandleFunc("GET /projects/graph/jobs", s.handleGraphJobs)
	s.mux.HandleFunc("POST /projects/graph/jobs", s.handleQueueGraphJob)
	s.mux.HandleFunc("POST /webhooks/code/{registry}", s.handleCodeWebhook)
	s.mux.HandleFunc("GET /projects/sources", s.handleProjectSources)
	s.mux.HandleFunc("POST /projects/sources/upload", s.handleUploadSources)
	s.mux.HandleFunc("POST /projects/sources/rescan", s.handleScanSources)
	s.mux.HandleFunc("POST /projects/sources/delete", s.handleDeleteSource)
	s.mux.HandleFunc("GET /projects/sources/layout-migration", s.handleSourceLayoutMigrationStatus)
	s.mux.HandleFunc("POST /projects/sources/layout-migration", s.handleSourceLayoutMigration)
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
	s.mux.HandleFunc("GET /chats", s.handleChats)
	s.mux.HandleFunc("POST /chats", s.handleCreateChat)
	s.mux.HandleFunc("GET /chats/{id}", s.handleChat)
	s.mux.HandleFunc("DELETE /chats/{id}", s.handleDeleteChat)
	s.mux.HandleFunc("POST /chats/{id}/messages", s.handleAppendChatMessage)
	s.mux.HandleFunc("POST /chats/{id}/runs", s.handleStartChatRun)
	s.mux.HandleFunc("GET /chats/{id}/runs/{run_id}/events", s.handleChatRunEvents)
	s.mux.HandleFunc("POST /chats/{id}/runs/{run_id}/cancel", s.handleCancelChatRun)
	s.mux.HandleFunc("GET /reviews", s.handleReviews)
	s.mux.HandleFunc("POST /reviews/resolve", s.handleResolveReview)
	s.mux.HandleFunc("POST /reviews/resolve-bulk", s.handleResolveReviewsBulk)
	s.mux.HandleFunc("POST /reviews/action", s.handleReviewAction)
	s.mux.HandleFunc("POST /reviews/sweep", s.handleReviewSweep)
	s.mux.HandleFunc("POST /research/jobs", s.handleCreateResearchJob)
	s.mux.HandleFunc("GET /research/jobs", s.handleResearchJobs)
	s.mux.HandleFunc("GET /research/jobs/{id}", s.handleResearchJob)
	s.mux.HandleFunc("POST /wiki/review", s.handleWikiReview)
	s.mux.HandleFunc("POST /wiki/sync-pg", s.handleSyncWikiPG)
	s.mux.HandleFunc("POST /code/import-graphify", s.handleCodeImportGraphify)
}

func (s *Server) handleWorkspaceStatus(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	projectID := firstNonEmpty(r.URL.Query().Get("project_id"), s.defaultProjectID)
	agent := firstNonEmpty(r.URL.Query().Get("agent"), s.defaultAgent, "llm")
	if s.bootstrap != nil {
		bootstrap := s.bootstrap.Snapshot()
		if bootstrap.Status != "succeeded" {
			writeJSON(w, http.StatusOK, service.WorkspaceStatus{
				OK: false, Service: "knowledge-core", Time: time.Now().UTC().Format(time.RFC3339),
				ProjectPath: projectPath, ProjectID: projectID, Agent: agent,
				PGConfigured: s.wikiPageStore != nil, EmbeddingConfigured: s.embeddingProvider != nil,
				Bootstrap: &bootstrap,
			})
			return
		}
	}
	status, err := service.WorkspaceStatusForProject(service.WorkspaceStatusOptions{
		ProjectPath:         projectPath,
		ProjectID:           projectID,
		Agent:               agent,
		PGConfigured:        s.wikiPageStore != nil,
		EmbeddingConfigured: s.embeddingProvider != nil,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.bootstrap != nil {
		bootstrap := s.bootstrap.Snapshot()
		status.Bootstrap = &bootstrap
		if bootstrap.Status != "succeeded" {
			status.OK = false
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleWorkspaceMaintain(w http.ResponseWriter, r *http.Request) {
	var req service.WorkspaceMaintainRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID)
	req.ProjectPath = projectPath
	req.ProjectID = projectID
	job, existing, err := service.CreateWorkspaceMaintainJob(projectPath, projectID, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if existing {
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "existing": true})
		return
	}
	opts, err := s.maintainOptions(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	go service.RunWorkspaceMaintainJob(job, opts)
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "existing": false})
}

func (s *Server) handleWorkspaceJobs(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	if err := service.RecoverWorkspaceJobs(projectPath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	jobs, err := service.ListWorkspaceJobs(projectPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "count": len(jobs)})
}

func (s *Server) handleWorkspaceJob(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	if err := service.RecoverWorkspaceJobs(projectPath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	job, err := service.GetWorkspaceJob(projectPath, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *Server) maintainOptions(req service.WorkspaceMaintainRequest) (service.MaintainWikiOptions, error) {
	agentName := firstNonEmpty(req.Agent, s.defaultAgent, "llm")
	provider, err := s.ingestProvider(agentName)
	if err != nil {
		return service.MaintainWikiOptions{}, err
	}
	var reviewAgent service.WikiReviewAgent
	var reviewAgentErr error
	if req.RunLLMReview {
		reviewAgent, reviewAgentErr = s.reviewAgent(agentName)
	}
	sweepAgent, sweepErr := s.queryAgentFor(agentName)
	if sweepErr != nil {
		return service.MaintainWikiOptions{}, sweepErr
	}
	return service.MaintainWikiOptions{
		ProjectPath:       req.ProjectPath,
		ProjectID:         req.ProjectID,
		QueueValidator:    compilerQueueValidator(provider),
		SkipUnchanged:     req.SkipUnchanged,
		RetryFailed:       req.RetryFailed,
		KeepDone:          req.KeepDone,
		RunLLMReview:      req.RunLLMReview,
		ReviewAgent:       reviewAgent,
		ReviewAgentError:  reviewAgentErr,
		SweepAgent:        sweepAgent,
		RunPGSync:         req.RunPGSync,
		WikiStore:         s.wikiPageStore,
		EmbeddingProvider: s.embeddingProvider,
		Context:           context.Background(),
	}, nil
}

func (s *Server) handleProjectFiles(w http.ResponseWriter, r *http.Request) {
	files, err := service.ListProjectFiles(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "count": len(files)})
}

func (s *Server) handleProjectFileContent(w http.ResponseWriter, r *http.Request) {
	content, err := service.ReadProjectFileContent(s.projectPath(r.URL.Query().Get("project")), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, content)
}

type writeProjectFileContentRequest struct {
	ProjectPath string `json:"project_path"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	Reason      string `json:"reason"`
}

func (s *Server) handleWriteProjectFileContent(w http.ResponseWriter, r *http.Request) {
	var req writeProjectFileContentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	result, err := service.WriteProjectFileContent(service.WriteProjectFileContentOptions{
		ProjectPath: projectPath,
		Path:        req.Path,
		Content:     req.Content,
		Reason:      req.Reason,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.HasPrefix(result.Path, "wiki/") {
		if err := s.syncWrittenWikiPages(r.Context(), projectPath, firstNonEmpty(s.defaultProjectID, "local"), aggregateWikiPaths(result.Path)); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

type projectSearchRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	Query       string `json:"q"`
	Limit       int    `json:"limit"`
}

func (s *Server) handleProjectSearch(w http.ResponseWriter, r *http.Request) {
	var req projectSearchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID)
	plan := core.QueryPlan{
		Question: req.Query,
		Searches: []core.QuerySearch{{
			Text:      req.Query,
			Weight:    6,
			Rationale: "agent search",
		}},
		CandidateLimit: req.Limit,
	}
	results, err := service.SearchWikiCandidatesWithStore(r.Context(), projectPath, projectID, s.searchStore, s.embeddingProvider, plan, req.Limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
}

func (s *Server) handleProjectGraph(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	graph, err := service.QueryProjectGraph(service.ProjectGraphQuery{
		ProjectPath: s.projectPath(r.URL.Query().Get("project")), Query: r.URL.Query().Get("q"),
		Domains: splitCSV(r.URL.Query().Get("domains")), Kinds: splitCSV(r.URL.Query().Get("kinds")),
		Relations: splitCSV(r.URL.Query().Get("relations")), Confidence: splitCSV(r.URL.Query().Get("confidence")),
		Depth: depth, Limit: limit, Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

func (s *Server) handleProjectGraphQuery(w http.ResponseWriter, r *http.Request) {
	var query service.ProjectGraphQuery
	if !decodeJSON(w, r, &query) {
		return
	}
	query.ProjectPath = s.projectPath(query.ProjectPath)
	graph, err := service.QueryProjectGraph(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

func (s *Server) handleProjectGraphNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("node id is required"))
		return
	}
	graph, err := service.BuildUnifiedProjectGraph(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, node := range graph.Nodes {
		if node.ID != id {
			continue
		}
		edges := []service.WikiGraphAPIEdge{}
		for _, edge := range graph.Edges {
			if edge.Source == id || edge.Target == id {
				edges = append(edges, edge)
				if len(edges) >= 250 {
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"node": node, "edges": edges})
		return
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("graph node not found"))
}

func (s *Server) handleGraphRepositories(w http.ResponseWriter, _ *http.Request) {
	if s.graphManager == nil {
		writeJSON(w, http.StatusOK, map[string]any{"repositories": []any{}, "enabled": false})
		return
	}
	repositories := s.graphManager.Repositories()
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repositories, "enabled": true, "count": len(repositories)})
}

func (s *Server) handleGraphJobs(w http.ResponseWriter, _ *http.Request) {
	if s.graphManager == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []any{}, "enabled": false})
		return
	}
	jobs, err := s.graphManager.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "enabled": true, "count": len(jobs)})
}

func (s *Server) handleQueueGraphJob(w http.ResponseWriter, r *http.Request) {
	if s.graphManager == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("graph registry manager is not configured"))
		return
	}
	var req service.QueueGraphJobOptions
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Trigger = firstNonEmpty(req.Trigger, "api")
	job, err := s.graphManager.Queue(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleCodeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.graphManager == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("graph registry manager is not configured"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.graphManager.HandleWebhook(r.PathValue("registry"), r.Header, body)
	if err != nil {
		status := http.StatusUnauthorized
		if strings.Contains(err.Error(), "project is not ready") {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) handleProjectGraphInsights(w http.ResponseWriter, r *http.Request) {
	insights, err := service.BuildWikiGraphInsights(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, insights)
}

func (s *Server) handleProjectSources(w http.ResponseWriter, r *http.Request) {
	sources, err := service.ListSourceManifest(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources, "count": len(sources)})
}

func (s *Server) handleSourceLayoutMigrationStatus(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	if r.URL.Query().Get("plan") == "true" {
		result, err := service.PlanSourceLayoutMigration(projectPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	result, err := service.SourceLayoutMigrationStatus(projectPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSourceLayoutMigration(w http.ResponseWriter, r *http.Request) {
	var req service.SourceLayoutMigrationOptions
	if !decodeJSON(w, r, &req) {
		return
	}
	req.ProjectPath = s.projectPath(req.ProjectPath)
	result, err := service.MigrateSourceLayout(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Apply && s.wikiPageStore != nil {
		projectID := firstNonEmpty(s.defaultProjectID, "local")
		if _, err := service.SyncWikiPagesToStore(r.Context(), service.WikiSyncOptions{
			ProjectPath: req.ProjectPath, ProjectID: projectID, Store: s.wikiPageStore,
			EmbeddingProvider: s.embeddingProvider,
		}); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	var req service.DeleteSourceOptions
	if !decodeJSON(w, r, &req) {
		return
	}
	req.ProjectPath = s.projectPath(req.ProjectPath)
	req.ProjectID = firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	result, err := service.DeleteSource(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !req.DryRun && s.wikiPageStore != nil && req.ProjectID != "" {
		if _, err := service.SyncWikiPagesToStore(r.Context(), service.WikiSyncOptions{
			ProjectPath:       req.ProjectPath,
			ProjectID:         req.ProjectID,
			Store:             s.wikiPageStore,
			EmbeddingProvider: s.embeddingProvider,
		}); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

type resolveReviewsBulkRequest struct {
	ProjectPath string   `json:"project_path"`
	ProjectID   string   `json:"project_id"`
	IDs         []string `json:"ids"`
	Status      string   `json:"status"`
	Action      string   `json:"action"`
}

func (s *Server) handleResolveReviewsBulk(w http.ResponseWriter, r *http.Request) {
	var req resolveReviewsBulkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	status := firstNonEmpty(req.Status, "resolved")
	action := firstNonEmpty(req.Action, "bulk")
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	resolved := []core.ReviewItem{}
	notFound := []string{}
	for _, id := range req.IDs {
		items, err := service.UpdateReviewItemsStatus(projectPath, projectID, []string{id}, status, action)
		if err != nil {
			notFound = append(notFound, id)
			continue
		}
		resolved = append(resolved, items...)
	}
	if len(resolved) > 0 {
		if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, []string{"wiki/reviews.md"}); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved": resolved, "not_found": notFound, "count": len(resolved)})
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
		Runtime:           s.queryRuntime,
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

func (s *Server) handleUploadSources(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	projectPath := s.projectPath(r.FormValue("project_path"))
	queue := true
	if value := strings.TrimSpace(r.FormValue("queue")); value != "" {
		queue = !(strings.EqualFold(value, "false") || value == "0")
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("at least one file is required"))
		return
	}
	inputs := make([]service.UploadSourceInput, 0, len(headers))
	paths := r.MultipartForm.Value["paths"]
	var opened []multipartFile
	defer func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}()
	for _, header := range headers {
		if header.Size > core.MaxSourceBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("source %s exceeds maximum size of %d bytes", header.Filename, core.MaxSourceBytes))
			return
		}
		file, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		opened = append(opened, file)
		relativePath := header.Filename
		if i := len(inputs); i < len(paths) && strings.TrimSpace(paths[i]) != "" {
			relativePath = paths[i]
		}
		inputs = append(inputs, service.UploadSourceInput{
			RelativePath: relativePath,
			Reader:       file,
		})
	}
	result, err := service.UploadSources(service.UploadSourcesOptions{
		ProjectPath: projectPath,
		TargetDir:   r.FormValue("target_dir"),
		Queue:       queue,
		Files:       inputs,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type multipartFile interface {
	Close() error
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
	if _, err := compiler.ConvergeWikiArtifacts(projectPath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := service.RepairKnownMentionLinks(projectPath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := wiki.RebuildIndex(projectPath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := service.RefreshRelationsArtifact(projectPath); err != nil {
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
	if err := service.RefreshRelationsArtifact(projectPath); err != nil {
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
		Runtime:           s.queryRuntime,
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

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	sessions, err := service.ListChatSessions(projectPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "count": len(sessions)})
}

type createChatRequest struct {
	ProjectPath string `json:"project_path"`
	Title       string `json:"title"`
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	var req createChatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	session, err := service.CreateChatSession(s.projectPath(req.ProjectPath), req.Title)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	session, err := service.ReadChatSession(s.projectPath(r.URL.Query().Get("project")), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	if err := service.DeleteChatSession(s.projectPath(r.URL.Query().Get("project")), r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

type appendChatMessageRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	Question    string `json:"q"`
	Limit       int    `json:"limit"`
	Agent       string `json:"agent"`
	SaveTitle   string `json:"save_title"`
}

func (s *Server) handleAppendChatMessage(w http.ResponseWriter, r *http.Request) {
	var req appendChatMessageRequest
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
	result, err := service.AppendChatMessage(service.ChatAppendOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		SessionID:         r.PathValue("id"),
		Question:          req.Question,
		Limit:             req.Limit,
		Agent:             agent,
		SaveTitle:         req.SaveTitle,
		SearchStore:       s.searchStore,
		GraphStore:        s.graphStore,
		QueryLogStore:     s.queryLogStore,
		EmbeddingProvider: s.embeddingProvider,
		Context:           r.Context(),
		Runtime:           s.queryRuntime,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if result.Writeback != nil {
		if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, aggregateWikiPaths(result.Writeback.Path)); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStartChatRun(w http.ResponseWriter, r *http.Request) {
	var req appendChatMessageRequest
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
	run, session, err := service.StartChatRun(service.ChatAppendOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		SessionID:         r.PathValue("id"),
		Question:          req.Question,
		Limit:             req.Limit,
		Agent:             agent,
		SaveTitle:         req.SaveTitle,
		SearchStore:       s.searchStore,
		GraphStore:        s.graphStore,
		QueryLogStore:     s.queryLogStore,
		EmbeddingProvider: s.embeddingProvider,
		Runtime:           s.queryRuntime,
		OnWriteback: func(ctx context.Context, writeback service.QueryWritebackResult) error {
			return s.syncWrittenWikiPages(ctx, projectPath, projectID, aggregateWikiPaths(writeback.Path))
		},
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run, "session": session})
}

func (s *Server) handleChatRunEvents(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	runID := r.PathValue("run_id")
	file, events, unsubscribe, err := service.SubscribeChatRun(projectPath, runID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer unsubscribe()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	for _, event := range file.Events {
		if err := writeChatRunSSE(w, event); err != nil {
			return
		}
		flusher.Flush()
	}
	if isTerminalChatRunStatus(file.Run.Status) {
		return
	}
	heartbeat := time.NewTicker(3 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case now := <-heartbeat.C:
			if err := writeChatRunSSE(w, service.ChatRunEvent{
				ID:        fmt.Sprintf("%s-heartbeat-%d", runID, now.UnixNano()),
				RunID:     runID,
				Type:      "heartbeat",
				Time:      now.UTC(),
				Message:   "查询仍在运行",
				ElapsedMS: now.Sub(file.Run.StartedAt).Milliseconds(),
			}); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeChatRunSSE(w, event); err != nil {
				return
			}
			flusher.Flush()
			if isTerminalChatRunEvent(event.Type) {
				return
			}
		}
	}
}

func (s *Server) handleCancelChatRun(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	run, err := service.CancelChatRun(projectPath, r.PathValue("run_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func writeChatRunSSE(w http.ResponseWriter, event service.ChatRunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", chatRunSSEEventName(event.Type), data)
	return err
}

func chatRunSSEEventName(eventType string) string {
	if isTerminalChatRunEvent(eventType) {
		return eventType
	}
	return "progress"
}

func isTerminalChatRunEvent(eventType string) bool {
	return eventType == "completed" || eventType == "error" || eventType == "canceled"
}

func isTerminalChatRunStatus(status service.ChatRunStatus) bool {
	return status == service.ChatRunSucceeded || status == service.ChatRunFailed || status == service.ChatRunCanceled
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
	Action      string `json:"action"`
}

func (s *Server) handleResolveReview(w http.ResponseWriter, r *http.Request) {
	var req resolveReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	status := firstNonEmpty(req.Status, "resolved")
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	items, err := service.UpdateReviewItemsStatus(projectPath, projectID, []string{req.ID}, status, req.Action)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, []string{"wiki/reviews.md"}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": items[0]})
}

type reviewActionRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	ID          string `json:"id"`
	Action      string `json:"action"`
	Agent       string `json:"agent"`
}

func (s *Server) handleReviewAction(w http.ResponseWriter, r *http.Request) {
	var req reviewActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	var agent service.QueryAgent
	if req.Action == "deep-research" {
		selected, err := s.queryAgentFor(firstNonEmpty(req.Agent, s.defaultAgent, "llm"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		agent = selected
	}
	result, err := service.RunReviewAction(service.ReviewActionOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		ReviewID:    req.ID,
		Action:      req.Action,
		Agent:       agent,
		Research:    s.researchOptions,
		Context:     r.Context(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	paths := append([]string{"wiki/reviews.md"}, result.WrittenPaths...)
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, paths); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type reviewSweepRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	Agent       string `json:"agent"`
}

func (s *Server) handleReviewSweep(w http.ResponseWriter, r *http.Request) {
	var req reviewSweepRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	var agent service.QueryAgent
	selected, err := s.queryAgentFor(firstNonEmpty(req.Agent, s.defaultAgent, "llm"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agent = selected
	result, err := service.SweepReviewItems(service.ReviewSweepOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Agent:       agent,
		Context:     r.Context(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.syncWrittenWikiPages(r.Context(), projectPath, projectID, []string{"wiki/reviews.md"}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCreateResearchJob(w http.ResponseWriter, r *http.Request) {
	var req service.ResearchJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projectPath := s.projectPath(req.ProjectPath)
	projectID := firstNonEmpty(req.ProjectID, s.defaultProjectID, "local")
	req.ProjectPath = projectPath
	req.ProjectID = projectID
	job, existing, err := service.CreateResearchJob(projectPath, projectID, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !existing {
		agent, err := s.queryAgentFor(firstNonEmpty(req.Agent, s.defaultAgent, "llm"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		go service.RunResearchJob(job, agent, s.researchOptions)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "existing": existing})
}

func (s *Server) handleResearchJobs(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	jobs, err := service.ListResearchJobs(projectPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "count": len(jobs)})
}

func (s *Server) handleResearchJob(w http.ResponseWriter, r *http.Request) {
	projectPath := s.projectPath(r.URL.Query().Get("project"))
	job, err := service.GetResearchJob(projectPath, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
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
	agentName = firstNonEmpty(agentName, s.defaultAgent, "llm")
	switch agentName {
	case "llm":
		if s.queryAgent != nil {
			return s.queryAgent, nil
		}
		return nil, fmt.Errorf("llm query agent is not configured")
	default:
		return nil, fmt.Errorf("unknown query agent %q", agentName)
	}
}

func (s *Server) ingestProvider(agentName string) (compiler.Provider, error) {
	agentName = firstNonEmpty(agentName, s.defaultAgent, "llm")
	switch agentName {
	case "llm":
		if s.ingestLLMProvider == nil {
			return nil, fmt.Errorf("llm ingest provider is not configured")
		}
		return s.ingestLLMProvider, nil
	default:
		return nil, fmt.Errorf("unknown ingest agent %q", agentName)
	}
}

func (s *Server) reviewAgent(agentName string) (service.WikiReviewAgent, error) {
	agentName = firstNonEmpty(agentName, s.defaultAgent, "llm")
	switch agentName {
	case "llm":
		if s.reviewLLMAgent == nil {
			return nil, fmt.Errorf("llm wiki review agent is not configured")
		}
		return s.reviewLLMAgent, nil
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
	out := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}
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
