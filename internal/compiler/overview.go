package compiler

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/projectlock"
	"github.com/hejw/knowledge-core/internal/promptbudget"
	"github.com/hejw/knowledge-core/internal/wiki"
	"gopkg.in/yaml.v3"
)

type OverviewInput struct {
	Purpose         string
	Schema          string
	Index           string
	CurrentOverview string
	PageExcerpts    string
	OpenReviews     string
	ValidationError string
}

type OverviewProvider interface {
	SynthesizeOverview(OverviewInput) (string, error)
}

func RefreshOverview(provider Provider, projectPath string) (bool, error) {
	selected, ok := provider.(OverviewProvider)
	if !ok {
		return false, nil
	}
	projectLock, err := projectlock.Acquire(projectPath)
	if err != nil {
		return false, err
	}
	defer projectLock.Release()
	excerpts, evidenceSources := overviewPageEvidence(projectPath)
	input := OverviewInput{
		Purpose:         readOptional(filepath.Join(projectPath, "purpose.md")),
		Schema:          readOptional(filepath.Join(projectPath, "schema.md")),
		Index:           readOptional(filepath.Join(projectPath, "wiki", "index.md")),
		CurrentOverview: readOptional(filepath.Join(projectPath, "wiki", "overview.md")),
		PageExcerpts:    excerpts,
		OpenReviews:     readOptional(filepath.Join(projectPath, "wiki", "reviews.md")),
	}
	content, err := selected.SynthesizeOverview(input)
	if err != nil {
		return false, err
	}
	content, validationErr := normalizeOverviewMarkdown(projectPath, content, evidenceSources)
	if validationErr != nil {
		input.ValidationError = "The previous response was invalid: " + validationErr.Error() + ". Return the full corrected page without code fences and use only resolvable [[wikilinks]]."
		repairedResponse, repairErr := selected.SynthesizeOverview(input)
		if repairErr != nil {
			return false, fmt.Errorf("overview synthesis repair: %w", repairErr)
		}
		content, validationErr = normalizeOverviewMarkdown(projectPath, repairedResponse, evidenceSources)
		if validationErr != nil {
			content = coerceOverviewMarkdownShape(repairedResponse)
			content, validationErr = normalizeOverviewMarkdown(projectPath, content, evidenceSources)
			if validationErr != nil {
				return false, fmt.Errorf("overview synthesis repair returned invalid markdown: %w", validationErr)
			}
		}
	}
	if err := wiki.WriteVersionedPage(projectPath, "wiki/overview.md", []byte(content+"\n"), "batch ingest: refresh global overview"); err != nil {
		return false, err
	}
	if err := wiki.SaveAggregateState(projectPath); err != nil {
		return false, err
	}
	return true, nil
}

func coerceOverviewMarkdownShape(response string) string {
	content := cleanOverviewResponse(response)
	if strings.TrimSpace(content) == "" || validOverviewMarkdown(content) {
		return content
	}
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			return strings.TrimSpace(strings.Join(lines[index:], "\n"))
		}
	}
	// The LLM supplied synthesis prose but omitted the page heading. Adding a
	// fixed H1 repairs only the Markdown envelope; it does not invent or rewrite
	// any evidence claim.
	return "# Wiki Overview\n\n" + strings.TrimSpace(content)
}

func cleanOverviewResponse(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```markdown") {
		content = strings.TrimSpace(strings.TrimPrefix(content, "```markdown"))
	} else if strings.HasPrefix(content, "```") {
		content = strings.TrimSpace(strings.TrimPrefix(content, "```"))
	}
	content = strings.TrimSpace(strings.TrimSuffix(content, "```"))
	return content
}

func validOverviewMarkdown(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	if strings.HasPrefix(content, "---\n") {
		rest := strings.TrimPrefix(content, "---\n")
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			return false
		}
		content = strings.TrimSpace(rest[end+len("\n---\n"):])
	}
	return strings.HasPrefix(content, "# ")
}

func normalizeOverviewMarkdown(projectPath, response string, evidenceSources []string) (string, error) {
	content := cleanOverviewResponse(response)
	if !validOverviewMarkdown(content) {
		return "", fmt.Errorf("expected optional YAML frontmatter followed by one H1 heading")
	}
	frontmatter := map[string]any{}
	body := content
	if strings.HasPrefix(strings.TrimSpace(content), "---\n") {
		parsed, parsedBody, err := splitGeneratedFrontmatter(content)
		if err != nil {
			return "", err
		}
		frontmatter = parsed
		body = parsedBody
	}
	frontmatter["type"] = "synthesis"
	if strings.TrimSpace(fmt.Sprint(frontmatter["title"])) == "" {
		frontmatter["title"] = "Wiki Overview"
	}
	frontmatter["aliases"] = []string{"Overview"}
	frontmatter["sources"] = append([]string(nil), evidenceSources...)
	frontmatter["confidence"] = "INFERRED"
	encoded, err := yaml.Marshal(frontmatter)
	if err != nil {
		return "", err
	}
	normalized := "---\n" + strings.TrimSpace(string(encoded)) + "\n---\n\n" + strings.TrimSpace(body) + "\n"
	blocks := ParsedBlocks{Files: []FileBlock{{Path: "wiki/overview.md", Content: normalized}}}
	// The global synthesis must not remain permanently blocked because the LLM
	// remembered an alias or page that convergence intentionally removed. Keep
	// the readable display text and deterministically downgrade only unresolved
	// links; valid navigation links remain intact.
	downgradeMissingWikilinks(projectPath, &blocks)
	return strings.TrimSpace(blocks.Files[0].Content), nil
}

func overviewPageExcerpts(projectPath string) string {
	excerpts, _ := overviewPageEvidence(projectPath)
	return excerpts
}

func overviewPageEvidence(projectPath string) (string, []string) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return "", nil
	}
	allowed, _ := manifestRawSourceAllowlist(projectPath)
	var summaries, durable []core.WikiPage
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/log.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/reviews.md" {
			continue
		}
		if page.Type == "source-summary" {
			summaries = append(summaries, page)
		} else {
			durable = append(durable, page)
		}
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Path < summaries[j].Path })
	sort.Slice(durable, func(i, j int) bool {
		if len(durable[i].Sources) == len(durable[j].Sources) {
			return durable[i].Path < durable[j].Path
		}
		return len(durable[i].Sources) > len(durable[j].Sources)
	})
	var b strings.Builder
	sourceSet := map[string]bool{}
	addSources := func(page core.WikiPage) {
		for _, source := range page.Sources {
			source = filepath.ToSlash(strings.TrimSpace(source))
			if allowed[source] {
				sourceSet[source] = true
			}
		}
	}
	appendPage := func(page core.WikiPage, bodyLimit int) bool {
		if b.Len() >= 40000 {
			return false
		}
		fmt.Fprintf(&b, "\n## %s | %s | %s\nSources: %s\n%s\n", page.Path, page.Type, page.Title, strings.Join(page.Sources, ", "), promptbudget.TrimEnd(page.Body, bodyLimit))
		addSources(page)
		return true
	}
	perSummary := 1200
	if len(summaries) > 0 && 24000/len(summaries) < perSummary {
		perSummary = 24000 / len(summaries)
	}
	if perSummary < 240 {
		perSummary = 240
	}
	for _, page := range summaries {
		body := promptbudget.TrimEnd(page.Body, perSummary)
		if len([]rune(page.Body)) > len([]rune(body)) {
			body += "\n[EXCERPT TRUNCATED]"
		}
		fmt.Fprintf(&b, "\n## %s | %s\n%s\n", page.Path, page.Title, body)
		addSources(page)
	}
	for _, page := range durable {
		if !appendPage(page, 400) {
			break
		}
	}
	sources := make([]string, 0, len(sourceSet))
	for source := range sourceSet {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return b.String(), sources
}
