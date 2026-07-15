package compiler

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

type pageGateTestProvider struct {
	decision NewPageGateDecision
	before   func()
}

func (p pageGateTestProvider) Analyze(AnalysisInput) (string, error) {
	return "", errors.New("unexpected Analyze call")
}

func (p pageGateTestProvider) Generate(string, AnalysisInput) (string, error) {
	return "", errors.New("unexpected Generate call")
}

func (p pageGateTestProvider) GateNewPages(NewPageGateInput) (NewPageGateDecision, error) {
	if p.before != nil {
		p.before()
	}
	return p.decision, nil
}

func TestApplyNewPageGateIgnoresDecisionForUnsubmittedSourceSummary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "page-gate"}); err != nil {
		t.Fatal(err)
	}
	const sourceRel = "raw/sources/chapter-056/original/chapter-056.txt"
	const summaryPath = "wiki/sources/chapter-056.md"
	const candidatePath = "wiki/entities/lasting-character.md"
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: summaryPath, Content: generatedPage("source-summary", "第五十六回", sourceRel)},
		{Path: candidatePath, Content: generatedPage("entity", "持久人物", sourceRel)},
	}}
	provider := pageGateTestProvider{decision: NewPageGateDecision{
		Accepted: []string{candidatePath},
		Rejected: []NewPageRejection{
			{Path: summaryPath, Reason: "model must not decide about the summary"},
			{Path: "wiki/concepts/not-submitted.md", Reason: "not submitted"},
		},
	}}
	work := &analyzedSource{
		title:  "第五十六回",
		rawRel: sourceRel,
		input: AnalysisInput{
			SourceText: "source evidence",
			GenerationPolicy: GenerationPolicy{
				MaxFileBlocks:        4,
				MaxNewPagesPerSource: 3,
				RemainingNewPages:    3,
			},
		},
	}
	got, err := applyNewPageGate(ValidateOptions{ProjectPath: root, Provider: provider}, work, blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 2 || got.Files[0].Path != summaryPath || got.Files[1].Path != candidatePath {
		t.Fatalf("gate changed files outside its candidate authority: %+v", got.Files)
	}
}

func TestApplyNewPageGateConservativelyDropsContradictoryCandidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "page-gate"}); err != nil {
		t.Fatal(err)
	}
	const sourceRel = "raw/sources/chapter-001/original/chapter-001.txt"
	const summaryPath = "wiki/sources/chapter-001.md"
	const candidatePath = "wiki/entities/stone-monkey.md"
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: summaryPath, Content: generatedPage("source-summary", "第一回", sourceRel)},
		{Path: candidatePath, Content: generatedPage("entity", "石猴", sourceRel)},
	}}
	provider := pageGateTestProvider{decision: NewPageGateDecision{
		Accepted: []string{candidatePath},
		Rejected: []NewPageRejection{{Path: candidatePath, Reason: "contradiction"}},
	}}
	work := &analyzedSource{
		title:  "第一回",
		rawRel: sourceRel,
		input: AnalysisInput{GenerationPolicy: GenerationPolicy{
			MaxFileBlocks:        4,
			MaxNewPagesPerSource: 3,
			RemainingNewPages:    3,
		}},
	}
	got, err := applyNewPageGate(ValidateOptions{ProjectPath: root, Provider: provider}, work, blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != summaryPath {
		t.Fatalf("files=%+v", got.Files)
	}
	if len(got.Reviews) != 1 || !strings.Contains(got.Reviews[0].Body, "rejection won conservatively") {
		t.Fatalf("reviews=%+v", got.Reviews)
	}
}

func TestApplyNewPageGateLeavesNewlyAppearedPageForSnapshotConflictCheck(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "page-gate"}); err != nil {
		t.Fatal(err)
	}
	const sourceRel = "raw/sources/chapter-085/original/chapter-085.txt"
	const summaryPath = "wiki/sources/chapter-085.md"
	const candidatePath = "wiki/entities/miefa-kingdom.md"
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: summaryPath, Content: generatedPage("source-summary", "第八十五回", sourceRel)},
		{Path: candidatePath, Content: generatedPage("entity", "灭法国", sourceRel)},
	}}
	provider := pageGateTestProvider{
		decision: NewPageGateDecision{Accepted: []string{candidatePath}},
		before: func() {
			abs := filepath.Join(root, filepath.FromSlash(candidatePath))
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, []byte(generatedPage("entity", "灭法国", "raw/sources/other.txt")), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	work := &analyzedSource{
		title:  "第八十五回",
		rawRel: sourceRel,
		input: AnalysisInput{GenerationPolicy: GenerationPolicy{
			MaxFileBlocks:        4,
			MaxNewPagesPerSource: 3,
			RemainingNewPages:    3,
		}},
	}
	got, err := applyNewPageGate(ValidateOptions{ProjectPath: root, Provider: provider}, work, blocks)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts := staleGeneratedPaths(root, map[string]string{}, nil, got.Files); len(conflicts) != 1 || conflicts[0] != candidatePath {
		t.Fatalf("stale conflicts=%v", conflicts)
	}
}

func generatedPage(pageType, title, sourceRel string) string {
	return "---\n" +
		"type: " + pageType + "\n" +
		"title: " + title + "\n" +
		"sources:\n" +
		"  - " + sourceRel + "\n" +
		"---\n\n" + title + "\n"
}
