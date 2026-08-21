package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRequestLoggerRecordsRequest(t *testing.T) {
	var logs bytes.Buffer
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	NewServerWithOptions(ServerOptions{RequestLogger: log.New(&logs, "", 0)}).ServeHTTP(rr, req)
	for _, want := range []string{"GET /health", "status=200", "duration="} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log=%q missing %q", logs.String(), want)
		}
	}
}

func TestGenerativeQueryAndKnowledgeChatRoutesAreRemoved(t *testing.T) {
	server := NewServer()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/projects/query"},
		{http.MethodPost, "/query"},
		{http.MethodGet, "/chats"},
		{http.MethodPost, "/chats"},
		{http.MethodPost, "/chats/id/messages"},
		{http.MethodPost, "/chats/id/runs"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d body=%s", route.method, route.path, rr.Code, rr.Body.String())
		}
	}
}

func TestProjectSearchRemainsDeterministicRecall(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "entities", "auth.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntitle: AuthService\ntype: entity\naliases: [Token Guard]\n---\n\n# AuthService\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"project_path": root, "q": "Token Guard", "limit": 5})
	req := httptest.NewRequest(http.MethodPost, "/projects/search", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	NewServer().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "wiki/entities/auth.md") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReviewEndpointsListAndResolve(t *testing.T) {
	root := t.TempDir()
	reviewsPath := filepath.Join(root, "wiki", "reviews.md")
	if err := os.MkdirAll(filepath.Dir(reviewsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# Reviews\n\n## [2026-07-02] missing-page | Alpha\n\n- Source: `raw/sources/a.md`\n- Status: open\n\n### Affected Pages\n\n- `wiki/sources/a.md`\n### Detail\n\nCreate Alpha.\n"
	if err := os.WriteFile(reviewsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(ServerOptions{DefaultProjectPath: root, DefaultProjectID: "project-1"})
	req := httptest.NewRequest(http.MethodGet, "/reviews?status=open", nil)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"count":1`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	items := wiki.ParseReviewItems("project-1", content)
	body, _ := json.Marshal(map[string]string{"id": items[0].ID, "status": "resolved"})
	req = httptest.NewRequest(http.MethodPost, "/reviews/resolve", bytes.NewReader(body))
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
