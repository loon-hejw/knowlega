package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestAuditWikiQualityRequiresStrictHealthyArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "audit"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nAlpha evidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := compiler.MockProvider{}
	if _, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath: root, SourcePath: source, Provider: provider, MaxFilesPerTask: 12, MaxNewPagesPerSource: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.ConvergeWikiArtifacts(root); err != nil {
		t.Fatal(err)
	}
	if err := wiki.RebuildIndex(root); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.RefreshOverview(provider, root); err != nil {
		t.Fatal(err)
	}
	audit, err := AuditWikiQuality(root)
	if err != nil {
		t.Fatal(err)
	}
	if !audit.Ready {
		t.Fatalf("audit=%+v", audit)
	}

	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		if page.Type != "entity" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(page.Path))
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(page.Sources) == 0 {
			t.Fatal("entity page has no provenance")
		}
		data = []byte(strings.Replace(string(data), page.Sources[0], "raw/sources/fake.txt", 1))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		break
	}
	audit, err = AuditWikiQuality(root)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Ready || audit.AggregateStateCurrent || len(audit.InvalidProvenance) == 0 {
		t.Fatalf("mutated wiki must not remain ready: %+v", audit)
	}
}
