package server

import (
	"net/http"
	"testing"
)

func TestProjectKnowledgeDocumentID(t *testing.T) {
	if got := projectKnowledgeDocumentID("/v1/projects/project-1/knowledge/documents"); got != "project-1" {
		t.Fatalf("id = %q", got)
	}
	for _, path := range []string{"/v1/projects/project-1/knowledge", "/v1/projects//knowledge/documents", "/v1/projects/project-1/files"} {
		if got := projectKnowledgeDocumentID(path); got != "" {
			t.Fatalf("%s id = %q", path, got)
		}
	}
}

func TestKnowledgeDocumentRouteIsGoOwned(t *testing.T) {
	h := &HTTPServer{}
	path := "/v1/projects/project-1/knowledge/documents"
	if !h.owns(http.MethodGet, path) {
		t.Fatal("knowledge document GET must be Go-owned")
	}
}
