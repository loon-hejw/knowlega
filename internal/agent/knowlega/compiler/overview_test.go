package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type overviewTestProvider struct{ MockProvider }

func (overviewTestProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	return "# Current Overview\n\nThe wiki connects its durable pages to immutable sources.\n\n" + strings.TrimSpace(input.Purpose), nil
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

type repairingOverviewProvider struct {
	MockProvider
	calls int
}

func (p *repairingOverviewProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	p.calls++
	if p.calls == 1 {
		return "not a complete page", nil
	}
	if input.ValidationError == "" {
		return "", nil
	}
	return "---\ntype: synthesis\ntitle: Overview\nsources: []\n---\n\n# Overview\n\nRecovered global synthesis.\n", nil
}

func TestRefreshOverviewAcceptsFrontmatterAndRepairsInvalidResponse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	provider := &repairingOverviewProvider{}
	written, err := RefreshOverview(provider, root)
	if err != nil {
		t.Fatal(err)
	}
	if !written || provider.calls != 2 {
		t.Fatalf("written=%v calls=%d", written, provider.calls)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "---\n") || !strings.Contains(string(data), "# Overview") {
		t.Fatalf("overview=%s", data)
	}
}

type headinglessRepairOverviewProvider struct {
	MockProvider
	calls int
}

func (p *headinglessRepairOverviewProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	p.calls++
	return "The current wiki records a grounded contradiction and its source evidence.", nil
}

func TestRefreshOverviewCoercesHeadingAfterLLMRepairStillOmitsIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	provider := &headinglessRepairOverviewProvider{}
	written, err := RefreshOverview(provider, root)
	if err != nil {
		t.Fatal(err)
	}
	if !written || provider.calls != 2 {
		t.Fatalf("written=%v calls=%d", written, provider.calls)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# Wiki Overview\n\nThe current wiki records") {
		t.Fatalf("overview=%s", data)
	}
}

func TestOverviewEvidenceIncludesAllSourceSummariesBeforeDurablePages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "chapters"}); err != nil {
		t.Fatal(err)
	}
	manifest := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{}}
	for i := 1; i <= 100; i++ {
		raw := fmt.Sprintf("raw/sources/chapter-%03d.txt", i)
		page := fmt.Sprintf("wiki/sources/chapter-%03d.md", i)
		rawAbs := filepath.Join(root, filepath.FromSlash(raw))
		if err := os.MkdirAll(filepath.Dir(rawAbs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rawAbs, []byte(fmt.Sprintf("chapter %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		pageAbs := filepath.Join(root, filepath.FromSlash(page))
		if err := os.MkdirAll(filepath.Dir(pageAbs), 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("---\ntype: source-summary\ntitle: Chapter %03d\nsources:\n  - %s\n---\n\n# Chapter %03d\n\nEvidence.\n", i, raw, i)
		if err := os.WriteFile(pageAbs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		manifest.Sources[fmt.Sprintf("chapter-%03d", i)] = SourceManifestEntry{RawPath: raw, Files: []string{page}}
	}
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	excerpts, sources := overviewPageEvidence(root)
	if len(sources) != 100 {
		t.Fatalf("sources=%d", len(sources))
	}
	for _, want := range []string{"wiki/sources/chapter-001.md", "wiki/sources/chapter-050.md", "wiki/sources/chapter-100.md"} {
		if !strings.Contains(excerpts, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestNormalizeOverviewDowngradesOnlyUnresolvedLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "wiki", "entities", "sun-wukong.md")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("---\ntype: entity\ntitle: Sun Wukong\n---\n\n# Sun Wukong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeOverviewMarkdown(root, "# Overview\n\n[[sun-wukong|Sun Wukong]] challenges [[missing-heaven|Heaven]].", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalized, "[[sun-wukong|Sun Wukong]]") || strings.Contains(normalized, "[[missing-heaven") || !strings.Contains(normalized, "Heaven") {
		t.Fatalf("normalized=%s", normalized)
	}
}
