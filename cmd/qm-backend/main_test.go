package main

import (
	"net/url"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestWithKnowledgeSearchPathIncludesPublicExtensions(t *testing.T) {
	dsn := "postgres://kbcore:kbcore@127.0.0.1:55433/kbcore?sslmode=disable"
	got := withKnowledgeSearchPath(dsn)

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse knowledge DSN: %v", err)
	}
	if searchPath := parsed.Query().Get("search_path"); searchPath != "knowledge_core,public" {
		t.Fatalf("search_path = %q, want knowledge_core,public", searchPath)
	}
	if sslMode := parsed.Query().Get("sslmode"); sslMode != "disable" {
		t.Fatalf("sslmode = %q, want disable", sslMode)
	}
}

func TestProjectFileWorkerIsIndependentFromGenericKnowledgeScan(t *testing.T) {
	cfg := config.Default()
	cfg.Knowledge.Worker = false
	cfg.Knowledge.AutoProcessProjectFiles = true
	project := data.KnowledgeScope{Kind: "project", ExternalScopeID: "group:web-project-p1"}
	personal := data.KnowledgeScope{Kind: "personal", ExternalScopeID: "personal:alice"}
	if !shouldMaintainKnowledgeScope(cfg, project) || shouldMaintainKnowledgeScope(cfg, personal) {
		t.Fatalf("unexpected default routing: project=%v personal=%v", shouldMaintainKnowledgeScope(cfg, project), shouldMaintainKnowledgeScope(cfg, personal))
	}
	cfg.Knowledge.AutoProcessProjectFiles = false
	cfg.Knowledge.Worker = true
	if shouldMaintainKnowledgeScope(cfg, project) || !shouldMaintainKnowledgeScope(cfg, personal) {
		t.Fatalf("unexpected generic routing: project=%v personal=%v", shouldMaintainKnowledgeScope(cfg, project), shouldMaintainKnowledgeScope(cfg, personal))
	}
}
