package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/config"
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
	Sources        []WikiReviewSource
	Pages          []WikiReviewPage
}

type WikiReviewSource struct {
	RawPath      string   `json:"raw_path"`
	OriginalPath string   `json:"original_path"`
	Title        string   `json:"title"`
	Files        []string `json:"files"`
	ReviewCount  int      `json:"review_count"`
	Excerpt      string   `json:"excerpt"`
}

type WikiReviewPage struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases,omitempty"`
	Excerpt string   `json:"excerpt"`
}

type OpenAICompatibleWikiReviewAgent struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewEnvWikiReviewAgent() (WikiReviewAgent, bool, error) {
	apiKey := config.Value("KB_CORE_LLM_API_KEY", "OPENAI_API_KEY")
	model := config.Value("KB_CORE_LLM_MODEL", "OPENAI_MODEL")
	if apiKey == "" || model == "" {
		return nil, false, nil
	}
	baseURL := config.Value("KB_CORE_LLM_BASE_URL", "OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return OpenAICompatibleWikiReviewAgent{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}, true, nil
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
		reviewPages = append(reviewPages, WikiReviewPage{
			Path:    page.RelPath,
			Title:   titleForSearchResult(page.Content, filepath.Base(page.RelPath), "wiki-page"),
			Type:    wikiPageType(page.Content),
			Aliases: aliasesFromMarkdown(page.Content),
			Excerpt: tailRunes(markdownBody(page.Content), 1600),
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
		if strings.TrimSpace(entry.RawPath) != "" {
			excerpt = tailRunes(readOptionalProjectText(projectPath, entry.RawPath), 1200)
		}
		sources = append(sources, WikiReviewSource{
			RawPath:      entry.RawPath,
			OriginalPath: entry.OriginalPath,
			Title:        entry.Title,
			Files:        append([]string(nil), entry.Files...),
			ReviewCount:  entry.ReviewCount,
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
	sourcesJSON, _ := json.MarshalIndent(input.Sources, "", "  ")
	pagesJSON, _ := json.MarshalIndent(input.Pages, "", "  ")
	system := `You review a persistent LLM Wiki for maintenance issues.
Return only JSON with this shape:
{"issues":[{"type":"contradiction|duplicate|missing-page|stale-claim|source-gap|review-needed","path":"wiki/...","detail":"specific actionable issue"}]}

Rules:
- Focus on semantic wiki maintenance, not markdown formatting.
- Report contradictions, duplicate pages, missing concept/entity/synthesis pages, stale or weakly sourced claims, and source gaps.
- Pay special attention to unresolved review items, raw source excerpts, and source manifest entries that conflict with generated wiki pages.
- Treat page aliases as valid names for their page; do not report an alias as a missing page when it is listed on the target page.
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

Raw Source Excerpts:
%s

Pages:
%s`, input.Purpose, input.Schema, input.SourceManifest, input.Index, input.Overview, string(sourcesJSON), string(pagesJSON))
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
		BaseURL: a.BaseURL,
		APIKey:  a.APIKey,
		Model:   a.Model,
		Client:  a.Client,
	}
	return queryAgent.chat(system, user)
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
