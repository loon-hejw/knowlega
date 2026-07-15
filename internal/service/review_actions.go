package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type ReviewActionOptions struct {
	ProjectPath string
	ProjectID   string
	ReviewID    string
	Action      string
	Agent       QueryAgent
	Research    ResearchOptions
	Context     context.Context
}

type ResearchOptions struct {
	BaseURL    string
	MaxResults int
	Timeout    time.Duration
}

type ReviewActionResult struct {
	Review       core.ReviewItem `json:"review"`
	WrittenPaths []string        `json:"written_paths"`
	Message      string          `json:"message"`
}

type ReviewSweepOptions struct {
	ProjectPath string
	ProjectID   string
	Agent       QueryAgent
	Context     context.Context
}

type ReviewSweepResult struct {
	RuleResolved int               `json:"rule_resolved"`
	LLMResolved  int               `json:"llm_resolved"`
	Reviews      []core.ReviewItem `json:"reviews"`
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

func RunReviewAction(opts ReviewActionOptions) (ReviewActionResult, error) {
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return ReviewActionResult{}, err
	}
	defer release()
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	item, err := findReviewItem(opts.ProjectPath, opts.ProjectID, opts.ReviewID)
	if err != nil {
		return ReviewActionResult{}, err
	}
	switch opts.Action {
	case "create-page":
		return runCreatePageReviewAction(ctx, opts, item)
	case "deep-research":
		return runDeepResearchReviewAction(ctx, opts, item)
	case "resolve":
		updated, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "resolved", "resolve", time.Now().UTC())
		return ReviewActionResult{Review: updated, Message: "review resolved"}, err
	case "dismiss":
		updated, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "dismissed", "dismiss", time.Now().UTC())
		return ReviewActionResult{Review: updated, Message: "review dismissed"}, err
	case "reopen":
		updated, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "open", "reopen", time.Time{})
		return ReviewActionResult{Review: updated, Message: "review reopened"}, err
	default:
		return ReviewActionResult{}, fmt.Errorf("unknown review action %q", opts.Action)
	}
}

func SweepReviewItems(opts ReviewSweepOptions) (ReviewSweepResult, error) {
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return ReviewSweepResult{}, err
	}
	defer release()
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: opts.ProjectPath, ProjectID: opts.ProjectID})
	if err != nil {
		return ReviewSweepResult{}, err
	}
	index, err := buildReviewWikiIndex(opts.ProjectPath)
	if err != nil {
		return ReviewSweepResult{}, err
	}
	var result ReviewSweepResult
	var remaining []core.ReviewItem
	for _, item := range items {
		if item.Status != "open" {
			continue
		}
		if reviewResolvedByRule(item, index) {
			if _, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "resolved", "auto-rule", time.Now().UTC()); err != nil {
				return ReviewSweepResult{}, err
			}
			result.RuleResolved++
			continue
		}
		remaining = append(remaining, item)
	}
	if opts.Agent != nil && len(remaining) > 0 {
		resolved, err := llmResolvedReviewIDs(ctx, opts.Agent, remaining, index)
		if err != nil {
			return ReviewSweepResult{}, err
		}
		for _, id := range resolved {
			if _, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, id, "resolved", "llm-sweep", time.Now().UTC()); err != nil {
				return ReviewSweepResult{}, err
			}
			result.LLMResolved++
		}
	}
	result.Reviews, err = wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: opts.ProjectPath, ProjectID: opts.ProjectID})
	return result, err
}

// UpdateReviewItemsStatus is the locked service boundary for callers that need
// to update several durable review records without running a higher-level
// review action. PostgreSQL remains derived from wiki/reviews.md.
func UpdateReviewItemsStatus(projectPath, projectID string, ids []string, status, action string) ([]core.ReviewItem, error) {
	release, err := acquireServiceProjectLock(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	updated := make([]core.ReviewItem, 0, len(ids))
	for _, id := range ids {
		item, err := wiki.UpdateReviewItemStatusWithAction(projectPath, projectID, id, status, action, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		updated = append(updated, item)
	}
	return updated, nil
}

func runCreatePageReviewAction(_ context.Context, opts ReviewActionOptions, item core.ReviewItem) (ReviewActionResult, error) {
	pageType := reviewPageType(item)
	title := reviewPageTitle(item)
	dir := reviewPageDir(pageType)
	rel := filepath.ToSlash(filepath.Join("wiki", dir, core.Slug(title)+".md"))
	content := fmt.Sprintf(`---
type: "%s"
title: "%s"
sources:
  - "%s"
confidence: "AMBIGUOUS"
---

# %s

This page was created from review task %s.

## Review Context

%s

## Next Steps

- Replace this draft with sourced LLM Wiki synthesis.
- Link related entity, concept, source, and synthesis pages.
`, pageType, escapeYAML(title), item.SourcePath, title, item.ID, item.Description)
	if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(content), "review action: create-page"); err != nil {
		return ReviewActionResult{}, err
	}
	if err := registerPageOwnership(opts.ProjectPath, rel, "review"); err != nil {
		return ReviewActionResult{}, err
	}
	updated, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "resolved", "create-page", time.Now().UTC())
	if err != nil {
		return ReviewActionResult{}, err
	}
	return ReviewActionResult{Review: updated, WrittenPaths: []string{rel}, Message: "page draft created"}, nil
}

func runDeepResearchReviewAction(ctx context.Context, opts ReviewActionOptions, item core.ReviewItem) (ReviewActionResult, error) {
	results, err := collectSearXNGResearch(ctx, item, opts.Research)
	if err != nil {
		return ReviewActionResult{}, err
	}
	if len(results) == 0 {
		return ReviewActionResult{}, fmt.Errorf("deep research found no sources")
	}
	agent := opts.Agent
	if agent == nil {
		agent = FallbackQueryAgent{}
	}
	docs := make([]QueryReadDocument, 0, len(results))
	for _, result := range results {
		docs = append(docs, QueryReadDocument{
			Path:    result.URL,
			Title:   result.Title,
			Kind:    "web-research",
			Content: result.Content,
		})
	}
	answer, err := agent.SynthesizeQuery(QuerySynthesisInput{
		Question: researchTopic(item),
		Docs:     docs,
		Plan: core.QueryPlan{
			Question:   researchTopic(item),
			Intent:     "deep_research_for_review",
			AnswerMode: "research_synthesis",
		},
	})
	if err != nil {
		return ReviewActionResult{}, err
	}
	rel := filepath.ToSlash(filepath.Join("wiki", "syntheses", core.Slug("research-"+item.Title)+".md"))
	content := researchSynthesisMarkdown(item, answer, results)
	if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(content), "review action: deep-research"); err != nil {
		return ReviewActionResult{}, err
	}
	if err := registerPageOwnership(opts.ProjectPath, rel, "review"); err != nil {
		return ReviewActionResult{}, err
	}
	updated, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, opts.ProjectID, item.ID, "resolved", "deep-research", time.Now().UTC())
	if err != nil {
		return ReviewActionResult{}, err
	}
	return ReviewActionResult{Review: updated, WrittenPaths: []string{rel}, Message: "research synthesis created"}, nil
}

func findReviewItem(projectPath, projectID, id string) (core.ReviewItem, error) {
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: projectID})
	if err != nil {
		return core.ReviewItem{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return core.ReviewItem{}, fmt.Errorf("review item not found: %s", id)
}

type reviewWikiIndex struct {
	IDs    map[string]bool
	Titles map[string]bool
}

func buildReviewWikiIndex(projectPath string) (reviewWikiIndex, error) {
	pages, err := loadWikiPages(projectPath)
	if err != nil {
		return reviewWikiIndex{}, err
	}
	index := reviewWikiIndex{IDs: map[string]bool{}, Titles: map[string]bool{}}
	for _, page := range pages {
		base := strings.TrimSuffix(filepath.Base(page.RelPath), filepath.Ext(page.RelPath))
		index.IDs[strings.ToLower(base)] = true
		title := titleForSearchResult(page.Content, base, "wiki-page")
		if title != "" {
			index.Titles[strings.ToLower(title)] = true
		}
	}
	return index, nil
}

func reviewResolvedByRule(item core.ReviewItem, index reviewWikiIndex) bool {
	switch item.Type {
	case "missing-page":
		for _, name := range reviewCandidateNames(item) {
			if pageNameExists(name, index) {
				return true
			}
		}
	case "duplicate":
		if len(item.AffectedPages) > 1 {
			existing := 0
			for _, page := range item.AffectedPages {
				base := strings.TrimSuffix(filepath.Base(page), filepath.Ext(page))
				if index.IDs[strings.ToLower(base)] {
					existing++
				}
			}
			return existing <= 1
		}
	}
	return false
}

func reviewCandidateNames(item core.ReviewItem) []string {
	names := []string{reviewPageTitle(item)}
	for _, page := range item.AffectedPages {
		base := strings.TrimSuffix(filepath.Base(page), filepath.Ext(page))
		if base != "" {
			names = append(names, base)
		}
	}
	return names
}

func pageNameExists(name string, index reviewWikiIndex) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	return index.IDs[name] || index.IDs[strings.ReplaceAll(name, " ", "-")] || index.Titles[name]
}

func llmResolvedReviewIDs(_ context.Context, agent QueryAgent, items []core.ReviewItem, index reviewWikiIndex) ([]string, error) {
	var list strings.Builder
	limit := len(items)
	if limit > 40 {
		limit = 40
	}
	for i := 0; i < limit; i++ {
		item := items[i]
		fmt.Fprintf(&list, "- id=%s type=%s title=%q detail=%s\n", item.ID, item.Type, item.Title, promptSnippet(item.Description, 240))
	}
	answer, err := agent.SynthesizeQuery(QuerySynthesisInput{
		Question: `Which review items are now resolved by the current wiki state?
Return only JSON in this shape: {"resolved":["review-id"]}.
Be conservative. Keep contradictions, confirmations, and human judgment items pending unless clearly resolved.`,
		Docs: []QueryReadDocument{
			{
				Path:    "wiki/index",
				Title:   "Current wiki index",
				Kind:    "wiki-navigation",
				Content: fmt.Sprintf("Page ids: %s\nPage titles: %s", mapKeys(index.IDs, 300), mapKeys(index.Titles, 300)),
			},
			{
				Path:    "wiki/reviews",
				Title:   "Pending review items",
				Kind:    "review-list",
				Content: list.String(),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Resolved []string `json:"resolved"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(answer)), &parsed); err != nil {
		return nil, nil
	}
	valid := map[string]bool{}
	for _, item := range items[:limit] {
		valid[item.ID] = true
	}
	var out []string
	for _, id := range parsed.Resolved {
		if valid[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func collectSearXNGResearch(ctx context.Context, item core.ReviewItem, opts ResearchOptions) ([]searxngResult, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("research.searxng_url is required for deep research")
	}
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	queries := item.SearchQueries
	if len(queries) == 0 {
		queries = []string{researchTopic(item)}
	}
	seen := map[string]bool{}
	var out []searxngResult
	for _, query := range queries {
		endpoint := baseURL + "/search?q=" + url.QueryEscape(query) + "&format=json"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var parsed searxngResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("searxng request failed: status=%d", resp.StatusCode)
		}
		for _, result := range parsed.Results {
			key := strings.ToLower(result.URL)
			if key == "" {
				key = strings.ToLower(result.Title + result.Content)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Content = promptSnippet(result.Content, 1800)
			out = append(out, result)
			if len(out) >= maxResults {
				return out, nil
			}
		}
	}
	return out, nil
}

func researchSynthesisMarkdown(item core.ReviewItem, answer string, results []searxngResult) string {
	queries := item.SearchQueries
	if len(queries) == 0 {
		queries = []string{researchTopic(item)}
	}
	return fmt.Sprintf(`---
type: "synthesis"
title: "%s"
review_id: "%s"
search_queries:
%s
research_sources:
%s
confidence: "INFERRED"
---

# %s

Original review: %s

%s
`, escapeYAML("Research: "+item.Title), item.ID, yamlList(queries), yamlList(resultURLs(results)), "Research: "+item.Title, item.ID, strings.TrimSpace(answer))
}

func reviewPageType(item core.ReviewItem) string {
	text := strings.ToLower(item.Title + "\n" + item.Description)
	switch {
	case strings.Contains(text, "entity") || strings.Contains(text, "实体"):
		return "entity"
	case strings.Contains(text, "synthesis") || strings.Contains(text, "综合"):
		return "synthesis"
	default:
		return "concept"
	}
}

func reviewPageDir(pageType string) string {
	switch pageType {
	case "entity":
		return "entities"
	case "synthesis":
		return "syntheses"
	default:
		return "concepts"
	}
}

func reviewPageTitle(item core.ReviewItem) string {
	title := strings.TrimSpace(item.Title)
	title = regexp.MustCompile(`(?i)^(missing page|missing-page|缺失页面|缺少页面)[:：\s-]*`).ReplaceAllString(title, "")
	if title == "" {
		return "Untitled Review Page"
	}
	return title
}

func researchTopic(item core.ReviewItem) string {
	if item.Title != "" {
		return item.Title
	}
	return item.Description
}

func yamlList(values []string) string {
	if len(values) == 0 {
		return `  - ""`
	}
	var b strings.Builder
	for _, value := range values {
		fmt.Fprintf(&b, "  - \"%s\"\n", escapeYAML(value))
	}
	return strings.TrimRight(b.String(), "\n")
}

func resultURLs(results []searxngResult) []string {
	out := make([]string, 0, len(results))
	for _, result := range results {
		if result.URL != "" {
			out = append(out, result.URL)
		}
	}
	return out
}

func escapeYAML(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func promptSnippet(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func mapKeys(values map[string]bool, limit int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
		if len(keys) >= limit {
			break
		}
	}
	return strings.Join(keys, ", ")
}
