package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestBuildWikiGraphInsightsFindsIsolatedAndMissingSources(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "graph"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "isolated.md"), []byte(`---
type: "concept"
title: "Isolated"
---

# Isolated
`), 0o644); err != nil {
		t.Fatal(err)
	}
	insights, err := BuildWikiGraphInsights(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights.IsolatedPages) == 0 {
		t.Fatalf("expected isolated insight: %+v", insights)
	}
	if len(insights.MissingSources) == 0 {
		t.Fatalf("expected missing source insight: %+v", insights)
	}
}
