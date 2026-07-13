package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBootstrapStatusKeepsReadAPIsAvailableAndGatesWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	tracker := service.NewBootstrapTracker(root, 100)
	server := NewServerWithOptions(ServerOptions{DefaultProjectPath: root, Bootstrap: tracker})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ready":false`) {
		t.Fatalf("health status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/workspace/status", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"bootstrap"`) {
		t.Fatalf("workspace status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/files", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"q":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "project bootstrap is not ready") {
		t.Fatalf("gated write status=%d body=%s", rr.Code, rr.Body.String())
	}
	tracker.Succeed(0, 0)
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ready":true`) {
		t.Fatalf("ready health status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRequestLoggerRecordsSuccessfulRequest(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{RequestLogger: logger}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	line := logs.String()
	for _, want := range []string{"GET /health", "status=200", "bytes=", "duration=", "remote="} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
}

func TestRequestLoggerRecordsNotFoundRequest(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{RequestLogger: logger}).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	line := logs.String()
	for _, want := range []string{"GET /missing", "status=404", "bytes=", "duration=", "remote="} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
}

func configureTestLLMProvider(t *testing.T) ServerOptions {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected llm path %s", r.URL.Path)
		}
		var request chatCompletionRequestForTest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		content := "analysis"
		if len(request.Messages) > 0 {
			system := request.Messages[0].Content
			user := ""
			if len(request.Messages) > 1 {
				user = request.Messages[1].Content
			}
			switch {
			case strings.Contains(system, "generating updates"):
				title := "Queue Source"
				path := "wiki/sources/queue-source.md"
				if strings.Contains(user, "Validate Source") {
					title = "Validate Source"
					path = "wiki/sources/validate-source.md"
				}
				sourceRel := testLineValue(user, "Source path: ")
				content = "---FILE: " + path + "\n---\ntype: \"source-summary\"\ntitle: \"" + title + "\"\nsources:\n  - \"" + sourceRel + "\"\nconfidence: \"EXTRACTED\"\n---\n\n# " + title + "\n\nAlpha source summary.\n"
			case strings.Contains(system, "query planner"):
				content = `{"intent":"answer_from_persistent_wiki","read_first":["wiki/index.md"],"searches":[{"text":"token auth","weight":6,"rationale":"test"}],"candidate_limit":3,"answer_mode":"llm_synthesis","can_write_back":true}`
			case strings.Contains(system, "persistent LLM Wiki through tools"):
				content = `{"action":"final","answer":"Test answer [wiki/index.md].","rationale":"test"}`
			case strings.Contains(system, "answer questions"):
				content = "Test answer [wiki/index.md]."
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": content},
			}},
		})
	}))
	t.Cleanup(server.Close)
	cfg := config.Defaults()
	cfg.LLM.BaseURL = server.URL
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.Model = "test-model"
	cfg.LLM.Retries = 0
	ingest, err := compiler.NewProvider(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	query, err := service.NewQueryAgent(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	review, err := service.NewWikiReviewAgent(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	return ServerOptions{
		DefaultAgent:   "llm",
		IngestProvider: ingest,
		QueryAgent:     query,
		ReviewAgent:    review,
	}
}

func testLineValue(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

type chatCompletionRequestForTest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func TestRequestLoggerPreservesSSEFlusher(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateChatSession(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := service.StartChatRun(service.ChatAppendOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		SessionID:   session.ID,
		Question:    "demo question",
		Agent:       service.FallbackQueryAgent{},
		Limit:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		file, err := service.LoadChatRun(root, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if file.Run.Status != service.ChatRunRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	req := httptest.NewRequest(http.MethodGet, "/chats/"+session.ID+"/runs/"+run.ID+"/events?project="+root, nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{RequestLogger: logger}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "event:") {
		t.Fatalf("expected SSE events, body=%s", rr.Body.String())
	}
}

func TestWorkspaceStatusEndpoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/workspace/status", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{
		DefaultProjectPath: root,
		DefaultProjectID:   "project-1",
		DefaultAgent:       "llm",
	}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["project_path"] != root || response["agent"] != "llm" {
		t.Fatalf("unexpected response=%+v", response)
	}
}

func TestAPITokenProtectsNonLoopbackRequests(t *testing.T) {
	server := NewServerWithOptions(ServerOptions{APIToken: "secret", APIRequireToken: true})
	req := httptest.NewRequest(http.MethodGet, "/projects/files?project=/tmp/missing", nil)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/files?project=/tmp/missing", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("authorized request rejected body=%s", rr.Body.String())
	}
}

func TestAgentFileEndpoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/files?project="+root, nil)
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "wiki/index.md") {
		t.Fatalf("files status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/files/content?project="+root+"&path=wiki/index.md", nil)
	rr = httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "# demo Index") {
		t.Fatalf("content status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadSourcesEndpointWritesRawSourcesAndQueuesSupportedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("project_path", root); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("target_dir", "uploads"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("paths", "folder/alpha.md"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("files", "alpha.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# Alpha\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/sources/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var uploadResult service.UploadSourcesResult
	if err := json.Unmarshal(rr.Body.Bytes(), &uploadResult); err != nil {
		t.Fatal(err)
	}
	if len(uploadResult.Uploaded) != 1 || uploadResult.Uploaded[0].ArchivePath == "" {
		t.Fatalf("upload result=%+v", uploadResult)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(uploadResult.Uploaded[0].OriginalPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "# Alpha\n" {
		t.Fatalf("written=%q", string(written))
	}
	queue, err := service.LoadIngestQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tasks) != 1 {
		t.Fatalf("queue=%+v", queue.Tasks)
	}
}

func TestWorkspaceMaintainEndpointRunsLoop(t *testing.T) {
	serverOptions := configureTestLLMProvider(t)
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "raw", "sources", "alpha.md")
	if err := os.WriteFile(source, []byte("# Alpha\n\nAlpha mentions Beta."), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"project_path":   root,
		"project_id":     "project-1",
		"agent":          "llm",
		"skip_unchanged": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/workspace/maintain", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	server := NewServerWithOptions(serverOptions)
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var started struct {
		Job service.WorkspaceJob `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Job.ID == "" || started.Job.Status != "queued" {
		t.Fatalf("unexpected started job=%+v body=%s", started.Job, rr.Body.String())
	}
	var fetched struct {
		Job service.WorkspaceJob `json:"job"`
	}
	for i := 0; i < 50; i++ {
		req = httptest.NewRequest(http.MethodGet, "/workspace/jobs/"+started.Job.ID+"?project="+root, nil)
		rr = httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &fetched); err != nil {
			t.Fatal(err)
		}
		if fetched.Job.Status == "succeeded" || fetched.Job.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fetched.Job.Status != "succeeded" || fetched.Job.Result == nil || len(fetched.Job.Result.Steps) == 0 {
		t.Fatalf("expected completed job, got %+v", fetched.Job)
	}
}

func TestQuerySaveTitleRejectsOfflineFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "concepts", "oauth.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`# OAuth Token Validation

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&q=token+validation&save_title=Token+Validation", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{QueryAgent: service.FallbackQueryAgent{}}).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not eligible for writeback") {
		t.Fatalf("unexpected body=%s", rr.Body.String())
	}
}

func TestQuerySaveTitleSyncsWrittenSynthesisWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "oauth.md"), []byte(`---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	agent := fakeAPIQueryAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/sources/oauth.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&project_id=project-1&q=token+auth&save_title=Token+Auth", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store, QueryAgent: agent}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/syntheses/token-auth.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected query writeback pages to sync, got %+v", store.pages)
	}
}

func TestQuerySaveTitleAutoUsesAgentSuggestedWritebackTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "oauth.md"), []byte(`---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	agent := &fakeAPIActionAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/index.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   true,
		},
		actions: []core.QueryAction{
			{Action: "read", Path: "wiki/sources/oauth.md", Rationale: "read evidence"},
			{Action: "writeback", Title: "Token Auth Auto", Answer: "Token validation calls AuthService [wiki/sources/oauth.md].", Rationale: "save reusable answer"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/query?project="+root+"&project_id=project-1&q=token+auth&save_title=auto", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store, QueryAgent: agent}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/syntheses/token-auth-auto.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected auto-titled query writeback pages to sync, got %+v", store.pages)
	}
}

func TestIngestSyncsWikiAndSourceManifestWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "oauth.md")
	if err := os.WriteFile(sourcePath, []byte("# OAuth\n\nToken validation."), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	body, _ := json.Marshal(map[string]string{
		"project_path": root,
		"project_id":   "project-1",
		"source_path":  sourcePath,
		"title":        "OAuth Notes",
	})
	req := httptest.NewRequest(http.MethodPost, "/projects/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: store}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/sources/oauth.md") || !hasWikiPage(store.pages, "wiki/index.md") || !hasWikiPage(store.pages, "wiki/overview.md") {
		t.Fatalf("expected written wiki pages to sync, got %+v", store.pages)
	}
	if len(store.sourceManifestEntries) != 1 {
		t.Fatalf("expected source manifest sync, got %+v", store.sourceManifestEntries)
	}
	if len(store.sources) != 1 || store.sources[0].Path != store.sourceManifestEntries[0].RawPath {
		t.Fatalf("expected raw source provenance sync, sources=%+v manifest=%+v", store.sources, store.sourceManifestEntries)
	}
}

type fakeAPIQueryAgent struct {
	plan core.QueryPlan
}

func (f fakeAPIQueryAgent) PlanQuery(service.QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (fakeAPIQueryAgent) SynthesizeQuery(service.QuerySynthesisInput) (string, error) {
	return "Token validation calls AuthService [wiki/sources/oauth.md].", nil
}

type fakeAPIActionAgent struct {
	plan    core.QueryPlan
	actions []core.QueryAction
	index   int
}

func (f *fakeAPIActionAgent) PlanQuery(service.QueryPlanningInput) (core.QueryPlan, error) {
	return f.plan, nil
}

func (f *fakeAPIActionAgent) NextQueryAction(service.QueryActionInput) (core.QueryAction, error) {
	if f.index >= len(f.actions) {
		return core.QueryAction{Action: "final", Answer: "done"}, nil
	}
	action := f.actions[f.index]
	f.index++
	return action, nil
}

func (*fakeAPIActionAgent) SynthesizeQuery(service.QuerySynthesisInput) (string, error) {
	return "synthesized fallback", nil
}

func TestCodeImportGraphifySyncsGraphAndWikiWhenStoreConfigured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	graphPath := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(graphPath, []byte(`{
  "nodes": [
    {"id":"file:main.go","kind":"file","label":"main.go","source_file":"main.go"},
    {"id":"fn:run","kind":"function","label":"run","source_file":"main.go"}
  ],
  "edges": [
    {"source":"file:main.go","target":"fn:run","relation":"defines","confidence":"EXTRACTED","weight":1}
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wikiStore := &recordingWikiStore{}
	graphStore := &recordingCodeGraphStore{}
	body, _ := json.Marshal(map[string]string{
		"project_path": root,
		"project_id":   "project-1",
		"repo_id":      "demo-repo",
		"repo_path":    "/tmp/demo-repo",
		"graph_path":   graphPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/code/import-graphify", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{WikiPageStore: wikiStore, CodeGraphStore: graphStore}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(graphStore.repos) != 1 || len(graphStore.nodes) != 2 || len(graphStore.edges) != 1 {
		t.Fatalf("expected graph sync, repos=%+v nodes=%+v edges=%+v", graphStore.repos, graphStore.nodes, graphStore.edges)
	}
	if graphStore.deletedProjectID != "project-1" || graphStore.deletedRepoID == "" {
		t.Fatalf("expected stale graph facts to be deleted first, store=%+v", graphStore)
	}
	if !hasWikiPage(wikiStore.pages, "wiki/code/demo-repo/overview.md") || !hasWikiPage(wikiStore.pages, "wiki/index.md") || !hasWikiPage(wikiStore.pages, "wiki/log.md") {
		t.Fatalf("expected code overview and aggregate wiki pages to sync, got %+v", wikiStore.pages)
	}
}

func TestQueueAndRunQueueEndpointsUseDefaultProject(t *testing.T) {
	serverOptions := configureTestLLMProvider(t)
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Queue Source\n\nToken validation calls AuthService."), 0o644); err != nil {
		t.Fatal(err)
	}
	serverOptions.DefaultProjectPath = root
	server := NewServerWithOptions(serverOptions)
	body, _ := json.Marshal(map[string]string{
		"source_path": sourcePath,
		"title":       "Queue Source",
	})
	req := httptest.NewRequest(http.MethodPost, "/sources/queue", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("queue status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/queue/tasks", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "pending") {
		t.Fatalf("tasks status=%d body=%s", rr.Code, rr.Body.String())
	}
	runBody, _ := json.Marshal(map[string]any{
		"agent":     "llm",
		"keep_done": true,
	})
	req = httptest.NewRequest(http.MethodPost, "/queue/run", bytes.NewReader(runBody))
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"processed":1`) || !strings.Contains(rr.Body.String(), `"done":1`) {
		t.Fatalf("unexpected run body=%s", rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "sources", "queue-source.md")); err != nil {
		t.Fatalf("expected queue source wiki page: %v", err)
	}
}

func TestValidateWikiEndpointRunsLLMCompilerFlow(t *testing.T) {
	serverOptions := configureTestLLMProvider(t)
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Validate Source\n\nAlpha mentions Beta."), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{
		"project_path": root,
		"source_path":  sourcePath,
		"title":        "Validate Source",
		"agent":        "llm",
	})
	req := httptest.NewRequest(http.MethodPost, "/wiki/validate", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(serverOptions).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"SourceCount":1`) {
		t.Fatalf("unexpected body=%s", rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "sources", "validate-source.md")); err != nil {
		t.Fatalf("expected validate source wiki page: %v", err)
	}
}

func TestPostQueryUsesJSONAndDefaultProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki", "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "sources", "oauth.md"), []byte(`---
type: "source-summary"
title: "OAuth Notes"
---

# OAuth Notes

Token validation calls AuthService.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiStore{}
	agent := fakeAPIQueryAgent{
		plan: core.QueryPlan{
			Question:       "token auth",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/sources/oauth.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_synthesis",
			CanWriteBack:   true,
		},
	}
	body, _ := json.Marshal(map[string]any{
		"q":          "token auth",
		"project_id": "project-1",
		"save_title": "Token Auth",
	})
	req := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{DefaultProjectPath: root, WikiPageStore: store, QueryAgent: agent}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !hasWikiPage(store.pages, "wiki/syntheses/token-auth.md") {
		t.Fatalf("expected JSON query writeback sync, pages=%+v", store.pages)
	}
}

func TestQueryEndpointRejectsRemovedAgentModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"auto", "mock", "fallback"} {
		body, _ := json.Marshal(map[string]any{
			"project_path": root,
			"q":            "token auth",
			"agent":        agent,
		})
		req := httptest.NewRequest(http.MethodPost, "/query", bytes.NewReader(body))
		rr := httptest.NewRecorder()
		NewServer().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown query agent") {
			t.Fatalf("agent=%s status=%d body=%s", agent, rr.Code, rr.Body.String())
		}
	}
}

func TestReviewEndpointsListAndResolve(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	reviewsPath := filepath.Join(root, "wiki", "reviews.md")
	if err := os.MkdirAll(filepath.Dir(reviewsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `# Reviews

## [2026-07-02] missing-page | Alpha

- Source: ` + "`raw/sources/a.md`" + `
- Status: open

### Affected Pages

- ` + "`wiki/sources/a.md`" + `
### Detail

Create Alpha.
`
	if err := os.WriteFile(reviewsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(ServerOptions{DefaultProjectPath: root, DefaultProjectID: "project-1"})
	req := httptest.NewRequest(http.MethodGet, "/reviews?status=open", nil)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"count":1`) {
		t.Fatalf("reviews status=%d body=%s", rr.Code, rr.Body.String())
	}
	items := wiki.ParseReviewItems("project-1", content)
	body, _ := json.Marshal(map[string]string{
		"id":     items[0].ID,
		"status": "resolved",
	})
	req = httptest.NewRequest(http.MethodPost, "/reviews/resolve", bytes.NewReader(body))
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve status=%d body=%s", rr.Code, rr.Body.String())
	}
	updated, err := os.ReadFile(reviewsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "- Status: resolved") {
		t.Fatalf("review not resolved:\n%s", updated)
	}
}

type recordingWikiStore struct {
	pages                 []core.WikiPage
	sources               []core.Source
	sourceManifestEntries []core.SourceManifestEntry
}

func (s *recordingWikiStore) UpsertWikiPages(_ context.Context, pages []core.WikiPage) error {
	s.pages = append(s.pages, pages...)
	return nil
}

func (s *recordingWikiStore) DeleteWikiPagesNotIn(context.Context, string, []string) error {
	return nil
}

func (s *recordingWikiStore) WikiPageEmbeddingStatus(context.Context, string, string) (core.WikiPageEmbeddingStatus, error) {
	return core.WikiPageEmbeddingStatus{}, nil
}

func (s *recordingWikiStore) UpsertWikiPageEmbedding(context.Context, string, string, []float32, core.WikiPageEmbeddingMetadata) error {
	return nil
}

func (s *recordingWikiStore) UpsertSource(_ context.Context, source core.Source) error {
	s.sources = append(s.sources, source)
	return nil
}

func (s *recordingWikiStore) UpsertSourceManifestEntry(_ context.Context, entry core.SourceManifestEntry) error {
	s.sourceManifestEntries = append(s.sourceManifestEntries, entry)
	return nil
}

func hasWikiPage(pages []core.WikiPage, path string) bool {
	for _, page := range pages {
		if page.Path == path {
			return true
		}
	}
	return false
}

var _ service.WikiPageStore = (*recordingWikiStore)(nil)

type recordingCodeGraphStore struct {
	repos            []core.CodeRepo
	nodes            []core.GraphNode
	edges            []core.GraphEdge
	deletedProjectID string
	deletedRepoID    string
}

func (s *recordingCodeGraphStore) UpsertCodeRepo(_ context.Context, repo core.CodeRepo) error {
	s.repos = append(s.repos, repo)
	return nil
}

func (s *recordingCodeGraphStore) DeleteGraphFacts(_ context.Context, projectID, repoID string) error {
	s.deletedProjectID = projectID
	s.deletedRepoID = repoID
	return nil
}

func (s *recordingCodeGraphStore) AddGraphNode(_ context.Context, node core.GraphNode) error {
	s.nodes = append(s.nodes, node)
	return nil
}

func (s *recordingCodeGraphStore) AddGraphEdge(_ context.Context, edge core.GraphEdge) error {
	s.edges = append(s.edges, edge)
	return nil
}

var _ service.CodeGraphStore = (*recordingCodeGraphStore)(nil)
