package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

type budgetRepairProvider struct {
	calls          int
	repairAnalysis string
}

type contractRecoveryProvider struct {
	calls int
}

func (p *contractRecoveryProvider) Analyze(AnalysisInput) (string, error) {
	return "## Wiki Plan\n- source summary\n- durable pages", nil
}

func (p *contractRecoveryProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	p.calls++
	if strings.Contains(analysis, "GENERATION CONTRACT RECOVERY REQUIRED") {
		return generatedPolicyBlocks(input.SourceRel, true, 3), nil
	}
	if strings.Contains(analysis, "SOURCE SUMMARY RECOVERY REQUIRED") {
		return fmt.Sprintf("---FILE: wiki/sources/recovered.md\n%s\n", generatedPolicyPage("source-summary", "Recovered", input.SourceRel)), nil
	}
	if p.calls == 1 {
		return "Here is the requested wiki update without block markers.", nil
	}
	return generatedPolicyBlocks(input.SourceRel, false, 4), nil
}

func generatedPolicyBlocks(source string, includeSummary bool, newPages int) string {
	var out strings.Builder
	if includeSummary {
		fmt.Fprintf(&out, "---FILE: wiki/sources/a.md\n%s\n", generatedPolicyPage("source-summary", "A", source))
	}
	for index := 0; index < newPages; index++ {
		fmt.Fprintf(&out, "\n---FILE: wiki/entities/recovery-%d.md\n%s\n", index, generatedPolicyPage("entity", fmt.Sprintf("Recovery %d", index), source))
	}
	return out.String()
}

func (p *budgetRepairProvider) Analyze(AnalysisInput) (string, error) {
	return "## Wiki Plan\n- source summary\n- durable pages", nil
}

func (p *budgetRepairProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	p.calls++
	count := 4
	if p.calls == 2 {
		p.repairAnalysis = analysis
		count = 3
	}
	var out strings.Builder
	fmt.Fprintf(&out, "---FILE: wiki/sources/a.md\n%s\n", generatedPolicyPage("source-summary", "A", input.SourceRel))
	for index := 0; index < count; index++ {
		fmt.Fprintf(&out, "\n---FILE: wiki/entities/entity-%d.md\n%s\n", index, generatedPolicyPage("entity", fmt.Sprintf("Entity %d", index), input.SourceRel))
	}
	return out.String(), nil
}

func TestGenerationPolicyTracksLifetimeBudgetAndContract(t *testing.T) {
	opts := ValidateOptions{MaxFilesPerTask: 12, MaxNewPagesPerSource: 3}
	purpose := "durable purpose"
	schema := "durable schema"
	policy, contract := generationPolicyFor(opts, SourceManifestEntry{}, false, "source-sha", purpose, schema)
	if policy.MaxFileBlocks != 12 || policy.RemainingNewPages != 3 || contract == "" {
		t.Fatalf("policy=%+v contract=%q", policy, contract)
	}
	entry := SourceManifestEntry{SHA256: "source-sha", GenerationContractSHA256: contract, NewPageCount: 2}
	policy, sameContract := generationPolicyFor(opts, entry, true, "source-sha", purpose, schema)
	if sameContract != contract || policy.RemainingNewPages != 1 {
		t.Fatalf("policy=%+v same_contract=%q", policy, sameContract)
	}
	opts.UpdateOnly = true
	policy, _ = generationPolicyFor(opts, entry, true, "source-sha", purpose, schema)
	if !policy.UpdateOnly || policy.RemainingNewPages != 0 {
		t.Fatalf("update-only policy=%+v", policy)
	}
	opts.UpdateOnly = false
	policy, changedContract := generationPolicyFor(opts, entry, true, "source-sha", purpose+" changed", schema)
	if changedContract == contract || policy.RemainingNewPages != 3 {
		t.Fatalf("changed policy=%+v contract=%q", policy, changedContract)
	}
}

func TestGenerationPolicyTextContainsEveryHardConstraint(t *testing.T) {
	text := generationPolicyText(GenerationPolicy{MaxFileBlocks: 9, MaxNewPagesPerSource: 3, RemainingNewPages: 1})
	for _, want := range []string{"at most 9 total", "exactly one source-summary", "at most 1 new non-summary", "primary language of the current source"} {
		if !strings.Contains(text, want) {
			t.Fatalf("policy text missing %q:\n%s", want, text)
		}
	}
	updateOnly := generationPolicyText(GenerationPolicy{MaxFileBlocks: 12, MaxNewPagesPerSource: 3, RemainingNewPages: 0, UpdateOnly: true})
	if !strings.Contains(updateOnly, "must not create any new non-summary page") {
		t.Fatalf("update-only text=%s", updateOnly)
	}
}

func TestEffectiveGenerationPolicyCapsFirstSourceAtSummaryPlusNewPages(t *testing.T) {
	base := GenerationPolicy{MaxFileBlocks: 12, MaxNewPagesPerSource: 3, RemainingNewPages: 3}
	if got := effectiveGenerationPolicy(base, 0); got.MaxFileBlocks != 4 {
		t.Fatalf("first-source policy=%+v", got)
	}
	if got := effectiveGenerationPolicy(base, 2); got.MaxFileBlocks != 6 {
		t.Fatalf("two-update policy=%+v", got)
	}
	if got := effectiveGenerationPolicy(base, 20); got.MaxFileBlocks != 12 {
		t.Fatalf("configured cap policy=%+v", got)
	}
}

func TestValidateGeneratedBlocksUsesRemainingLifetimeBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := "raw/sources/a.txt"
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: "wiki/sources/a.md", Content: generatedPolicyPage("source-summary", "A", source)},
		{Path: "wiki/entities/new.md", Content: generatedPolicyPage("entity", "New", source)},
	}}
	policy := GenerationPolicy{MaxFileBlocks: 12, MaxNewPagesPerSource: 3, RemainingNewPages: 0, UpdateOnly: true}
	err := validateGeneratedBlocksForPolicy(ValidateOptions{ProjectPath: root}, blocks, source, policy)
	if err == nil || !strings.Contains(err.Error(), "maximum per source is 0") {
		t.Fatalf("err=%v", err)
	}
}

func TestGenerationRepairRestatesBudgetAndPersistsLifetimeLedger(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(source, []byte("# A\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &budgetRepairProvider{}
	result, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root, SourcePath: source, Provider: provider,
		MaxFilesPerTask: 12, MaxNewPagesPerSource: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 || len(result.Files) != 4 {
		t.Fatalf("calls=%d result=%+v", provider.calls, result)
	}
	for _, want := range []string{"at most 4 total ---FILE blocks", "at most 3 new non-summary pages", "Drop the least durable proposed new pages"} {
		if !strings.Contains(provider.repairAnalysis, want) {
			t.Fatalf("repair analysis missing %q:\n%s", want, provider.repairAnalysis)
		}
	}
	manifest, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := manifest.Sources[sourceManifestKey(source)]
	if entry.NewPageBudget != 3 || entry.NewPageCount != 3 || len(entry.CreatedPages) != 3 || entry.GenerationContractSHA256 == "" {
		t.Fatalf("manifest entry=%+v", entry)
	}
}

func TestGenerationContractRecoveryHandlesMalformedOverBudgetRepair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(source, []byte("# A\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &contractRecoveryProvider{}
	var phases []string
	result, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root, SourcePath: source, Provider: provider,
		MaxFilesPerTask: 12, MaxNewPagesPerSource: 3,
		OnProgress: func(progress ValidateProgress) { phases = append(phases, progress.Phase) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 4 || len(result.Files) != 4 || !containsString(phases, "generation_contract_repair") {
		t.Fatalf("calls=%d files=%v phases=%v", provider.calls, result.Files, phases)
	}
}

func TestPurposeChangeInvalidatesUnchangedSourceManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(source, []byte("# A\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Provider: MockProvider{}}); err != nil {
		t.Fatal(err)
	}
	before, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	beforeContract := before.Sources[sourceManifestKey(source)].GenerationContractSHA256
	if err := os.WriteFile(filepath.Join(root, "purpose.md"), []byte("# Changed\n\nUse a materially changed project purpose.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &recoveryCountingProvider{}
	result, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Provider: provider, SkipUnchanged: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	afterContract := after.Sources[sourceManifestKey(source)].GenerationContractSHA256
	if result.Skipped || provider.calls != 2 || beforeContract == "" || afterContract == beforeContract {
		t.Fatalf("result=%+v calls=%d before=%q after=%q", result, provider.calls, beforeContract, afterContract)
	}
}

func generatedPolicyPage(pageType, title, source string) string {
	return fmt.Sprintf("---\ntype: %q\ntitle: %q\nsources:\n  - %q\nconfidence: EXTRACTED\n---\n\n# %s\n", pageType, title, source, title)
}
