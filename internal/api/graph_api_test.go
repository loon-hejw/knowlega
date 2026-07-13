package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestProjectGraphQueryReturnsBoundedUnifiedGraph(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "graph-api"}); err != nil {
		t.Fatal(err)
	}
	page := wiki.RenderPage(wiki.Page{Title: "Alpha", Type: "concept", Body: "# Alpha\n"})
	if err := wiki.WriteVersionedPage(project, "wiki/concepts/alpha.md", []byte(page), "test"); err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(ServerOptions{DefaultProjectPath: project})
	body := bytes.NewBufferString(`{"query":"Alpha","domains":["wiki"],"limit":10}`)
	req := httptest.NewRequest(http.MethodPost, "/projects/graph/query", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var graph service.WikiGraphAPIResult
	if err := json.Unmarshal(rr.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 1 || graph.Nodes[0].ID != "wiki/concepts/alpha.md" || graph.Nodes[0].Domain != "wiki" {
		t.Fatalf("graph=%+v", graph)
	}
}

func TestCodeWebhookUsesProviderSignatureInsteadOfAPIToken(t *testing.T) {
	project := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "webhook-api"}); err != nil {
		t.Fatal(err)
	}
	manager, err := service.NewGraphManager(service.GraphManagerOptions{ProjectPath: project, Config: config.GraphConfig{
		MaxParallelJobs: 1,
		Registries: []config.CodeRegistryConfig{{
			ID: "github", Provider: "github", BaseURL: "https://github.example.test", WebhookSecret: "webhook-secret", Directory: "github",
			Repositories: []config.CodeRepositoryConfig{{ID: "demo", FullName: "team/demo", CloneURL: "https://github.example.test/team/demo.git", Branch: "main"}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(ServerOptions{GraphManager: manager, APIToken: "api-token", APIRequireToken: true})
	body := []byte(`{"ref":"refs/heads/main","after":"abcdef123456","repository":{"full_name":"team/demo"}}`)
	h := hmac.New(sha256.New, []byte("webhook-secret"))
	_, _ = h.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/code/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "delivery-api-test")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(h.Sum(nil)))
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := manager.List()
	if err != nil || len(jobs) != 1 || jobs[0].DeliveryID != "delivery-api-test" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}
