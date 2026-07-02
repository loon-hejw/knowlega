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
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type Server struct {
	mux               *http.ServeMux
	searchStore       service.SearchEvidenceStore
	graphStore        service.GraphEvidenceStore
	codeGraphStore    service.CodeGraphStore
	wikiPageStore     service.WikiPageStore
	queryLogStore     service.QueryLogStore
	queryAgent        service.QueryAgent
	embeddingProvider service.EmbeddingProvider
	defaultProjectID  string
}

type ServerOptions struct {
	SearchStore       service.SearchEvidenceStore
	GraphStore        service.GraphEvidenceStore
	CodeGraphStore    service.CodeGraphStore
	WikiPageStore     service.WikiPageStore
	QueryLogStore     service.QueryLogStore
	QueryAgent        service.QueryAgent
	EmbeddingProvider service.EmbeddingProvider
	DefaultProjectID  string
}

func NewServer() *Server {
	return NewServerWithOptions(ServerOptions{})
}

func NewServerWithOptions(opts ServerOptions) *Server {
	s := &Server{
		mux:               http.NewServeMux(),
		searchStore:       opts.SearchStore,
		graphStore:        opts.GraphStore,
		codeGraphStore:    opts.CodeGraphStore,
		wikiPageStore:     opts.WikiPageStore,
		queryLogStore:     opts.QueryLogStore,
		queryAgent:        opts.QueryAgent,
		embeddingProvider: opts.EmbeddingProvider,
		defaultProjectID:  opts.DefaultProjectID,
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
	agent := s.queryAgent
	if agent == nil {
		envAgent, ok, err := service.NewEnvQueryAgent()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if ok {
			agent = envAgent
		}
	}
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		projectID = s.defaultProjectID
	}
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		ProjectPath:       r.URL.Query().Get("project"),
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
			ProjectPath: r.URL.Query().Get("project"),
			Title:       writebackTitle,
			Answer:      answer,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.syncWrittenWikiPages(r.Context(), r.URL.Query().Get("project"), projectID, aggregateWikiPaths(writeback.Path)); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"answer": answer, "writeback": writeback})
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	issues, err := service.LintWiki(r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "count": len(issues)})
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
