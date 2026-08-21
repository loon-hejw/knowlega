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
	maintenanceAgent   service.MaintenanceSynthesisAgent
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
}

type ServerOptions struct {
	SearchStore        service.SearchEvidenceStore
	GraphStore         service.GraphEvidenceStore
	CodeGraphStore     service.CodeGraphStore
	WikiPageStore      service.WikiPageStore
	MaintenanceAgent   service.MaintenanceSynthesisAgent
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
		maintenanceAgent:   opts.MaintenanceAgent,
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
	case "/projects/graph", "/projects/graph/insights", "/projects/graph/node", "/projects/graph/repos", "/projects/graph/jobs", "/reviews", "/research/jobs":
		return true
	default:
		return strings.HasPrefix(r.URL.Path, "/workspace/jobs/") ||
			strings.HasPrefix(r.URL.Path, "/research/jobs/")
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
	s.mux.HandleFunc("POST /projects/search", s.handleProjectSearch)
	s.mux.HandleFunc("GET /projects/graph", s.handleProjectGraph)
	s.mux.HandleFunc("POST /projects/graph/query", s.handleProjectGraphQuery)
	s.mux.HandleFunc("GET /projects/graph/node", s.handleProjectGraphNode)
	s.mux.HandleFunc("GET /projects/graph/insights", s.handleProjectGraphInsights)
	s.mux.HandleFunc("GET /projects/graph/repos", s.handleGraphRepositories)
	s.mux.HandleFunc("GET /projects/graph/jobs", s.handleGraphJobs)
	s.mux.HandleFunc("POST /projects/graph/jobs", s.handleQueueGraphJob)
	s.mux.HandleFunc("POST /webhooks/code/{registry}", s.handleCodeWebhook)
	s.mux.HandleFunc("POST /projects/init", s.handleInit)
	s.mux.HandleFunc("GET /projects/lint", s.handleLint)
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
		SweepAgent:        s.maintenanceAgent,
		RunPGSync:         req.RunPGSync,
		WikiStore:         s.wikiPageStore,
		EmbeddingProvider: s.embeddingProvider,
		Context:           context.Background(),
	}, nil
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
	results, err := service.SearchProjectDocuments(r.Context(), projectPath, projectID, req.Query, req.Limit, s.searchStore, s.embeddingProvider)
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

func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	issues, err := service.LintWiki(s.projectPath(r.URL.Query().Get("project")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "count": len(issues)})
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
	result, err := service.RunReviewAction(service.ReviewActionOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		ReviewID:    req.ID,
		Action:      req.Action,
		Agent:       s.maintenanceAgent,
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
	result, err := service.SweepReviewItems(service.ReviewSweepOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Agent:       s.maintenanceAgent,
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
		go service.RunResearchJob(job, s.maintenanceAgent, s.researchOptions)
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
