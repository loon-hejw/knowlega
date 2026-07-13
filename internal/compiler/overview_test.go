package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

type overviewTestProvider struct{ MockProvider }

func (overviewTestProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	return "# Current Overview\n\nThe wiki connects [[alpha]] to its source.\n\n" + strings.TrimSpace(input.Purpose), nil
}

func TestRefreshOverviewWritesVersionedGlobalSynthesis(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "overview.md"), []byte("# Old Overview\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	written, err := RefreshOverview(overviewTestProvider{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("expected overview write")
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# Current Overview") {
		t.Fatalf("overview=%s", data)
	}
	versions, err := wiki.ScanWikiPageVersions(wiki.ScanOptions{ProjectPath: root, ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions=%d", len(versions))
	}
}
