package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type WikiQualityAudit struct {
	ProjectPath                string         `json:"project_path"`
	Ready                      bool           `json:"ready"`
	Pages                      int            `json:"pages"`
	PagesByType                map[string]int `json:"pages_by_type"`
	PageVersions               int            `json:"page_versions"`
	Reviews                    int            `json:"reviews"`
	ReviewTypes                map[string]int `json:"review_types"`
	TaskExecutions             int            `json:"task_executions"`
	MaxSourceAttempts          int            `json:"max_source_attempts"`
	ManifestSources            int            `json:"manifest_sources"`
	SourceSummaries            int            `json:"source_summaries"`
	MissingSourceSummaries     []string       `json:"missing_source_summaries,omitempty"`
	InvalidGenerationContracts []string       `json:"invalid_generation_contracts,omitempty"`
	OverBudgetSources          []string       `json:"over_budget_sources,omitempty"`
	InvalidProvenance          []string       `json:"invalid_provenance,omitempty"`
	NonCanonicalPages          []string       `json:"non_canonical_pages,omitempty"`
	UnownedPages               []string       `json:"unowned_pages,omitempty"`
	IndexMissingPages          []string       `json:"index_missing_pages,omitempty"`
	AliasConflicts             []string       `json:"alias_conflicts,omitempty"`
	InvalidReviewItems         []string       `json:"invalid_review_items,omitempty"`
	LintIssues                 []LintIssue    `json:"lint_issues,omitempty"`
	AggregateStateCurrent      bool           `json:"aggregate_state_current"`
}

func AuditWikiQuality(projectPath string) (WikiQualityAudit, error) {
	audit := WikiQualityAudit{ProjectPath: projectPath, PagesByType: map[string]int{}, ReviewTypes: map[string]int{}}
	manifest, err := loadBootstrapManifest(projectPath)
	if err != nil {
		return audit, err
	}
	audit.ManifestSources = len(manifest)
	allowedSources := map[string]bool{}
	owned := map[string]bool{}
	for key, entry := range manifest {
		allowedSources[filepath.ToSlash(strings.TrimSpace(entry.RawPath))] = true
		for _, path := range entry.Files {
			owned[filepath.ToSlash(path)] = true
		}
		if entry.PipelineVersion >= core.SourceManifestPipelineVersion && (strings.TrimSpace(entry.GenerationContractSHA256) == "" || entry.NewPageBudget <= 0) {
			audit.InvalidGenerationContracts = append(audit.InvalidGenerationContracts, key)
		}
		if entry.NewPageBudget > 0 && entry.NewPageCount > entry.NewPageBudget {
			audit.OverBudgetSources = append(audit.OverBudgetSources, fmt.Sprintf("%s: %d > %d", key, entry.NewPageCount, entry.NewPageBudget))
		}
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "audit"})
	if err != nil {
		return audit, err
	}
	versions, err := wiki.ScanWikiPageVersions(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "audit"})
	if err != nil {
		return audit, err
	}
	audit.PageVersions = len(versions)
	indexData, _ := os.ReadFile(filepath.Join(projectPath, "wiki", "index.md"))
	indexText := string(indexData)
	aliasOwners := map[string]map[string]bool{}
	pageByPath := make(map[string]core.WikiPage, len(pages))
	for _, page := range pages {
		pageByPath[page.Path] = page
		audit.Pages++
		audit.PagesByType[page.Type]++
		if page.Type == "source-summary" {
			audit.SourceSummaries++
		}
		if isAuditAggregate(page.Path) {
			continue
		}
		if !canonicalAuditPath(page.Path, page.Type) {
			audit.NonCanonicalPages = append(audit.NonCanonicalPages, page.Path)
		}
		if page.Type != "synthesis" && !strings.HasPrefix(page.Path, "wiki/code/") && !owned[page.Path] {
			audit.UnownedPages = append(audit.UnownedPages, page.Path)
		}
		for _, source := range page.Sources {
			source = filepath.ToSlash(strings.TrimSpace(source))
			codeEvidence := strings.HasPrefix(page.Path, "wiki/code/") && strings.HasPrefix(source, "raw/code-graphs/")
			if !allowedSources[source] && !codeEvidence {
				audit.InvalidProvenance = append(audit.InvalidProvenance, page.Path+" -> "+source)
			}
		}
		if !strings.Contains(indexText, "(`"+page.Path+"`)") {
			audit.IndexMissingPages = append(audit.IndexMissingPages, page.Path)
		}
		content, _ := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(page.Path)))
		if page.Type == "source-summary" {
			continue
		}
		for _, identity := range append([]string{page.Title}, aliasesFromMarkdown(string(content))...) {
			key := normalizeLinkID(identity)
			if key == "" {
				continue
			}
			if aliasOwners[key] == nil {
				aliasOwners[key] = map[string]bool{}
			}
			aliasOwners[key][page.Path] = true
		}
	}
	for key, entry := range manifest {
		if !entryHasSourceSummary(entry, pageByPath) {
			audit.MissingSourceSummaries = append(audit.MissingSourceSummaries, key)
		}
	}
	for alias, owners := range aliasOwners {
		if len(owners) < 2 {
			continue
		}
		paths := make([]string, 0, len(owners))
		for path := range owners {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		audit.AliasConflicts = append(audit.AliasConflicts, fmt.Sprintf("%s -> %s", alias, strings.Join(paths, ", ")))
	}
	for _, section := range []string{"Sources", "Concepts", "Entities", "Syntheses", "Code"} {
		if !strings.Contains(indexText, "## "+section) {
			audit.IndexMissingPages = append(audit.IndexMissingPages, "missing section: "+section)
		}
	}
	reviews, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "audit"})
	if err != nil {
		return audit, err
	}
	audit.Reviews = len(reviews)
	validReviewTypes := map[string]bool{
		"contradiction": true, "duplicate": true, "missing-page": true,
		"stale-claim": true, "source-gap": true, "review-needed": true,
	}
	validStatuses := map[string]bool{"open": true, "resolved": true, "dismissed": true}
	for _, review := range reviews {
		audit.ReviewTypes[review.Type]++
		if !validReviewTypes[review.Type] || !validStatuses[review.Status] {
			audit.InvalidReviewItems = append(audit.InvalidReviewItems, review.Type+" | "+review.Title+" | "+review.Status)
		}
	}
	var taskState struct {
		Sources map[string]struct {
			Attempts int `json:"attempts"`
		} `json:"sources"`
	}
	if data, readErr := os.ReadFile(filepath.Join(projectPath, ".kbcore", "bootstrap-state.json")); readErr == nil && json.Unmarshal(data, &taskState) == nil {
		for _, source := range taskState.Sources {
			audit.TaskExecutions += source.Attempts
			if source.Attempts > audit.MaxSourceAttempts {
				audit.MaxSourceAttempts = source.Attempts
			}
		}
	}
	audit.LintIssues, err = LintWiki(projectPath)
	if err != nil {
		return audit, err
	}
	audit.AggregateStateCurrent, _ = wiki.AggregateStateCurrent(projectPath)
	sort.Strings(audit.InvalidProvenance)
	sort.Strings(audit.InvalidGenerationContracts)
	sort.Strings(audit.OverBudgetSources)
	sort.Strings(audit.MissingSourceSummaries)
	sort.Strings(audit.NonCanonicalPages)
	sort.Strings(audit.UnownedPages)
	sort.Strings(audit.IndexMissingPages)
	sort.Strings(audit.AliasConflicts)
	sort.Strings(audit.InvalidReviewItems)
	audit.Ready = audit.ManifestSources > 0 && audit.SourceSummaries == audit.ManifestSources && len(audit.MissingSourceSummaries) == 0 && audit.AggregateStateCurrent &&
		len(audit.InvalidGenerationContracts) == 0 && len(audit.OverBudgetSources) == 0 &&
		len(audit.InvalidProvenance) == 0 && len(audit.NonCanonicalPages) == 0 && len(audit.UnownedPages) == 0 &&
		len(audit.IndexMissingPages) == 0 && len(audit.AliasConflicts) == 0 && len(audit.InvalidReviewItems) == 0 && len(audit.LintIssues) == 0
	return audit, nil
}

func ValidateWikiReady(projectPath, sourcePath string) (WikiQualityAudit, error) {
	if _, err := ValidateBootstrapProject(projectPath, sourcePath); err != nil {
		return WikiQualityAudit{}, err
	}
	return ValidateWikiQualityReady(projectPath)
}

func ValidateWikiQualityReady(projectPath string) (WikiQualityAudit, error) {
	audit, err := AuditWikiQuality(projectPath)
	if err != nil {
		return audit, err
	}
	if !audit.Ready {
		return audit, fmt.Errorf("wiki quality gate failed: summaries=%d contracts=%d over_budget=%d provenance=%d noncanonical=%d unowned=%d index=%d aliases=%d reviews=%d lint=%d aggregate_current=%t",
			len(audit.MissingSourceSummaries), len(audit.InvalidGenerationContracts), len(audit.OverBudgetSources), len(audit.InvalidProvenance), len(audit.NonCanonicalPages), len(audit.UnownedPages), len(audit.IndexMissingPages),
			len(audit.AliasConflicts), len(audit.InvalidReviewItems), len(audit.LintIssues), audit.AggregateStateCurrent)
	}
	return audit, nil
}

func isAuditAggregate(path string) bool {
	return path == "wiki/index.md" || path == "wiki/overview.md" || path == "wiki/log.md" || path == "wiki/reviews.md"
}

func canonicalAuditPath(path, pageType string) bool {
	switch pageType {
	case "source-summary":
		return strings.HasPrefix(path, "wiki/sources/")
	case "entity":
		return strings.HasPrefix(path, "wiki/entities/")
	case "concept":
		return strings.HasPrefix(path, "wiki/concepts/")
	case "synthesis":
		return strings.HasPrefix(path, "wiki/syntheses/")
	case "code-overview", "code-module", "code-flow", "code-symbol", "code-impact":
		return strings.HasPrefix(path, "wiki/code/")
	default:
		return false
	}
}

func AuditComparisonMarkdown(baseline, current WikiQualityAudit) string {
	var b strings.Builder
	b.WriteString("# Wiki Quality Comparison\n\n")
	fmt.Fprintf(&b, "| Metric | Baseline | Current |\n|---|---:|---:|\n")
	rows := []struct {
		name     string
		old, new int
	}{
		{"Pages", baseline.Pages, current.Pages},
		{"Page versions", baseline.PageVersions, current.PageVersions},
		{"Reviews", baseline.Reviews, current.Reviews},
		{"Task executions", baseline.TaskExecutions, current.TaskExecutions},
		{"Maximum source attempts", baseline.MaxSourceAttempts, current.MaxSourceAttempts},
		{"Manifest sources", baseline.ManifestSources, current.ManifestSources},
		{"Source summaries", baseline.SourceSummaries, current.SourceSummaries},
		{"Missing source summaries", len(baseline.MissingSourceSummaries), len(current.MissingSourceSummaries)},
		{"Invalid generation contracts", len(baseline.InvalidGenerationContracts), len(current.InvalidGenerationContracts)},
		{"Over-budget sources", len(baseline.OverBudgetSources), len(current.OverBudgetSources)},
		{"Invalid provenance", len(baseline.InvalidProvenance), len(current.InvalidProvenance)},
		{"Non-canonical pages", len(baseline.NonCanonicalPages), len(current.NonCanonicalPages)},
		{"Unowned pages", len(baseline.UnownedPages), len(current.UnownedPages)},
		{"Index gaps", len(baseline.IndexMissingPages), len(current.IndexMissingPages)},
		{"Alias conflicts", len(baseline.AliasConflicts), len(current.AliasConflicts)},
		{"Invalid reviews", len(baseline.InvalidReviewItems), len(current.InvalidReviewItems)},
		{"Lint issues", len(baseline.LintIssues), len(current.LintIssues)},
	}
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", row.name, row.old, row.new)
	}
	fmt.Fprintf(&b, "\n- Baseline ready: `%t`\n- Current ready: `%t`\n", baseline.Ready, current.Ready)
	return b.String()
}
