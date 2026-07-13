package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/llmretry"
	"github.com/hejw/knowledge-core/internal/promptbudget"
)

type WikiReviewOptions struct {
	ProjectPath string
	Agent       WikiReviewAgent
}

type WikiReviewAgent interface {
	ReviewWiki(WikiReviewInput) ([]LintIssue, error)
}

type WikiReviewInput struct {
	Purpose        string
	Schema         string
	SourceManifest string
	Index          string
	Overview       string
	Reviews        string
	Sources        []WikiReviewSource
	Pages          []WikiReviewPage
}

type WikiReviewSource struct {
	RawPath        string   `json:"raw_path"`
	OriginalPath   string   `json:"original_path"`
	Title          string   `json:"title"`
	Files          []string `json:"files"`
	ReviewCount    int      `json:"review_count"`
	BodyRunes      int      `json:"body_runes,omitempty"`
	Excerpt        string   `json:"excerpt,omitempty"`
	ExcerptOmitted bool     `json:"excerpt_omitted_due_to_budget,omitempty"`
}

type WikiReviewPage struct {
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
	Aliases        []string `json:"aliases,omitempty"`
	BodyRunes      int      `json:"body_runes"`
	Excerpt        string   `json:"excerpt,omitempty"`
	ExcerptOmitted bool     `json:"excerpt_omitted_due_to_budget,omitempty"`
}

type OpenAICompatibleWikiReviewAgent struct {
	Protocol         string
	BaseURL          string
	APIKey           string
	Model            string
	UserAgent        string
	AnthropicVersion string
	Client           *http.Client
	MaxInputChars    int
	MaxOutputTokens  int
	DisableThinking  bool
	RetryOptions     llmretry.Options
}

func NewWikiReviewAgent(cfg config.LLMConfig) (WikiReviewAgent, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("llm.api_key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm.model is required")
	}
	return OpenAICompatibleWikiReviewAgent{
		Protocol:         cfg.Protocol,
		BaseURL:          strings.TrimRight(cfg.BaseURL, "/"),
		APIKey:           cfg.APIKey,
		Model:            cfg.Model,
		UserAgent:        cfg.UserAgent,
		AnthropicVersion: cfg.AnthropicVersion,
		Client:           &http.Client{Timeout: cfg.Timeout.Duration},
		MaxInputChars:    cfg.MaxInputChars,
		MaxOutputTokens:  cfg.MaxOutputTokens,
		DisableThinking:  cfg.DisableThinking,
		RetryOptions: llmretry.Options{
			Retries:   cfg.Retries,
			BaseDelay: cfg.RetryBaseDelay.Duration,
			MaxDelay:  cfg.RetryMaxDelay.Duration,
		},
	}, nil
}

func ReviewWiki(opts WikiReviewOptions) ([]LintIssue, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	if opts.Agent == nil {
		return nil, fmt.Errorf("wiki review agent is required")
	}
	input, err := wikiReviewInput(opts.ProjectPath)
	if err != nil {
		return nil, err
	}
	issues, err := opts.Agent.ReviewWiki(input)
	if err != nil {
		return nil, err
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Type == issues[j].Type {
			return issues[i].Path < issues[j].Path
		}
		return issues[i].Type < issues[j].Type
	})
	return issues, nil
}

func wikiReviewInput(projectPath string) (WikiReviewInput, error) {
	pages, err := loadWikiPages(projectPath)
	if err != nil {
		return WikiReviewInput{}, err
	}
	keys := make([]string, 0, len(pages))
	for key := range pages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	reviewPages := make([]WikiReviewPage, 0, len(keys))
	for _, key := range keys {
		page := pages[key]
		body := markdownBody(page.Content)
		reviewPages = append(reviewPages, WikiReviewPage{
			Path:      page.RelPath,
			Title:     titleForSearchResult(page.Content, filepath.Base(page.RelPath), "wiki-page"),
			Type:      wikiPageType(page.Content),
			Aliases:   aliasesFromMarkdown(page.Content),
			BodyRunes: len([]rune(strings.TrimSpace(body))),
			Excerpt:   tailRunes(body, 1600),
		})
	}
	sources, err := wikiReviewSources(projectPath)
	if err != nil {
		return WikiReviewInput{}, err
	}
	return WikiReviewInput{
		Purpose:        readOptionalProjectText(projectPath, "purpose.md"),
		Schema:         readOptionalProjectText(projectPath, "schema.md"),
		SourceManifest: readOptionalProjectText(projectPath, filepath.Join(".kbcore", "source-manifest.json")),
		Index:          readOptionalProjectText(projectPath, filepath.Join("wiki", "index.md")),
		Overview:       readOptionalProjectText(projectPath, filepath.Join("wiki", "overview.md")),
		Reviews:        readOptionalProjectText(projectPath, filepath.Join("wiki", "reviews.md")),
		Sources:        sources,
		Pages:          reviewPages,
	}, nil
}

func wikiReviewSources(projectPath string) ([]WikiReviewSource, error) {
	entries, err := loadSourceManifestEntries(projectPath, "wiki-review")
	if err != nil {
		return nil, err
	}
	sources := make([]WikiReviewSource, 0, len(entries))
	for _, entry := range entries {
		excerpt := ""
		bodyRunes := 0
		if strings.TrimSpace(entry.RawPath) != "" {
			raw := readOptionalProjectText(projectPath, entry.RawPath)
			bodyRunes = len([]rune(strings.TrimSpace(raw)))
			excerpt = tailRunes(raw, 1200)
		}
		sources = append(sources, WikiReviewSource{
			RawPath:      entry.RawPath,
			OriginalPath: entry.OriginalPath,
			Title:        entry.Title,
			Files:        append([]string(nil), entry.Files...),
			ReviewCount:  entry.ReviewCount,
			BodyRunes:    bodyRunes,
			Excerpt:      excerpt,
		})
	}
	return sources, nil
}

func readOptionalProjectText(projectPath, rel string) string {
	content, err := readProjectText(projectPath, rel)
	if err != nil {
		return ""
	}
	return content
}

func (a OpenAICompatibleWikiReviewAgent) ReviewWiki(input WikiReviewInput) ([]LintIssue, error) {
	input = budgetWikiReviewInput(input)
	sourcesJSON, _ := json.MarshalIndent(input.Sources, "", "  ")
	pagesJSON, _ := json.MarshalIndent(input.Pages, "", "  ")
	system := `You review a persistent LLM Wiki for maintenance issues.
Return only JSON with this shape:
{"issues":[{"type":"contradiction|duplicate|missing-page|stale-claim|source-gap|review-needed","path":"wiki/...","detail":"specific actionable issue"}]}

Rules:
- Focus on semantic wiki maintenance, not markdown formatting.
- Report contradictions, duplicate pages, missing concept/entity/synthesis pages, stale or weakly sourced claims, and source gaps.
- Pay special attention to concrete unresolved review items in wiki/reviews.md, raw source excerpts, and source manifest entries that conflict with generated wiki pages.
- Treat page aliases as valid names for their page; do not report an alias as a missing page when it is listed on the target page.
- Do not report missing content just because excerpt is absent or excerpt_omitted_due_to_budget is true; use body_runes to distinguish omitted prompt context from empty files.
- Do not report a broad review-needed issue solely because many source_manifest entries have review_count > 0; report specific actionable unresolved review items instead.
- Use project-relative wiki paths when possible.
- Do not invent issues; return {"issues":[]} when the wiki looks healthy.`
	user := fmt.Sprintf(`Purpose:
%s

Schema:
%s

Source Manifest:
%s

Index:
%s

Overview:
%s

Open Review Items:
%s

Raw Source Excerpts:
%s

Pages:
%s`, input.Purpose, input.Schema, input.SourceManifest, input.Index, input.Overview, input.Reviews, string(sourcesJSON), string(pagesJSON))
	content, err := a.chat(system, user)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Issues []LintIssue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &parsed); err != nil {
		return nil, fmt.Errorf("parse llm wiki review: %w: %s", err, content)
	}
	return sanitizeReviewIssues(parsed.Issues), nil
}

func (a OpenAICompatibleWikiReviewAgent) chat(system, user string) (string, error) {
	queryAgent := OpenAICompatibleQueryAgent{
		Protocol:         a.Protocol,
		BaseURL:          a.BaseURL,
		APIKey:           a.APIKey,
		Model:            a.Model,
		UserAgent:        a.UserAgent,
		AnthropicVersion: a.AnthropicVersion,
		Client:           a.Client,
		MaxInputChars:    a.MaxInputChars,
		MaxOutputTokens:  a.MaxOutputTokens,
		DisableThinking:  a.DisableThinking,
		RetryOptions:     a.RetryOptions,
	}
	return queryAgent.chat(system, user)
}

func budgetWikiReviewInput(input WikiReviewInput) WikiReviewInput {
	input.Purpose = promptbudget.TrimEnd(input.Purpose, 6000)
	input.Schema = promptbudget.TrimEnd(input.Schema, 6000)
	input.SourceManifest = promptbudget.TrimMiddle(input.SourceManifest, 20000)
	input.Index = promptbudget.TrimMiddle(input.Index, 24000)
	input.Overview = promptbudget.TrimMiddle(input.Overview, 16000)
	input.Reviews = promptbudget.TrimMiddle(input.Reviews, 24000)
	input.Sources = budgetWikiReviewSources(input.Sources, 18000)
	input.Pages = budgetWikiReviewPages(input.Pages, 52000)
	return input
}

func budgetWikiReviewSources(sources []WikiReviewSource, totalExcerptRunes int) []WikiReviewSource {
	out := make([]WikiReviewSource, 0, len(sources))
	remaining := totalExcerptRunes
	for _, source := range sources {
		maxRunes := remaining
		if maxRunes > 600 {
			maxRunes = 600
		}
		if maxRunes < 0 {
			maxRunes = 0
		}
		original := strings.TrimSpace(source.Excerpt)
		source.Excerpt = promptbudget.TrimMiddle(original, maxRunes)
		if original != "" && source.Excerpt == "" {
			source.ExcerptOmitted = true
		}
		remaining -= len([]rune(source.Excerpt))
		out = append(out, source)
	}
	return out
}

func budgetWikiReviewPages(pages []WikiReviewPage, totalExcerptRunes int) []WikiReviewPage {
	out := make([]WikiReviewPage, 0, len(pages))
	remaining := totalExcerptRunes
	for _, page := range pages {
		maxRunes := remaining
		if maxRunes > 600 {
			maxRunes = 600
		}
		if maxRunes < 0 {
			maxRunes = 0
		}
		original := strings.TrimSpace(page.Excerpt)
		page.Excerpt = promptbudget.TrimMiddle(original, maxRunes)
		if original != "" && page.Excerpt == "" {
			page.ExcerptOmitted = true
		}
		remaining -= len([]rune(page.Excerpt))
		out = append(out, page)
	}
	return out
}

func sanitizeReviewIssues(issues []LintIssue) []LintIssue {
	out := make([]LintIssue, 0, len(issues))
	for _, issue := range issues {
		issue.Type = strings.TrimSpace(issue.Type)
		issue.Path = filepath.ToSlash(strings.TrimSpace(issue.Path))
		issue.Detail = strings.TrimSpace(issue.Detail)
		if issue.Type == "" || issue.Detail == "" {
			continue
		}
		out = append(out, issue)
	}
	return out
}
