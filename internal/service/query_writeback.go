package service

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type QueryWritebackOptions struct {
	ProjectPath string
	Title       string
	Answer      *core.QueryAnswer
}

type QueryWritebackResult struct {
	Path  string
	Title string
}

func WriteQueryAnswer(opts QueryWritebackOptions) (QueryWritebackResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return QueryWritebackResult{}, fmt.Errorf("project path is required")
	}
	if opts.Answer == nil {
		return QueryWritebackResult{}, fmt.Errorf("query answer is required")
	}
	if err := validateQueryWritebackEligibility(opts.Answer); err != nil {
		return QueryWritebackResult{}, err
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = synthesisTitle(opts.Answer.Question)
	}
	slug := core.Slug(title)
	if slug == "untitled" {
		slug = "query-" + core.StableID(opts.Answer.Question, opts.Answer.Answer)[:12]
	}
	rel := filepath.ToSlash(filepath.Join("wiki", "syntheses", slug+".md"))
	content := renderQuerySynthesisPage(title, opts.Answer)
	if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(content), "query-writeback: "+opts.Answer.Question); err != nil {
		return QueryWritebackResult{}, err
	}
	if err := appendIndex(opts.ProjectPath, "Syntheses", title, rel); err != nil {
		return QueryWritebackResult{}, err
	}
	if err := appendLog(opts.ProjectPath, "query", title, fmt.Sprintf("Query answer for `%s` written to `%s`.", opts.Answer.Question, rel)); err != nil {
		return QueryWritebackResult{}, err
	}
	if err := appendOverview(opts.ProjectPath, "Recent Syntheses", title, rel, "synthesis"); err != nil {
		return QueryWritebackResult{}, err
	}
	return QueryWritebackResult{Path: rel, Title: title}, nil
}

func validateQueryWritebackEligibility(answer *core.QueryAnswer) error {
	if !answer.Plan.CanWriteBack {
		return fmt.Errorf("query answer is not eligible for writeback")
	}
	if strings.HasPrefix(answer.Plan.Intent, "offline_") || strings.HasPrefix(answer.Plan.AnswerMode, "offline_") {
		return fmt.Errorf("offline query answers are not eligible for writeback")
	}
	for _, citation := range answer.Citations {
		if citation.Path == "" || isAggregateWikiPath(citation.Path) {
			continue
		}
		return nil
	}
	return fmt.Errorf("query writeback requires at least one non-navigation citation")
}

func synthesisTitle(question string) string {
	question = strings.TrimSpace(question)
	if question == "" {
		return "Query synthesis"
	}
	return "Query synthesis: " + question
}

func renderQuerySynthesisPage(title string, answer *core.QueryAnswer) string {
	sources := make([]string, 0, len(answer.Citations))
	seen := map[string]bool{}
	for _, citation := range answer.Citations {
		if citation.Path == "" || seen[citation.Path] {
			continue
		}
		seen[citation.Path] = true
		sources = append(sources, citation.Path)
	}
	planJSON, _ := json.MarshalIndent(answer.Plan, "", "  ")
	traceJSON, _ := json.MarshalIndent(answer.Trace, "", "  ")
	body := fmt.Sprintf(`# %s

## Question

%s

## Answer

%s

## Citations

%s

## Query Plan

%s
`, title, answer.Question, strings.TrimSpace(answer.Answer), renderCitations(answer.Citations), fenced(string(planJSON)))
	if len(answer.Trace) > 0 {
		body += fmt.Sprintf(`
## Query Trace

%s
`, fenced(string(traceJSON)))
	}
	return wiki.RenderPage(wiki.Page{
		Title:       title,
		Type:        "synthesis",
		Sources:     sources,
		GeneratedBy: "knowledge-core",
		Extra: map[string]string{
			"question":    answer.Question,
			"answer_mode": answer.Plan.AnswerMode,
			"created":     time.Now().Format("2006-01-02"),
		},
		Body: body,
	})
}

func renderCitations(citations []core.QueryCitation) string {
	if len(citations) == 0 {
		return "- No citations were returned.\n"
	}
	var b strings.Builder
	for _, citation := range citations {
		fmt.Fprintf(&b, "- `%s` (%s) %s\n", citation.Path, citation.Kind, citation.Title)
	}
	return b.String()
}
