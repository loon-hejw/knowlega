package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/core"
)

type QueryPlanningInput struct {
	Question string
	Purpose  string
	Schema   string
	Index    string
	Overview string
	LogTail  string
}

type QuerySynthesisInput struct {
	Question string
	Plan     core.QueryPlan
	Results  []core.QueryResult
	Docs     []QueryReadDocument
	Trace    []core.QueryTraceStep
}

type QueryReadDocument struct {
	Path    string
	Title   string
	Kind    string
	Content string
}

type QueryNavigationObservation struct {
	Action  string
	Query   string
	Path    string
	Pages   []QueryNavigationPage
	Skipped []string
}

type QueryNavigationPage struct {
	Path  string
	Title string
	Type  string
	Score int
}

type QueryAgent interface {
	PlanQuery(QueryPlanningInput) (core.QueryPlan, error)
	SynthesizeQuery(QuerySynthesisInput) (string, error)
}

type QueryActionInput struct {
	Question   string
	Plan       core.QueryPlan
	Results    []core.QueryResult
	Docs       []QueryReadDocument
	Navigation []QueryNavigationObservation
	Trace      []core.QueryTraceStep
	Step       int
}

type QueryActionAgent interface {
	NextQueryAction(QueryActionInput) (core.QueryAction, error)
}

type GraphEvidenceStore interface {
	SearchGraphEvidence(context.Context, string, string, int) ([]core.GraphEvidence, error)
}

type SearchEvidenceStore interface {
	SearchWikiEvidence(context.Context, string, core.QueryPlan, int) ([]core.QueryResult, error)
}

type QueryLogStore interface {
	InsertQueryLog(context.Context, string, string, string, string, int) error
}

type VectorSearchEvidenceStore interface {
	SearchWikiEvidenceVector(context.Context, string, core.QueryPlan, []float32, int) ([]core.QueryResult, error)
}

type EmbeddingProvider interface {
	EmbedText(context.Context, string) ([]float32, error)
}

type QueryOptions struct {
	ProjectPath       string
	ProjectID         string
	Question          string
	Limit             int
	Agent             QueryAgent
	SearchStore       SearchEvidenceStore
	GraphStore        GraphEvidenceStore
	QueryLogStore     QueryLogStore
	EmbeddingProvider EmbeddingProvider
	Context           context.Context
}

type FallbackQueryAgent struct{}

func QueryLLMWiki(projectPath, q string, limit int) (*core.QueryAnswer, error) {
	return QueryLLMWikiWithAgent(projectPath, q, limit, FallbackQueryAgent{})
}

func QueryLLMWikiWithAgent(projectPath, q string, limit int, agent QueryAgent) (*core.QueryAnswer, error) {
	return QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath: projectPath,
		Question:    q,
		Limit:       limit,
		Agent:       agent,
	})
}

func QueryLLMWikiWithOptions(opts QueryOptions) (*core.QueryAnswer, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	projectPath := opts.ProjectPath
	q := opts.Question
	limit := opts.Limit
	agent := opts.Agent
	if agent == nil {
		agent = FallbackQueryAgent{}
	}
	fallbackAgent := isFallbackQueryAgent(agent)
	input, err := queryPlanningInput(projectPath, q)
	if err != nil {
		return nil, err
	}
	plan, err := agent.PlanQuery(input)
	if err != nil {
		return nil, err
	}
	if plan.Question == "" {
		plan.Question = q
	}
	if limit > 0 {
		plan.CandidateLimit = limit
	} else if plan.CandidateLimit <= 0 {
		plan.CandidateLimit = 10
	}
	if strings.TrimSpace(plan.AnswerMode) == "" {
		if fallbackAgent {
			plan.AnswerMode = "offline_reading_fallback"
		} else {
			plan.AnswerMode = "llm_synthesis"
		}
	}
	docs, err := readPlanDocuments(projectPath, plan, 5000)
	if err != nil {
		return nil, err
	}
	var results []core.QueryResult
	var trace []core.QueryTraceStep
	var finalAnswer string
	var suggestedWritebackTitle string
	if actionAgent, ok := agent.(QueryActionAgent); ok {
		results, docs, trace, finalAnswer, suggestedWritebackTitle, err = runQueryActionLoop(ctx, projectPath, opts.ProjectID, q, plan, results, docs, actionAgent, opts.SearchStore, opts.GraphStore, opts.EmbeddingProvider)
		if err != nil {
			return nil, err
		}
	} else if len(plan.Searches) > 0 {
		results, err = SearchWikiCandidatesWithStore(ctx, projectPath, opts.ProjectID, opts.SearchStore, opts.EmbeddingProvider, plan, plan.CandidateLimit)
		if err != nil {
			return nil, err
		}
		candidateDocs, readErr := readQueryDocuments(projectPath, results, 8, 5000)
		if readErr != nil {
			return nil, readErr
		}
		docs = appendQueryDocs(docs, candidateDocs)
	}
	answer := finalAnswer
	if strings.TrimSpace(answer) == "" {
		answer, err = agent.SynthesizeQuery(QuerySynthesisInput{
			Question: q,
			Plan:     plan,
			Results:  results,
			Docs:     docs,
			Trace:    trace,
		})
		if err != nil {
			return nil, err
		}
	}
	notes := []string{
		"SearchWikiCandidates is only the recall tool; it is not the LLM Wiki query workflow by itself.",
	}
	if fallbackAgent {
		notes = append([]string{
			"FallbackQueryAgent is an offline scaffold. It reads recalled pages but does not perform semantic LLM planning, synthesis, or writeback.",
			"Use an OpenAI-compatible QueryAgent for the real LLM Wiki workflow.",
		}, notes...)
	}
	answerResult := &core.QueryAnswer{
		Question:                q,
		Plan:                    plan,
		Results:                 results,
		Answer:                  answer,
		SuggestedWritebackTitle: suggestedWritebackTitle,
		Citations:               queryCitations(docs),
		Trace:                   trace,
		Notes:                   notes,
	}
	if opts.QueryLogStore != nil && strings.TrimSpace(opts.ProjectID) != "" {
		if err := opts.QueryLogStore.InsertQueryLog(ctx, queryLogID(opts.ProjectID, q, plan.AnswerMode), opts.ProjectID, q, plan.AnswerMode, len(results)); err != nil {
			return nil, err
		}
	}
	return answerResult, nil
}

func isFallbackQueryAgent(agent QueryAgent) bool {
	_, ok := agent.(FallbackQueryAgent)
	return ok
}

func queryLogID(projectID, q, mode string) string {
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(projectID + "\x00" + q + "\x00" + mode + "\x00" + now.Format(time.RFC3339Nano)))
	return fmt.Sprintf("query-%d-%s", now.UnixNano(), hex.EncodeToString(sum[:])[:12])
}

func QueryWiki(projectPath, q string, limit int) ([]core.QueryResult, error) {
	plan := core.QueryPlan{
		Question: q,
		Searches: []core.QuerySearch{{
			Text:      q,
			Weight:    6,
			Rationale: "direct lexical fallback search",
		}},
		CandidateLimit: limit,
	}
	return SearchWikiCandidates(projectPath, plan, limit)
}

func SearchWikiCandidatesWithStore(ctx context.Context, projectPath, projectID string, store SearchEvidenceStore, embeddingProvider EmbeddingProvider, plan core.QueryPlan, limit int) ([]core.QueryResult, error) {
	if store != nil && strings.TrimSpace(projectID) != "" {
		if vectorStore, ok := store.(VectorSearchEvidenceStore); ok && embeddingProvider != nil {
			embedding, err := embeddingProvider.EmbedText(ctx, queryTextForEmbedding(plan))
			if err != nil {
				return nil, err
			}
			if len(embedding) > 0 {
				results, err := vectorStore.SearchWikiEvidenceVector(ctx, projectID, plan, embedding, limit)
				if err != nil {
					return nil, err
				}
				if len(results) > 0 {
					return results, nil
				}
			}
		}
		results, err := store.SearchWikiEvidence(ctx, projectID, plan, limit)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
	}
	return SearchWikiCandidates(projectPath, plan, limit)
}

func queryTextForEmbedding(plan core.QueryPlan) string {
	var parts []string
	for _, search := range plan.Searches {
		if strings.TrimSpace(search.Text) != "" {
			parts = append(parts, strings.TrimSpace(search.Text))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return strings.TrimSpace(plan.Question)
}

func SearchWikiCandidates(projectPath string, plan core.QueryPlan, limit int) ([]core.QueryResult, error) {
	if limit <= 0 {
		limit = 10
	}
	terms := queryTermsFromSearches(plan.Searches)
	if len(terms) == 0 {
		return nil, fmt.Errorf("query is empty")
	}
	var results []core.QueryResult
	if err := walkSearchRoot(projectPath, "wiki", ".md", terms, &results); err != nil {
		return nil, err
	}
	if err := walkSearchRoot(projectPath, filepath.Join("raw", "sources"), "", terms, &results); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			if results[i].Kind != results[j].Kind {
				return results[i].Kind == "wiki"
			}
			return results[i].Path < results[j].Path
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func queryPlanningInput(projectPath, q string) (QueryPlanningInput, error) {
	purpose, err := readProjectText(projectPath, "purpose.md")
	if err != nil {
		return QueryPlanningInput{}, err
	}
	schema, err := readProjectText(projectPath, "schema.md")
	if err != nil {
		return QueryPlanningInput{}, err
	}
	index, err := readProjectText(projectPath, filepath.Join("wiki", "index.md"))
	if err != nil {
		return QueryPlanningInput{}, err
	}
	overview, err := readProjectText(projectPath, filepath.Join("wiki", "overview.md"))
	if err != nil {
		return QueryPlanningInput{}, err
	}
	logData, err := readProjectText(projectPath, filepath.Join("wiki", "log.md"))
	if err != nil {
		return QueryPlanningInput{}, err
	}
	return QueryPlanningInput{
		Question: q,
		Purpose:  purpose,
		Schema:   schema,
		Index:    index,
		Overview: overview,
		LogTail:  tailRunes(logData, 4000),
	}, nil
}

func readProjectText(projectPath, rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readQueryDocuments(projectPath string, results []core.QueryResult, maxDocs int, maxRunes int) ([]QueryReadDocument, error) {
	if maxDocs <= 0 || maxDocs > len(results) {
		maxDocs = len(results)
	}
	docs := make([]QueryReadDocument, 0, maxDocs)
	for i := 0; i < maxDocs; i++ {
		result := results[i]
		content, err := readProjectText(projectPath, result.Path)
		if err != nil {
			return nil, err
		}
		docs = append(docs, QueryReadDocument{
			Path:    result.Path,
			Title:   result.Title,
			Kind:    result.Kind,
			Content: tailRunes(content, maxRunes),
		})
	}
	return docs, nil
}

func readPlanDocuments(projectPath string, plan core.QueryPlan, maxRunes int) ([]QueryReadDocument, error) {
	docs := make([]QueryReadDocument, 0, len(plan.ReadFirst))
	for _, rel := range plan.ReadFirst {
		if rel == "" {
			continue
		}
		content, err := readProjectText(projectPath, rel)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		docs = append(docs, QueryReadDocument{
			Path:    filepath.ToSlash(rel),
			Title:   titleForSearchResult(content, filepath.Base(rel), "wiki"),
			Kind:    "wiki-navigation",
			Content: tailRunes(content, maxRunes),
		})
	}
	return docs, nil
}

func appendQueryDocs(base, extra []QueryReadDocument) []QueryReadDocument {
	seen := make(map[string]bool, len(base)+len(extra))
	for _, doc := range base {
		seen[doc.Path] = true
	}
	for _, doc := range extra {
		if seen[doc.Path] {
			continue
		}
		base = append(base, doc)
		seen[doc.Path] = true
	}
	return base
}

func queryCitations(docs []QueryReadDocument) []core.QueryCitation {
	citations := make([]core.QueryCitation, 0, len(docs))
	seen := map[string]bool{}
	for _, doc := range docs {
		if seen[doc.Path] || isAggregateWikiPath(doc.Path) {
			continue
		}
		citations = append(citations, core.QueryCitation{
			Path:  doc.Path,
			Title: doc.Title,
			Kind:  doc.Kind,
		})
		seen[doc.Path] = true
	}
	return citations
}

func runQueryActionLoop(ctx context.Context, projectPath, projectID, q string, plan core.QueryPlan, results []core.QueryResult, docs []QueryReadDocument, agent QueryActionAgent, searchStore SearchEvidenceStore, graphStore GraphEvidenceStore, embeddingProvider EmbeddingProvider) ([]core.QueryResult, []QueryReadDocument, []core.QueryTraceStep, string, string, error) {
	const maxSteps = 6
	var trace []core.QueryTraceStep
	var navigation []QueryNavigationObservation
	for step := 1; step <= maxSteps; step++ {
		action, err := agent.NextQueryAction(QueryActionInput{
			Question:   q,
			Plan:       plan,
			Results:    results,
			Docs:       docs,
			Navigation: navigation,
			Trace:      trace,
			Step:       step,
		})
		if err != nil {
			return nil, nil, nil, "", "", err
		}
		action.Action = strings.ToLower(strings.TrimSpace(action.Action))
		if action.Action == "" && strings.TrimSpace(action.Answer) != "" {
			action.Action = "final"
		}
		switch action.Action {
		case "read":
			doc, err := readToolDocument(projectPath, action.Path, 6000)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			docs = appendQueryDocs(docs, []QueryReadDocument{doc})
			trace = append(trace, core.QueryTraceStep{
				Step:        step,
				Action:      action,
				Observation: fmt.Sprintf("read %s (%d chars)", doc.Path, len(doc.Content)),
			})
		case "list", "list_pages":
			listLimit := action.Limit
			if listLimit <= 0 {
				listLimit = 20
			}
			pages, err := listWikiPages(projectPath, action.Query, listLimit)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			navigation = append(navigation, QueryNavigationObservation{
				Action: action.Action,
				Query:  action.Query,
				Pages:  pages,
			})
			trace = append(trace, core.QueryTraceStep{
				Step:        step,
				Action:      action,
				Observation: formatWikiPageListObservation(pages),
			})
		case "follow_links":
			linkLimit := action.Limit
			if linkLimit <= 0 {
				linkLimit = 5
			}
			linkDocs, skipped, err := followWikiPageLinks(projectPath, action.Path, linkLimit, 6000)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			docs = appendQueryDocs(docs, linkDocs)
			navigation = append(navigation, QueryNavigationObservation{
				Action:  action.Action,
				Path:    action.Path,
				Pages:   wikiPageSummariesFromDocs(linkDocs),
				Skipped: skipped,
			})
			trace = append(trace, core.QueryTraceStep{
				Step:        step,
				Action:      action,
				Observation: formatFollowLinksObservation(linkDocs, skipped),
			})
		case "search":
			searchText := strings.TrimSpace(action.Query)
			if searchText == "" {
				searchText = strings.TrimSpace(q)
			}
			searchLimit := action.Limit
			if searchLimit <= 0 {
				searchLimit = plan.CandidateLimit
			}
			if searchLimit <= 0 {
				searchLimit = 10
			}
			searchPlan := core.QueryPlan{
				Question: q,
				Searches: []core.QuerySearch{{
					Text:      searchText,
					Weight:    6,
					Rationale: action.Rationale,
				}},
				CandidateLimit: searchLimit,
			}
			searchResults, err := SearchWikiCandidatesWithStore(ctx, projectPath, projectID, searchStore, embeddingProvider, searchPlan, searchLimit)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			results = appendQueryResults(results, searchResults)
			searchDocs, skipped, err := readSearchResultDocuments(projectPath, searchResults, minInt(searchLimit, 5), 6000)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			docs = appendQueryDocs(docs, searchDocs)
			if len(searchDocs) > 0 {
				seedPaths := make([]string, 0, len(searchDocs))
				for _, doc := range searchDocs {
					if strings.HasPrefix(doc.Path, "wiki/") {
						seedPaths = append(seedPaths, doc.Path)
					}
				}
				graphDocs, graphErr := SearchWikiGraphDocuments(projectPath, searchText, seedPaths, 3, 6000)
				if graphErr != nil {
					return nil, nil, nil, "", "", graphErr
				}
				docs = appendQueryDocs(docs, graphDocs)
			}
			trace = append(trace, core.QueryTraceStep{
				Step:        step,
				Action:      action,
				Observation: formatSearchObservation(searchResults) + formatSearchReadObservation(searchDocs, skipped),
			})
		case "graph", "expand":
			graphText := strings.TrimSpace(action.Query)
			if graphText == "" {
				graphText = strings.TrimSpace(q)
			}
			graphLimit := action.Limit
			if graphLimit <= 0 {
				graphLimit = 5
			}
			graphDocs, err := searchCodeGraphDocuments(ctx, projectPath, projectID, graphStore, graphText, graphLimit)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			wikiGraphDocs, err := SearchWikiGraphDocuments(projectPath, graphText, nil, graphLimit, 6000)
			if err != nil {
				return nil, nil, nil, "", "", err
			}
			docs = appendQueryDocs(docs, graphDocs)
			docs = appendQueryDocs(docs, wikiGraphDocs)
			trace = append(trace, core.QueryTraceStep{
				Step:        step,
				Action:      action,
				Observation: formatGraphObservation(graphDocs) + "; " + formatWikiGraphObservation(wikiGraphDocs),
			})
		case "final", "save", "writeback":
			finalAnswer, suggestedTitle, accepted := acceptFinalQueryAction(step, action, results, docs, &trace)
			if !accepted {
				continue
			}
			return results, docs, trace, finalAnswer, suggestedTitle, nil
		default:
			return nil, nil, nil, "", "", fmt.Errorf("unknown query action %q", action.Action)
		}
	}
	trace = append(trace, core.QueryTraceStep{
		Step: maxSteps + 1,
		Action: core.QueryAction{
			Action:    "synthesize",
			Rationale: "query action step limit reached",
		},
		Observation: "synthesizing with collected evidence",
	})
	return results, docs, trace, "", "", nil
}

func acceptFinalQueryAction(step int, action core.QueryAction, results []core.QueryResult, docs []QueryReadDocument, trace *[]core.QueryTraceStep) (string, string, bool) {
	if unsupported := unsupportedSearchOnlyCitations(action.Answer, results, docs); len(unsupported) > 0 {
		*trace = append(*trace, core.QueryTraceStep{
			Step:        step,
			Action:      action,
			Observation: action.Action + " rejected: cited search candidate(s) were not read: " + strings.Join(unsupported, ", "),
		})
		return "", "", false
	}
	if !hasAnswerEvidence(docs) {
		*trace = append(*trace, core.QueryTraceStep{
			Step:   step,
			Action: action,
			Observation: action.Action + " rejected: LLM Wiki answers require a read wiki/raw page or graph evidence; " +
				"search snippets are candidate recall only",
		})
		return "", "", false
	}
	observation := "final answer requested"
	if action.Action == "save" || action.Action == "writeback" {
		observation = "writeback suggested"
	}
	*trace = append(*trace, core.QueryTraceStep{
		Step:        step,
		Action:      action,
		Observation: observation,
	})
	return strings.TrimSpace(action.Answer), strings.TrimSpace(action.Title), true
}

func readSearchResultDocuments(projectPath string, results []core.QueryResult, maxDocs int, maxRunes int) ([]QueryReadDocument, []string, error) {
	if maxDocs <= 0 || maxDocs > len(results) {
		maxDocs = len(results)
	}
	docs := make([]QueryReadDocument, 0, maxDocs)
	var skipped []string
	for i := 0; i < maxDocs; i++ {
		result := results[i]
		rel, err := normalizeToolPath(result.Path)
		if err != nil {
			skipped = append(skipped, result.Path)
			continue
		}
		content, err := readProjectText(projectPath, rel)
		if err != nil {
			if os.IsNotExist(err) {
				skipped = append(skipped, rel)
				continue
			}
			return nil, nil, err
		}
		if strings.TrimSpace(content) == "" {
			skipped = append(skipped, rel)
			continue
		}
		kind := result.Kind
		if kind == "" {
			kind = "wiki-page"
			if strings.HasPrefix(rel, "raw/sources/") {
				kind = "raw-source"
			}
		}
		title := result.Title
		if title == "" {
			title = titleForSearchResult(content, filepath.Base(rel), kind)
		}
		docs = append(docs, QueryReadDocument{
			Path:    rel,
			Title:   title,
			Kind:    kind,
			Content: tailRunes(content, maxRunes),
		})
	}
	return docs, skipped, nil
}

func hasAnswerEvidence(docs []QueryReadDocument) bool {
	for _, doc := range docs {
		if doc.Path == "" || isAggregateWikiPath(doc.Path) {
			continue
		}
		return true
	}
	return false
}

func unsupportedSearchOnlyCitations(answer string, results []core.QueryResult, docs []QueryReadDocument) []string {
	if strings.TrimSpace(answer) == "" || len(results) == 0 {
		return nil
	}
	read := make(map[string]bool, len(docs))
	for _, doc := range docs {
		read[doc.Path] = true
	}
	var unsupported []string
	seen := map[string]bool{}
	for _, result := range results {
		if result.Path == "" || read[result.Path] || seen[result.Path] {
			continue
		}
		if strings.Contains(answer, result.Path) {
			unsupported = append(unsupported, result.Path)
			seen[result.Path] = true
		}
	}
	return unsupported
}

func formatSearchReadObservation(docs []QueryReadDocument, skipped []string) string {
	if len(docs) == 0 && len(skipped) == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if len(docs) > 0 {
		paths := make([]string, 0, len(docs))
		for _, doc := range docs {
			paths = append(paths, doc.Path)
		}
		parts = append(parts, fmt.Sprintf("auto-read %d candidate document(s): %s", len(docs), strings.Join(paths, ", ")))
	}
	if len(skipped) > 0 {
		parts = append(parts, fmt.Sprintf("skipped %d unreadable candidate(s): %s", len(skipped), strings.Join(skipped, ", ")))
	}
	return "; " + strings.Join(parts, "; ")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func listWikiPages(projectPath, q string, limit int) ([]QueryNavigationPage, error) {
	if limit <= 0 {
		limit = 20
	}
	root := filepath.Join(projectPath, "wiki")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	terms := queryTerms(q)
	var pages []QueryNavigationPage
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		rel, relErr := filepath.Rel(projectPath, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)
		title := titleForSearchResult(content, filepath.Base(path), "wiki-page")
		pageType := wikiPageType(content)
		score := 1
		if len(terms) > 0 {
			score = scoreContent(strings.Join([]string{rel, title, pageType}, "\n"), terms, rel, "wiki-page")
			if score == 0 {
				return nil
			}
		}
		if isAggregateWikiPath(rel) {
			score -= 1000
		}
		pages = append(pages, QueryNavigationPage{
			Path:  rel,
			Title: title,
			Type:  pageType,
			Score: score,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].Score == pages[j].Score {
			return pages[i].Path < pages[j].Path
		}
		return pages[i].Score > pages[j].Score
	})
	if len(pages) > limit {
		pages = pages[:limit]
	}
	return pages, nil
}

func wikiPageType(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "type:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "type:")), `"`)
		}
		if line == "---" || line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			break
		}
	}
	return "wiki-page"
}

func formatWikiPageListObservation(pages []QueryNavigationPage) string {
	if len(pages) == 0 {
		return "list_pages returned 0 wiki pages"
	}
	parts := make([]string, 0, len(pages))
	for _, page := range pages {
		parts = append(parts, fmt.Sprintf("%s title=%q type=%s", page.Path, page.Title, page.Type))
	}
	return fmt.Sprintf("list_pages returned %d wiki page(s): %s", len(pages), strings.Join(parts, "; "))
}

func wikiPageSummariesFromDocs(docs []QueryReadDocument) []QueryNavigationPage {
	pages := make([]QueryNavigationPage, 0, len(docs))
	for _, doc := range docs {
		pageType := doc.Kind
		if pageType == "" {
			pageType = "wiki-page"
		}
		pages = append(pages, QueryNavigationPage{
			Path:  doc.Path,
			Title: doc.Title,
			Type:  pageType,
		})
	}
	return pages
}

func followWikiPageLinks(projectPath, rel string, limit int, maxRunes int) ([]QueryReadDocument, []string, error) {
	if limit <= 0 {
		limit = 5
	}
	rel, err := normalizeToolPath(rel)
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasPrefix(rel, "wiki/") {
		return nil, nil, fmt.Errorf("follow_links path must be under wiki/: %s", rel)
	}
	content, err := readProjectText(projectPath, rel)
	if err != nil {
		return nil, nil, err
	}
	links := extractWikiMarkdownLinks(content)
	docs := make([]QueryReadDocument, 0, minInt(limit, len(links)))
	var skipped []string
	seen := map[string]bool{}
	for _, link := range links {
		if len(docs) >= limit {
			break
		}
		target, err := resolveWikiLink(projectPath, rel, link)
		if err != nil {
			skipped = append(skipped, link)
			continue
		}
		if target == rel || seen[target] {
			continue
		}
		seen[target] = true
		doc, err := readToolDocument(projectPath, target, maxRunes)
		if err != nil {
			if os.IsNotExist(err) {
				skipped = append(skipped, link)
				continue
			}
			return nil, nil, err
		}
		docs = append(docs, doc)
	}
	return docs, skipped, nil
}

func formatFollowLinksObservation(docs []QueryReadDocument, skipped []string) string {
	parts := make([]string, 0, 2)
	if len(docs) > 0 {
		paths := make([]string, 0, len(docs))
		for _, doc := range docs {
			paths = append(paths, doc.Path)
		}
		parts = append(parts, fmt.Sprintf("read %d linked document(s): %s", len(docs), strings.Join(paths, ", ")))
	}
	if len(skipped) > 0 {
		parts = append(parts, fmt.Sprintf("skipped %d unresolved link(s): %s", len(skipped), strings.Join(skipped, ", ")))
	}
	if len(parts) == 0 {
		return "follow_links found no readable linked documents"
	}
	return "follow_links " + strings.Join(parts, "; ")
}

func extractWikiMarkdownLinks(content string) []string {
	seen := map[string]bool{}
	var links []string
	add := func(link string) {
		link = strings.TrimSpace(link)
		if link == "" || seen[link] {
			return
		}
		seen[link] = true
		links = append(links, link)
	}
	for _, match := range wikilinkPattern.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			target := strings.Split(match[1], "|")[0]
			add(target)
		}
	}
	for _, match := range markdownLinkPattern.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	return links
}

func resolveWikiLink(projectPath, sourceRel, link string) (string, error) {
	link = strings.TrimSpace(strings.Split(link, "#")[0])
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "mailto:") || link == "" {
		return "", fmt.Errorf("external or empty link: %s", link)
	}
	if strings.HasPrefix(link, "wiki/") || strings.HasPrefix(link, "raw/sources/") {
		return normalizeToolPath(link)
	}
	if strings.HasSuffix(link, ".md") || strings.Contains(link, "/") {
		sourceDir := filepath.ToSlash(filepath.Dir(sourceRel))
		candidates := []string{
			filepath.ToSlash(filepath.Clean(filepath.Join("wiki", filepath.FromSlash(link)))),
			filepath.ToSlash(filepath.Clean(filepath.Join(filepath.FromSlash(sourceDir), filepath.FromSlash(link)))),
		}
		for _, candidate := range candidates {
			normalized, err := normalizeToolPath(candidate)
			if err != nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(normalized))); err == nil {
				return normalized, nil
			}
		}
	}
	return resolveWikiNameLink(projectPath, link)
}

func resolveWikiNameLink(projectPath, link string) (string, error) {
	targets := wikiLinkIDVariants(link)
	root := filepath.Join(projectPath, "wiki")
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" || found != "" {
			return err
		}
		rel, relErr := filepath.Rel(projectPath, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		candidates := []string{
			strings.TrimSuffix(strings.TrimPrefix(rel, "wiki/"), ".md"),
			strings.TrimSuffix(filepath.Base(rel), ".md"),
			titleForSearchResult(string(data), filepath.Base(path), "wiki-page"),
		}
		candidates = append(candidates, aliasesFromMarkdown(string(data))...)
		for _, candidate := range candidates {
			if wikiLinkIDsOverlap(targets, wikiLinkIDVariants(candidate)) {
				found = rel
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("unresolved wiki link: %s", link)
	}
	return found, nil
}

func normalizeWikiLinkID(s string) string {
	return canonicalWikiLinkID(s)
}

func wikiLinkIDVariants(s string) map[string]bool {
	s = cleanWikiLinkText(s)
	out := map[string]bool{}
	if s == "" {
		return out
	}
	out[s] = true
	out[canonicalWikiLinkID(s)] = true
	out[strings.ReplaceAll(canonicalWikiLinkID(s), "-", "")] = true
	return out
}

func canonicalWikiLinkID(s string) string {
	s = cleanWikiLinkText(s)
	spaced := strings.NewReplacer("_", " ", "-", " ").Replace(s)
	fields := strings.Fields(spaced)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, "-")
}

func cleanWikiLinkText(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[[") && strings.HasSuffix(s, "]]") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "[["), "]]")
	}
	s = strings.Split(s, "|")[0]
	s = strings.Split(s, "#")[0]
	s = strings.TrimSuffix(s, ".md")
	s = strings.ReplaceAll(s, "\\", "/")
	s = strings.Trim(s, "/")
	return strings.ToLower(strings.TrimSpace(s))
}

func wikiLinkIDsOverlap(a, b map[string]bool) bool {
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

func readToolDocument(projectPath, rel string, maxRunes int) (QueryReadDocument, error) {
	rel, err := resolveReadToolPath(projectPath, rel)
	if err != nil {
		return QueryReadDocument{}, err
	}
	content, err := readProjectText(projectPath, rel)
	if err != nil {
		return QueryReadDocument{}, err
	}
	if strings.TrimSpace(content) == "" {
		return QueryReadDocument{}, fmt.Errorf("query read found no content at %s", rel)
	}
	kind := "wiki-page"
	if strings.HasPrefix(rel, "raw/sources/") {
		kind = "raw-source"
	}
	return QueryReadDocument{
		Path:    rel,
		Title:   titleForSearchResult(content, filepath.Base(rel), kind),
		Kind:    kind,
		Content: tailRunes(content, maxRunes),
	}, nil
}

func resolveReadToolPath(projectPath, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("query read action requires path")
	}
	if strings.HasPrefix(target, "wiki/") || strings.HasPrefix(target, "raw/sources/") {
		return normalizeToolPath(target)
	}
	if strings.HasSuffix(target, ".md") || strings.Contains(target, "/") {
		resolved, err := resolveWikiLink(projectPath, "wiki/index.md", target)
		if err == nil {
			return resolved, nil
		}
	}
	return resolveWikiNameLink(projectPath, target)
}

func normalizeToolPath(rel string) (string, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return "", fmt.Errorf("query read action requires path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("query read path must be project-relative: %s", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("query read path escapes project: %s", rel)
	}
	if !strings.HasPrefix(clean, "wiki/") && !strings.HasPrefix(clean, "raw/sources/") {
		return "", fmt.Errorf("query read path must be under wiki/ or raw/sources/: %s", rel)
	}
	return clean, nil
}

func searchCodeGraphDocuments(ctx context.Context, projectPath, projectID string, graphStore GraphEvidenceStore, q string, limit int) ([]QueryReadDocument, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("graph query is empty")
	}
	if limit <= 0 {
		limit = 5
	}
	if graphStore != nil && strings.TrimSpace(projectID) != "" {
		evidence, err := graphStore.SearchGraphEvidence(ctx, projectID, q, limit)
		if err != nil {
			return nil, err
		}
		if len(evidence) > 0 {
			return graphEvidenceDocuments(evidence), nil
		}
	}
	root := filepath.Join(projectPath, "raw", "code-graphs")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	terms := queryTerms(q)
	var matches []graphDocMatch
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "graph.json" {
			return err
		}
		rel, _ := filepath.Rel(projectPath, path)
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		repoID := "code"
		if len(parts) >= 3 {
			repoID = parts[2]
		}
		snap, importErr := codegraph.ImportGraphify(repoID, "", path, "")
		if importErr != nil {
			return importErr
		}
		matches = append(matches, graphMatchesForSnapshot(rel, snap, terms)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return matches[i].Path < matches[j].Path
		}
		return matches[i].Score > matches[j].Score
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	docs := make([]QueryReadDocument, 0, len(matches))
	for _, match := range matches {
		docs = append(docs, QueryReadDocument{
			Path:    match.Path,
			Title:   match.Title,
			Kind:    "code-graph",
			Content: match.Content,
		})
	}
	return docs, nil
}

func graphEvidenceDocuments(evidence []core.GraphEvidence) []QueryReadDocument {
	docs := make([]QueryReadDocument, 0, len(evidence))
	for _, item := range evidence {
		docs = append(docs, QueryReadDocument{
			Path:    item.Path,
			Title:   item.Title,
			Kind:    "code-graph",
			Content: item.Content,
		})
	}
	return docs
}

type graphDocMatch struct {
	Path    string
	Title   string
	Content string
	Score   int
}

func graphMatchesForSnapshot(rel string, snap codegraph.Snapshot, terms []searchTerm) []graphDocMatch {
	nodeByID := map[string]codegraph.Node{}
	for _, node := range snap.Nodes {
		nodeByID[node.ID] = node
	}
	var matches []graphDocMatch
	for _, node := range snap.Nodes {
		text := strings.Join([]string{node.ID, string(node.Kind), node.Label, node.SourceFile, node.Community, fmt.Sprint(node.Props)}, " ")
		score := scoreGraphText(text, terms)
		if score == 0 {
			continue
		}
		matches = append(matches, graphDocMatch{
			Path:  rel + "#node/" + node.ID,
			Title: fmt.Sprintf("%s %s", node.Kind, node.Label),
			Score: score,
			Content: fmt.Sprintf("Graph node evidence\n\nRepo: %s\nGraph: %s\nNode ID: %s\nKind: %s\nLabel: %s\nSource file: %s\nCommunity: %s\nProps: %v",
				snap.RepoID, rel, node.ID, node.Kind, node.Label, node.SourceFile, node.Community, node.Props),
		})
	}
	for _, edge := range snap.Edges {
		source := nodeByID[edge.Source]
		target := nodeByID[edge.Target]
		text := strings.Join([]string{edge.Source, edge.Target, string(edge.Relation), edge.Confidence, source.Label, target.Label, source.SourceFile, target.SourceFile, fmt.Sprint(edge.Props)}, " ")
		score := scoreGraphText(text, terms)
		if score == 0 {
			continue
		}
		matches = append(matches, graphDocMatch{
			Path:  rel + "#edge/" + edge.Source + "/" + edge.Target,
			Title: fmt.Sprintf("%s -> %s %s", edge.Source, edge.Target, edge.Relation),
			Score: score,
			Content: fmt.Sprintf("Graph edge evidence\n\nRepo: %s\nGraph: %s\nSource: %s (%s)\nTarget: %s (%s)\nRelation: %s\nConfidence: %s\nWeight: %.3f\nProps: %v",
				snap.RepoID, rel, edge.Source, source.Label, edge.Target, target.Label, edge.Relation, edge.Confidence, edge.Weight, edge.Props),
		})
	}
	return matches
}

func scoreGraphText(text string, terms []searchTerm) int {
	text = strings.ToLower(text)
	score := 0
	matchedGroups := map[int]bool{}
	for _, term := range terms {
		count := strings.Count(text, term.Text)
		if count == 0 {
			continue
		}
		score += count * term.Weight
		matchedGroups[term.Group] = true
	}
	score += len(matchedGroups) * 80
	if len(matchedGroups) == queryGroupCount(terms) {
		score += 120
	}
	return score
}

func appendQueryResults(base, extra []core.QueryResult) []core.QueryResult {
	seen := make(map[string]bool, len(base)+len(extra))
	for _, result := range base {
		seen[result.Path] = true
	}
	for _, result := range extra {
		if seen[result.Path] {
			continue
		}
		base = append(base, result)
		seen[result.Path] = true
	}
	return base
}

func formatSearchObservation(results []core.QueryResult) string {
	if len(results) == 0 {
		return "search returned 0 results"
	}
	limit := len(results)
	if limit > 5 {
		limit = 5
	}
	paths := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		paths = append(paths, results[i].Path)
	}
	return fmt.Sprintf("search returned %d result(s): %s", len(results), strings.Join(paths, ", "))
}

func formatGraphObservation(docs []QueryReadDocument) string {
	if len(docs) == 0 {
		return "graph returned 0 evidence item(s)"
	}
	limit := len(docs)
	if limit > 5 {
		limit = 5
	}
	paths := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		paths = append(paths, docs[i].Path)
	}
	return fmt.Sprintf("graph returned %d evidence item(s): %s", len(docs), strings.Join(paths, ", "))
}

func (FallbackQueryAgent) PlanQuery(input QueryPlanningInput) (core.QueryPlan, error) {
	readFirst := []string{"wiki/index.md"}
	if input.Overview != "" {
		readFirst = append(readFirst, "wiki/overview.md")
	}
	if input.LogTail != "" {
		readFirst = append(readFirst, "wiki/log.md")
	}
	searches := []core.QuerySearch{{
		Text:      input.Question,
		Weight:    6,
		Rationale: "fallback direct search over wiki pages and raw sources",
	}}
	for _, field := range strings.Fields(input.Question) {
		if cjkRuneCount(field) >= 4 {
			searches = append(searches, core.QuerySearch{
				Text:      strings.Join(cjkBigrams(field), " "),
				Weight:    2,
				Rationale: "fallback CJK substring recall for long terms",
			})
		}
	}
	return core.QueryPlan{
		Question:       input.Question,
		Intent:         "offline_fallback_query",
		ReadFirst:      readFirst,
		Searches:       searches,
		CandidateLimit: 10,
		AnswerMode:     "offline_reading_fallback",
		CanWriteBack:   false,
	}, nil
}

func (FallbackQueryAgent) SynthesizeQuery(input QuerySynthesisInput) (string, error) {
	if len(input.Results) == 0 {
		return "No matching wiki pages or raw sources were found. A real LLM query agent should inspect the index, ask for a broader search, or create a review item for the missing concept.", nil
	}
	evidenceDocs := fallbackEvidenceDocs(input.Docs)
	var b strings.Builder
	b.WriteString("Offline fallback answer. This is not a full LLM Wiki synthesis; it is an extractive summary from recalled, read pages.\n\n")
	if len(evidenceDocs) > 0 {
		for i, doc := range evidenceDocs {
			fmt.Fprintf(&b, "%d. %s (%s)\n\n%s\n\n", i+1, doc.Title, doc.Path, fallbackDocExcerpt(doc.Content, 520))
		}
		return strings.TrimSpace(b.String()), nil
	}
	limit := minInt(len(input.Results), 5)
	for i := 0; i < limit; i++ {
		result := input.Results[i]
		fmt.Fprintf(&b, "%d. %s (%s): %s\n", i+1, result.Title, result.Path, result.Snippet)
	}
	return b.String(), nil
}

func fallbackEvidenceDocs(docs []QueryReadDocument) []QueryReadDocument {
	out := make([]QueryReadDocument, 0, minInt(len(docs), 5))
	for _, doc := range docs {
		if doc.Path == "" || isAggregateWikiPath(doc.Path) {
			continue
		}
		out = append(out, doc)
		if len(out) == 5 {
			break
		}
	}
	return out
}

func fallbackDocExcerpt(content string, maxRunes int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return tailRunes(content, maxRunes)
}

func walkSearchRoot(projectPath, relRoot, requiredExt string, terms []searchTerm, results *[]core.QueryResult) error {
	root := filepath.Join(projectPath, relRoot)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if requiredExt != "" && filepath.Ext(path) != requiredExt {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)
		rel, _ := filepath.Rel(projectPath, path)
		rel = filepath.ToSlash(rel)
		kind := "wiki"
		if strings.HasPrefix(rel, "raw/sources/") {
			kind = "raw-source"
		}
		score := scoreContent(content, terms, rel, kind)
		if score == 0 {
			return nil
		}
		*results = append(*results, core.QueryResult{
			Path:    rel,
			Title:   titleForSearchResult(content, filepath.Base(path), kind),
			Snippet: snippet(content, terms),
			Score:   score,
			Kind:    kind,
		})
		return nil
	})
}

type searchTerm struct {
	Text   string
	Weight int
	Group  int
}

func queryTerms(q string) []searchTerm {
	return queryTermsFromSearches([]core.QuerySearch{{Text: q, Weight: 6}})
}

func queryTermsFromSearches(searches []core.QuerySearch) []searchTerm {
	if len(searches) == 0 {
		return nil
	}
	out := make([]searchTerm, 0, len(searches)*3)
	seen := map[string]bool{}
	group := 0
	for _, search := range searches {
		weight := search.Weight
		if weight <= 0 {
			weight = 4
		}
		terms, nextGroup := queryTermsForSearch(search.Text, weight, group, seen)
		for _, term := range terms {
			out = append(out, term)
		}
		group = nextGroup
	}
	return out
}

func queryTermsForSearch(q string, weight int, group int, seen map[string]bool) ([]searchTerm, int) {
	fields := strings.Fields(strings.ToLower(q))
	out := make([]searchTerm, 0, len(fields)*3)
	for _, field := range fields {
		field = strings.Trim(field, `"'.,:;()[]{}<>`)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, searchTerm{Text: field, Weight: weight, Group: group})
		if cjkRuneCount(field) >= 4 {
			bigramWeight := weight / 3
			if bigramWeight < 1 {
				bigramWeight = 1
			}
			for _, gram := range cjkBigrams(field) {
				if !seen[gram] {
					seen[gram] = true
					out = append(out, searchTerm{Text: gram, Weight: bigramWeight, Group: group})
				}
			}
		}
		group++
	}
	return out, group
}

func scoreContent(content string, terms []searchTerm, relPath string, kind string) int {
	lower := strings.ToLower(content)
	score := 0
	title := strings.ToLower(titleFromMarkdown(content, ""))
	if kind == "raw-source" {
		title = strings.ToLower(rawSourceTitle(content))
	}
	aliases := aliasesFromMarkdown(content)
	matchedGroups := map[int]bool{}
	for _, term := range terms {
		count := strings.Count(lower, term.Text)
		score += count * term.Weight
		if strings.Contains(title, term.Text) {
			score += 10 * term.Weight
		}
		if containsAnyAlias(aliases, term.Text) {
			score += 30 * term.Weight
		}
		if count > 0 || strings.Contains(title, term.Text) || containsAnyAlias(aliases, term.Text) {
			matchedGroups[term.Group] = true
		}
	}
	score += len(matchedGroups) * 80
	if len(matchedGroups) == queryGroupCount(terms) {
		score += 120
	}
	if kind == "raw-source" {
		score = score * 2 / 3
		if score > 0 {
			score += 2
		}
	}
	if isAggregateWikiPath(relPath) {
		score = score / 4
	}
	return score
}

func queryGroupCount(terms []searchTerm) int {
	groups := map[int]bool{}
	for _, term := range terms {
		groups[term.Group] = true
	}
	return len(groups)
}

func aliasesFromMarkdown(content string) []string {
	frontmatter := frontmatterBlock(content)
	if frontmatter == "" {
		return nil
	}
	lines := strings.Split(frontmatter, "\n")
	var aliases []string
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "aliases:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "aliases:"))
			if value != "" {
				aliases = append(aliases, splitAliasValue(value)...)
				continue
			}
			for j := i + 1; j < len(lines); j++ {
				child := strings.TrimSpace(lines[j])
				if child == "" {
					continue
				}
				if !strings.HasPrefix(child, "- ") {
					break
				}
				aliases = append(aliases, splitAliasValue(strings.TrimSpace(strings.TrimPrefix(child, "- ")))...)
			}
		}
	}
	return aliases
}

func frontmatterBlock(content string) string {
	content = strings.TrimLeft(content, "\ufeff\r\n\t ")
	if !strings.HasPrefix(content, "---\n") {
		return ""
	}
	rest := strings.TrimPrefix(content, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func splitAliasValue(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	parts := strings.Split(value, ",")
	if len(parts) == 1 {
		parts = strings.Split(value, "|")
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, `"'`))
		if part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

func containsAnyAlias(aliases []string, term string) bool {
	for _, alias := range aliases {
		if strings.Contains(alias, term) {
			return true
		}
	}
	return false
}

func titleFromMarkdown(content, fallback string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "title:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "title:")), `"`)
		}
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return strings.TrimSuffix(fallback, filepath.Ext(fallback))
}

func titleForSearchResult(content, fallback, kind string) string {
	if kind == "raw-source" {
		if title := rawSourceTitle(content); title != "" {
			return title
		}
	}
	return titleFromMarkdown(content, fallback)
}

func rawSourceTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "《西游记》" {
			continue
		}
		if strings.HasPrefix(line, "《》目录 ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "《》目录 "))
		}
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
		return line
	}
	return ""
}

func snippet(content string, terms []searchTerm) string {
	term := ""
	lower := strings.ToLower(content)
	for _, candidate := range terms {
		if candidate.Text != "" && strings.Contains(lower, strings.ToLower(candidate.Text)) {
			term = candidate.Text
			break
		}
	}
	idx := strings.Index(lower, strings.ToLower(term))
	if idx < 0 {
		return runeWindow(content, 0, 220)
	}
	return runeWindowAroundByte(content, idx, 80, 160)
}

func isAggregateWikiPath(relPath string) bool {
	return relPath == "wiki/index.md" || relPath == "wiki/log.md" || relPath == "wiki/overview.md" || relPath == "wiki/reviews.md"
}

func runeWindow(content string, startRune, maxRunes int) string {
	runes := []rune(content)
	if startRune >= len(runes) {
		return ""
	}
	end := startRune + maxRunes
	if end > len(runes) {
		end = len(runes)
	}
	out := strings.TrimSpace(string(runes[startRune:end]))
	if end < len(runes) {
		out += "..."
	}
	return out
}

func runeWindowAroundByte(content string, byteIdx int, beforeRunes, afterRunes int) string {
	prefix := content[:byteIdx]
	centerRune := utf8.RuneCountInString(prefix)
	start := centerRune - beforeRunes
	if start < 0 {
		start = 0
	}
	return runeWindow(content, start, beforeRunes+afterRunes)
}

func tailRunes(content string, maxRunes int) string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[len(runes)-maxRunes:])
}

var cjkPattern = regexp.MustCompile(`[\p{Han}]`)
var wikilinkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
var markdownLinkPattern = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)

func cjkRuneCount(s string) int {
	count := 0
	for _, r := range s {
		if cjkPattern.MatchString(string(r)) {
			count++
		}
	}
	return count
}

func cjkBigrams(s string) []string {
	runes := []rune(s)
	out := make([]string, 0, len(runes))
	for i := 0; i+1 < len(runes); i++ {
		if cjkPattern.MatchString(string(runes[i])) && cjkPattern.MatchString(string(runes[i+1])) {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}
