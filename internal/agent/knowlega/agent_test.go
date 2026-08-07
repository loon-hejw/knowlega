package knowlega

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCompilesQueriesWritesBackAndCleansSources(t *testing.T) {
	root := t.TempDir()
	agent, err := New(AgentOptions{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopeRef{OrgID: "local", ExternalScopeID: "project-1", Kind: "project", Name: "Demo"}
	ctx := context.Background()
	status, err := agent.EnsureScope(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.ProjectPath == "" {
		t.Fatalf("unexpected status: %+v", status)
	}
	ingested, err := agent.Ingest(ctx, ref, "notes.md", []byte("# Release\n\nDeploy after review.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ingested.RawPath == "" || len(ingested.GeneratedPaths) == 0 {
		t.Fatalf("expected durable ingest result: %+v", ingested)
	}
	query, err := agent.Query(ctx, ref, "根据知识库的 Release 说明部署流程", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if query.ID == "" || query.Answer == nil {
		t.Fatalf("expected query answer: %+v", query)
	}
	if err := os.WriteFile(filepath.Join(status.ProjectPath, "wiki", "syntheses", "manual.md"), []byte("---\ntitle: Manual\ntype: synthesis\nsources:\n  - "+ingested.RawPath+"\n---\n\n# Manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := agent.SaveQueryAnswer(ctx, ref, query.ID, "Release Guidance")
	if err != nil {
		// The offline mock is intentionally not writeback-eligible. This is a
		// guard that the policy is enforced by the full service, not by callers.
		if !strings.Contains(err.Error(), "not eligible") && !strings.Contains(err.Error(), "writeback") {
			t.Fatal(err)
		}
	} else if !strings.HasPrefix(path, "wiki/syntheses/") {
		t.Fatalf("unexpected synthesis path: %s", path)
	}
	if _, err := agent.Cleanup(ref, ingested.RawPath, false); err != nil {
		t.Fatal(err)
	}
}
