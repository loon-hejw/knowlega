package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestValidateLLMWikiPathReportsSourceStagesAndSkips(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var progress []ValidateProgress
	_, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: MockProvider{}, SkipUnchanged: true,
		OnProgress: func(item ValidateProgress) { progress = append(progress, item) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 2} {
		for _, phase := range []string{"analysis", "generation", "persisting", "completed"} {
			if !hasProgress(progress, index, 2, phase) {
				t.Fatalf("missing progress index=%d phase=%s: %+v", index, phase, progress)
			}
		}
	}
	progress = nil
	_, err = ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: MockProvider{}, SkipUnchanged: true,
		OnProgress: func(item ValidateProgress) { progress = append(progress, item) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || progress[0].Phase != "skipped" || progress[1].Phase != "skipped" {
		t.Fatalf("expected two skipped events, got %+v", progress)
	}
}

type phaseConcurrencyProvider struct {
	mu              sync.Mutex
	activeAnalyze   int
	maxAnalyze      int
	activeGenerate  int
	maxGenerate     int
	generatedTitles []string
}

func (p *phaseConcurrencyProvider) Analyze(input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.activeAnalyze++
	if p.activeAnalyze > p.maxAnalyze {
		p.maxAnalyze = p.activeAnalyze
	}
	p.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.activeAnalyze--
	p.mu.Unlock()
	return fmt.Sprintf("## Wiki Plan\n- wiki/sources/%s.md", safeSourceSlug(input)), nil
}

func (p *phaseConcurrencyProvider) Generate(_ string, input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.activeGenerate++
	if p.activeGenerate > p.maxGenerate {
		p.maxGenerate = p.activeGenerate
	}
	p.generatedTitles = append(p.generatedTitles, input.SourceTitle)
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p.mu.Lock()
	p.activeGenerate--
	p.mu.Unlock()
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestValidateLLMWikiPathRunsAnalysisConcurrentlyAndGenerationSerially(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &phaseConcurrencyProvider{}
	result, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 4 || provider.maxAnalyze < 2 || provider.maxGenerate != 1 {
		t.Fatalf("result=%+v maxAnalyze=%d maxGenerate=%d", result, provider.maxAnalyze, provider.maxGenerate)
	}
	want := append([]string(nil), provider.generatedTitles...)
	sort.Strings(want)
	if fmt.Sprint(provider.generatedTitles) != fmt.Sprint(want) {
		t.Fatalf("generation order=%v want sorted=%v", provider.generatedTitles, want)
	}
}

func hasProgress(items []ValidateProgress, index, total int, phase string) bool {
	for _, item := range items {
		if item.Index == index && item.Total == total && item.Phase == phase {
			return true
		}
	}
	return false
}
