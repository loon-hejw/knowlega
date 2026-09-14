package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/codegraph"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

// KnowledgeSearchPlan is the complete contract needed by deterministic recall.
// It deliberately contains no answer-planning or LLM runtime state.
type KnowledgeSearchPlan struct {
	Query       string
	Limit       int
	Terms       []string
	ExactPhrase string
}

type SearchEvidenceStore interface {
	SearchWikiEvidence(context.Context, string, KnowledgeSearchPlan) ([]core.KnowledgeSearchResult, error)
}

type VectorSearchEvidenceStore interface {
	SearchWikiEvidenceVector(context.Context, string, KnowledgeSearchPlan, []float32) ([]core.KnowledgeSearchResult, error)
}

type GraphEvidenceStore interface {
	SearchGraphEvidence(context.Context, string, string, int) ([]core.GraphEvidence, error)
}

type EmbeddingProvider interface {
	EmbedText(context.Context, string) ([]float32, error)
}

type KnowledgeDocument struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Aliases []string `json:"aliases,omitempty"`
	Sources []string `json:"sources,omitempty"`
	Content string   `json:"content"`
}

type KnowledgePage struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Type  string `json:"type"`
	Score int    `json:"score"`
}

type KnowledgeFollowResult struct {
	Documents []KnowledgeDocument `json:"documents"`
	Skipped   []string            `json:"skipped,omitempty"`
}

// SearchProjectDocuments performs candidate recall only. Its results are
// navigation hints; callers must read a document before treating it as evidence.
func SearchProjectDocuments(ctx context.Context, projectPath, projectID, query string, limit int, store SearchEvidenceStore, embeddingProvider EmbeddingProvider) ([]core.KnowledgeSearchResult, error) {
	plan, err := normalizeKnowledgeSearchPlan(query, limit)
	if err != nil {
		return nil, err
	}
	backendLimit := plan.Limit * 3
	if backendLimit < 20 {
		backendLimit = 20
	}
	if backendLimit > 60 {
		backendLimit = 60
	}
	backendPlan := plan
	backendPlan.Limit = backendLimit
	var rankedLists [][]core.KnowledgeSearchResult
	var backendErrors []error
	if store != nil && strings.TrimSpace(projectID) != "" {
		if vectorStore, ok := store.(VectorSearchEvidenceStore); ok && embeddingProvider != nil {
			embedding, embedErr := embeddingProvider.EmbedText(ctx, plan.Query)
			if embedErr != nil {
				backendErrors = append(backendErrors, fmt.Errorf("vector embedding: %w", embedErr))
			} else if len(embedding) > 0 {
				results, searchErr := vectorStore.SearchWikiEvidenceVector(ctx, projectID, backendPlan, embedding)
				if searchErr != nil {
					backendErrors = append(backendErrors, fmt.Errorf("vector search: %w", searchErr))
				} else {
					rankedLists = append(rankedLists, results)
				}
			}
		}
		results, searchErr := store.SearchWikiEvidence(ctx, projectID, backendPlan)
		if searchErr != nil {
			backendErrors = append(backendErrors, fmt.Errorf("postgres search: %w", searchErr))
		} else {
			rankedLists = append(rankedLists, results)
		}
	}
	fileResults, fileErr := searchProjectFiles(projectPath, backendPlan)
	if fileErr != nil {
		backendErrors = append(backendErrors, fmt.Errorf("file search: %w", fileErr))
	} else {
		rankedLists = append(rankedLists, fileResults)
	}
	if len(rankedLists) == 0 {
		return nil, errors.Join(backendErrors...)
	}
	if plan.ExactPhrase != "" && fileErr == nil {
		// Markdown is authoritative. For opaque identifier-like queries (for
		// example a candidate-specific negative check), only paths whose actual
		// file content contains the complete phrase are eligible. This prevents
		// PG FTS, vector similarity, and individual common-term matches from
		// turning an exact absence into a false positive.
		exactPaths := make(map[string]bool, len(fileResults))
		for _, result := range fileResults {
			exactPaths[normalizeKnowledgePath(result.Path)] = true
		}
		if len(exactPaths) == 0 {
			return []core.KnowledgeSearchResult{}, nil
		}
		for i, list := range rankedLists {
			filtered := list[:0]
			for _, result := range list {
				if exactPaths[normalizeKnowledgePath(result.Path)] {
					filtered = append(filtered, result)
				}
			}
			rankedLists[i] = filtered
		}
	}
	direct := fuseKnowledgeRankings(rankedLists, plan.Limit)
	return appendKnowledgeSearchExpansion(projectPath, plan, direct), nil
}

func normalizeKnowledgeSearchPlan(query string, limit int) (KnowledgeSearchPlan, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return KnowledgeSearchPlan{}, fmt.Errorf("search query is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	return KnowledgeSearchPlan{Query: query, Limit: limit, Terms: KnowledgeSearchTerms(query), ExactPhrase: knowledgeExactPhrase(query)}, nil
}

func appendKnowledgeSearchExpansion(projectPath string, plan KnowledgeSearchPlan, direct []core.KnowledgeSearchResult) []core.KnowledgeSearchResult {
	if plan.ExactPhrase != "" {
		return direct
	}
	if len(direct) >= plan.Limit {
		return direct
	}
	seedPaths := make([]string, 0, len(direct))
	for _, result := range direct {
		if !IsAggregateKnowledgePath(result.Path) {
			seedPaths = append(seedPaths, result.Path)
		}
	}
	docs, err := SearchWikiGraphDocuments(projectPath, plan.Query, seedPaths, plan.Limit-len(direct), 2000)
	if err != nil {
		return direct
	}
	seen := map[string]bool{}
	for _, result := range direct {
		seen[normalizeKnowledgePath(result.Path)] = true
	}
	nextScore := 1
	if len(direct) > 0 {
		nextScore = maxKnowledgeInt(1, direct[len(direct)-1].Score-1)
	}
	for _, doc := range docs {
		key := normalizeKnowledgePath(doc.Path)
		if key == "" || seen[key] || IsAggregateKnowledgePath(doc.Path) {
			continue
		}
		seen[key] = true
		direct = append(direct, core.KnowledgeSearchResult{Path: doc.Path, Title: doc.Title, Snippet: tailKnowledgeRunes(doc.Content, 320), Score: nextScore, Kind: "wiki-graph"})
		if len(direct) >= plan.Limit {
			break
		}
	}
	return direct
}

func searchProjectFiles(projectPath string, plan KnowledgeSearchPlan) ([]core.KnowledgeSearchResult, error) {
	terms := knowledgeQueryTerms(plan.Query)
	var results []core.KnowledgeSearchResult
	if err := walkKnowledgeSearchRoot(projectPath, "wiki", ".md", plan, terms, &results); err != nil {
		return nil, err
	}
	if err := walkKnowledgeSearchRoot(projectPath, filepath.Join("raw", "sources"), "", plan, terms, &results); err != nil {
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
	if len(results) > plan.Limit {
		results = results[:plan.Limit]
	}
	return results, nil
}

func fuseKnowledgeRankings(lists [][]core.KnowledgeSearchResult, limit int) []core.KnowledgeSearchResult {
	type fused struct {
		result core.KnowledgeSearchResult
		score  int
	}
	byPath := map[string]*fused{}
	for _, list := range lists {
		seen := map[string]bool{}
		for rank, result := range list {
			path := normalizeKnowledgePath(result.Path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			entry := byPath[path]
			if entry == nil {
				copy := result
				copy.Path = filepath.ToSlash(strings.TrimPrefix(result.Path, "./"))
				entry = &fused{result: copy}
				byPath[path] = entry
			}
			entry.score += 1_000_000 / (61 + rank)
		}
	}
	results := make([]core.KnowledgeSearchResult, 0, len(byPath))
	for _, entry := range byPath {
		entry.result.Score = entry.score
		if IsAggregateKnowledgePath(entry.result.Path) {
			entry.result.Score /= 4
		}
		results = append(results, entry.result)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Path < results[j].Path
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// ReadProjectDocument resolves safe wiki/raw names, titles, aliases, and links.
func ReadProjectDocument(projectPath, target string) (KnowledgeDocument, error) {
	rel, err := resolveKnowledgeReadPath(projectPath, target)
	if err != nil {
		return KnowledgeDocument{}, err
	}
	content, err := readKnowledgeText(projectPath, rel)
	if err != nil {
		return KnowledgeDocument{}, err
	}
	if strings.TrimSpace(content) == "" {
		return KnowledgeDocument{}, fmt.Errorf("knowledge read found no content at %s", rel)
	}
	kind := "wiki-page"
	if strings.HasPrefix(rel, "raw/sources/") {
		kind = "raw-source"
	}
	aliases := []string(nil)
	sources := []string(nil)
	if strings.HasPrefix(rel, "wiki/") {
		page := wiki.ParseWikiPage("knowledge", rel, content)
		aliases = frontmatterStringList(page.Frontmatter, "aliases")
		sources = append(sources, page.Sources...)
	}
	return KnowledgeDocument{Path: rel, Title: knowledgeTitle(content, filepath.Base(rel), kind), Kind: kind, Aliases: aliases, Sources: sources, Content: tailKnowledgeRunes(content, 120000)}, nil
}

func ListProjectPages(projectPath, query string, limit int) ([]KnowledgePage, error) {
	if limit <= 0 {
		limit = 20
	}
	root := filepath.Join(projectPath, "wiki")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	terms := knowledgeQueryTerms(query)
	var pages []KnowledgePage
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
			return walkErr
		}
		rel, err := filepath.Rel(projectPath, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		title := knowledgeTitle(content, filepath.Base(path), "wiki-page")
		pageType := knowledgePageType(content)
		score := 1
		if len(terms) > 0 {
			score = scoreKnowledgeContent(strings.Join([]string{rel, title, pageType}, "\n"), terms, rel, "wiki-page")
			if score == 0 {
				return nil
			}
		}
		if IsAggregateKnowledgePath(rel) {
			score -= 1000
		}
		pages = append(pages, KnowledgePage{Path: rel, Title: title, Type: pageType, Score: score})
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

func FollowProjectLinks(projectPath, path string, limit int) (KnowledgeFollowResult, error) {
	if limit <= 0 {
		limit = 5
	}
	doc, err := ReadProjectDocument(projectPath, path)
	if err != nil {
		return KnowledgeFollowResult{}, err
	}
	links := extractKnowledgeLinks(doc.Content)
	result := KnowledgeFollowResult{Documents: make([]KnowledgeDocument, 0, minKnowledgeInt(limit, len(links)))}
	seen := map[string]bool{doc.Path: true}
	for _, link := range links {
		if len(result.Documents) >= limit {
			break
		}
		target, err := resolveKnowledgeWikiLink(projectPath, doc.Path, link)
		if err != nil {
			result.Skipped = append(result.Skipped, link)
			continue
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		linked, err := ReadProjectDocument(projectPath, target)
		if err != nil {
			result.Skipped = append(result.Skipped, link)
			continue
		}
		result.Documents = append(result.Documents, linked)
	}
	return result, nil
}

// DiscoverProjectCandidates deterministically recalls candidate entity pages
// and ranks common neighbors of named reference entities before lexical recall.
func DiscoverProjectCandidates(ctx context.Context, projectPath, projectID string, requirements []core.KnowledgeRequirement, limit int, store SearchEvidenceStore, embeddingProvider EmbeddingProvider) ([]core.KnowledgeCandidate, error) {
	if len(requirements) == 0 {
		return nil, fmt.Errorf("discover requires at least one requirement")
	}
	if limit <= 0 {
		limit = 12
	}
	index, err := buildKnowledgeEntityIndex(projectPath)
	if err != nil {
		return nil, err
	}
	var referenceGroups [][]string
	referencePaths := map[string]bool{}
	for _, requirement := range requirements {
		if strings.EqualFold(requirement.Kind, "negative") {
			continue
		}
		paths := referencedKnowledgeEntities(index, requirement.Text)
		if len(paths) == 0 {
			continue
		}
		referenceGroups = append(referenceGroups, paths)
		for _, path := range paths {
			referencePaths[path] = true
		}
	}
	type recalled struct {
		path, title  string
		ids          map[string]bool
		score        int
		contextScore int
	}
	candidates := map[string]*recalled{}
	for _, requirement := range requirements {
		id := strings.TrimSpace(requirement.ID)
		text := strings.TrimSpace(requirement.Text)
		if id == "" || text == "" || strings.EqualFold(requirement.Kind, "negative") {
			continue
		}
		results, err := discoverKnowledgeRequirementResults(ctx, projectPath, projectID, text, index, store, embeddingProvider)
		if err != nil {
			return nil, err
		}
		for rank, result := range results {
			if IsAggregateKnowledgePath(result.Path) {
				continue
			}
			directEntities, err := attributedKnowledgeRequirementEntities(projectPath, index, result, text)
			if err != nil {
				return nil, err
			}
			for _, entityPath := range directEntities {
				page := index.pages[entityPath]
				candidate := candidates[entityPath]
				if candidate == nil {
					candidate = &recalled{path: entityPath, title: page.Title, ids: map[string]bool{}}
					candidates[entityPath] = candidate
				}
				if !candidate.ids[id] {
					candidate.ids[id] = true
					candidate.score += 100 + maxKnowledgeInt(1, 20-rank)
				}
			}
			for _, entityPath := range inferredKnowledgeEntities(index, result) {
				if containsKnowledgePath(directEntities, entityPath) {
					continue
				}
				page := index.pages[entityPath]
				candidate := candidates[entityPath]
				if candidate == nil {
					candidate = &recalled{path: entityPath, title: page.Title, ids: map[string]bool{}}
					candidates[entityPath] = candidate
				}
				candidate.score += maxKnowledgeInt(1, 5-rank/4)
			}
		}
	}
	items := make([]core.KnowledgeCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if len(referenceGroups) > 1 && referencePaths[candidate.path] {
			continue
		}
		ids := make([]string, 0, len(candidate.ids))
		for id := range candidate.ids {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(referenceGroups) > 1 && len(ids) == 0 {
			continue
		}
		candidate.contextScore = knowledgeCandidateReferenceScore(index, candidate.path, referenceGroups)
		items = append(items, core.KnowledgeCandidate{Path: candidate.path, Title: candidate.title, Kind: "entity", Score: candidate.contextScore*1000 + len(ids)*10000 + candidate.score, RecallRequirementIDs: ids, RecallRequirementCount: len(ids)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if items[i].RecallRequirementCount != items[j].RecallRequirementCount {
			return items[i].RecallRequirementCount > items[j].RecallRequirementCount
		}
		return items[i].Path < items[j].Path
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func SearchProjectGraph(ctx context.Context, projectPath, projectID, query string, seedPaths []string, limit int, graphStore GraphEvidenceStore) ([]KnowledgeDocument, error) {
	if strings.TrimSpace(query) == "" && len(seedPaths) == 0 {
		return nil, fmt.Errorf("graph query or seed_paths is required")
	}
	if limit <= 0 {
		limit = 5
	}
	var docs []KnowledgeDocument
	if strings.TrimSpace(query) != "" {
		codeDocs, err := searchKnowledgeCodeGraph(ctx, projectPath, projectID, graphStore, query, limit)
		if err != nil {
			return nil, err
		}
		docs = appendKnowledgeDocuments(docs, codeDocs)
	}
	resolvedSeeds, err := resolveKnowledgeGraphSeeds(projectPath, seedPaths)
	if err != nil {
		return nil, err
	}
	wikiDocs, err := SearchWikiGraphDocuments(projectPath, query, resolvedSeeds, limit, 12000)
	if err != nil {
		return nil, err
	}
	docs = appendKnowledgeDocuments(docs, wikiDocs)
	if len(docs) > limit {
		docs = docs[:limit]
	}
	return docs, nil
}

func resolveKnowledgeGraphSeeds(projectPath string, seeds []string) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	bySource := map[string][]string{}
	for _, page := range pages {
		if IsAggregateKnowledgePath(page.Path) {
			continue
		}
		for _, source := range page.Sources {
			key := normalizeKnowledgePath(source)
			bySource[key] = appendUniqueKnowledgeString(bySource[key], page.Path)
		}
	}
	var resolved []string
	for _, seed := range seeds {
		path, err := resolveKnowledgeReadPath(projectPath, seed)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(path, "wiki/") {
			if !IsAggregateKnowledgePath(path) {
				resolved = appendUniqueKnowledgeString(resolved, path)
			}
			continue
		}
		resolved = appendUniqueKnowledgeStrings(resolved, bySource[normalizeKnowledgePath(path)]...)
	}
	return resolved, nil
}

func IsAggregateKnowledgePath(path string) bool {
	path = normalizeKnowledgePath(path)
	return path == "wiki/index.md" || path == "wiki/log.md" || path == "wiki/overview.md" || path == "wiki/reviews.md"
}

func normalizeKnowledgePath(path string) string {
	return strings.ToLower(filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(path), "./")))
}

func normalizeKnowledgeToolPath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("knowledge path must be project-relative")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("knowledge path escapes project: %s", path)
	}
	if !strings.HasPrefix(clean, "wiki/") && !strings.HasPrefix(clean, "raw/sources/") {
		return "", fmt.Errorf("knowledge path must be under wiki/ or raw/sources/: %s", path)
	}
	return clean, nil
}

func resolveKnowledgeReadPath(projectPath, target string) (string, error) {
	target = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(target, "[["), "]]"))
	target = strings.Split(strings.Split(target, "|")[0], "#")[0]
	if target == "" {
		return "", fmt.Errorf("knowledge read requires path")
	}
	aliasDir, aliasName := "", ""
	if strings.HasPrefix(target, "wiki/") || strings.HasPrefix(target, "raw/sources/") {
		rel, err := normalizeKnowledgeToolPath(target)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(rel))); err == nil {
			return rel, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if strings.HasPrefix(rel, "wiki/") && filepath.Ext(rel) == ".md" {
			aliasDir = filepath.ToSlash(filepath.Dir(rel))
			aliasName = knowledgeLinkID(filepath.Base(rel))
		}
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	wanted := knowledgeLinkID(target)
	var matches []string
	for _, page := range pages {
		keys := append([]string{page.Path, page.Title, strings.TrimSuffix(filepath.Base(page.Path), filepath.Ext(page.Path))}, frontmatterStringList(page.Frontmatter, "aliases")...)
		for _, key := range keys {
			if knowledgeLinkID(key) == wanted {
				matches = append(matches, page.Path)
				break
			}
		}
	}
	// A moved page can retain its old filename as a frontmatter alias. Resolve
	// missing explicit paths only within the requested directory, after exact
	// references have been checked; never guess across page types or raw sources.
	if len(matches) == 0 && aliasDir != "" {
		for _, page := range pages {
			if filepath.ToSlash(filepath.Dir(page.Path)) != aliasDir {
				continue
			}
			for _, alias := range frontmatterStringList(page.Frontmatter, "aliases") {
				if knowledgeLinkID(alias) == aliasName {
					matches = append(matches, page.Path)
					break
				}
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("knowledge path %q is ambiguous: %s", target, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("knowledge path not found: %s", target)
}

func resolveKnowledgeWikiLink(projectPath, sourceRel, link string) (string, error) {
	link = strings.TrimSpace(strings.Split(link, "#")[0])
	if link == "" || strings.Contains(link, "://") {
		return "", fmt.Errorf("external or empty link")
	}
	if strings.HasSuffix(strings.ToLower(link), ".md") && !strings.HasPrefix(link, "wiki/") {
		candidate := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(sourceRel), filepath.FromSlash(link))))
		if rel, err := normalizeKnowledgeToolPath(candidate); err == nil {
			if _, statErr := os.Stat(filepath.Join(projectPath, filepath.FromSlash(rel))); statErr == nil {
				return rel, nil
			}
		}
	}
	return resolveKnowledgeReadPath(projectPath, link)
}

func readKnowledgeText(projectPath, rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func knowledgePageType(content string) string {
	page := wiki.ParseWikiPage("knowledge", "wiki/page.md", content)
	if strings.TrimSpace(page.Type) != "" {
		return page.Type
	}
	return "wiki-page"
}

func knowledgeTitle(content, fallback, kind string) string {
	if kind == "raw-source" {
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "# ") {
				return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "# "))
			}
		}
	}
	page := wiki.ParseWikiPage("knowledge", "wiki/"+fallback, content)
	if strings.TrimSpace(page.Title) != "" {
		return page.Title
	}
	return strings.TrimSuffix(fallback, filepath.Ext(fallback))
}

type knowledgeSearchTerm struct {
	text   string
	weight int
}

func knowledgeQueryTerms(query string) []knowledgeSearchTerm {
	indexes := map[string]int{}
	var terms []knowledgeSearchTerm
	add := func(value string, weight int) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		if index, ok := indexes[value]; ok {
			if weight > terms[index].weight {
				terms[index].weight = weight
			}
			return
		}
		indexes[value] = len(terms)
		terms = append(terms, knowledgeSearchTerm{text: value, weight: weight})
	}
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	if phrase := strings.Join(fields, " "); phrase != "" && len([]rune(phrase)) <= 64 {
		add(phrase, 10)
	}
	for _, field := range fields {
		add(field, 6)
		for _, run := range knowledgeCJKRuns(field) {
			runes := []rune(run)
			for size, weight := range map[int]int{2: 2, 3: 3} {
				for i := 0; i+size <= len(runes); i++ {
					add(string(runes[i:i+size]), weight)
				}
			}
		}
	}
	return terms
}

// knowledgeExactPhrase identifies opaque, punctuation-delimited lookups. Such
// queries are commonly used for candidate-specific negative requirements and
// must not degrade into an OR search over one common component (for example
// "zqx-no-finance-director-91" matching every page that says "director").
func knowledgeExactPhrase(query string) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if len([]rune(query)) < 5 || strings.IndexFunc(query, unicode.IsSpace) >= 0 {
		return ""
	}
	if !strings.ContainsAny(query, "-_/.:@") {
		return ""
	}
	hasLetter, hasDigit := false, false
	for _, r := range query {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return ""
	}
	return query
}

// KnowledgeSearchTerms exposes the same punctuation and CJK normalization to
// PostgreSQL and graph backends so file and derived-index recall stay aligned.
func KnowledgeSearchTerms(query string) []string {
	weighted := knowledgeQueryTerms(query)
	terms := make([]string, 0, len(weighted))
	for _, term := range weighted {
		terms = append(terms, term.text)
	}
	return terms
}

func knowledgeCJKRuns(value string) []string {
	var runs []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			runs = append(runs, string(current))
			current = nil
		}
	}
	for _, r := range value {
		if isKnowledgeCJKRune(r) {
			current = append(current, r)
		} else {
			flush()
		}
	}
	flush()
	return runs
}

func walkKnowledgeSearchRoot(projectPath, relRoot, extension string, plan KnowledgeSearchPlan, terms []knowledgeSearchTerm, results *[]core.KnowledgeSearchResult) error {
	root := filepath.Join(projectPath, relRoot)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		if extension != "" && filepath.Ext(path) != extension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(projectPath, path)
		rel = filepath.ToSlash(rel)
		kind := "wiki"
		if strings.HasPrefix(rel, "raw/sources/") {
			kind = "raw-source"
		}
		content := string(data)
		score := scoreKnowledgeContentForPlan(content, plan, terms, rel, kind)
		if score == 0 {
			return nil
		}
		*results = append(*results, core.KnowledgeSearchResult{Path: rel, Title: knowledgeTitle(content, filepath.Base(path), kind), Snippet: knowledgeSnippet(content, terms), Score: score, Kind: kind})
		return nil
	})
}

func scoreKnowledgeContentForPlan(content string, plan KnowledgeSearchPlan, terms []knowledgeSearchTerm, path, kind string) int {
	if plan.ExactPhrase != "" {
		haystack := strings.ToLower(content + "\n" + path)
		if !strings.Contains(haystack, plan.ExactPhrase) {
			return 0
		}
	}
	return scoreKnowledgeContent(content, terms, path, kind)
}

func scoreKnowledgeContent(content string, terms []knowledgeSearchTerm, path, kind string) int {
	page := wiki.ParseWikiPage("knowledge", path, content)
	body := strings.ToLower(content)
	title := strings.ToLower(knowledgeTitle(content, filepath.Base(path), kind))
	aliases := frontmatterStringList(page.Frontmatter, "aliases")
	sources := page.Sources
	lowerPath := strings.ToLower(filepath.ToSlash(path))
	score := 0
	for _, term := range terms {
		count := strings.Count(body, term.text)
		if count > 5 {
			count = 5
		}
		score += count * term.weight
		if strings.Contains(lowerPath, term.text) {
			score += term.weight * 4
		}
		if strings.Contains(title, term.text) {
			score += term.weight * 20
		}
		for _, alias := range aliases {
			if strings.Contains(strings.ToLower(alias), term.text) {
				score += term.weight * 32
				break
			}
		}
		for _, source := range sources {
			if strings.Contains(strings.ToLower(source), term.text) {
				score += term.weight * 12
				break
			}
		}
	}
	if IsAggregateKnowledgePath(path) {
		score /= 4
	}
	return score
}

func knowledgeSnippet(content string, terms []knowledgeSearchTerm) string {
	lower := strings.ToLower(content)
	index := -1
	for _, term := range terms {
		if next := strings.Index(lower, term.text); next >= 0 {
			index = next
			break
		}
	}
	if index < 0 {
		return tailKnowledgeRunes(content, 320)
	}
	prefixRunes := utf8.RuneCountInString(content[:index])
	runes := []rune(content)
	start := maxKnowledgeInt(0, prefixRunes-80)
	end := minKnowledgeInt(len(runes), prefixRunes+240)
	return strings.TrimSpace(string(runes[start:end]))
}

func tailKnowledgeRunes(content string, limit int) string {
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	return string(runes[len(runes)-limit:])
}

func isKnowledgeCJKRune(r rune) bool {
	return r >= 0x3400 && r <= 0x9fff || r >= 0xf900 && r <= 0xfaff
}

var knowledgeWikiLinkPattern = regexpMustCompile(`\[\[([^\]]+)\]\]`)
var knowledgeMarkdownLinkPattern = regexpMustCompile(`\[[^\]]+\]\(([^)]+)\)`)

func extractKnowledgeLinks(content string) []string {
	seen := map[string]bool{}
	var links []string
	add := func(link string) {
		link = strings.TrimSpace(link)
		if link != "" && !seen[link] {
			seen[link] = true
			links = append(links, link)
		}
	}
	for _, match := range knowledgeWikiLinkPattern.FindAllStringSubmatch(content, -1) {
		add(strings.Split(match[1], "|")[0])
	}
	for _, match := range knowledgeMarkdownLinkPattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	return links
}

type knowledgeRegexp interface {
	FindAllStringSubmatch(string, int) [][]string
}

func regexpMustCompile(pattern string) knowledgeRegexp { return regexp.MustCompile(pattern) }

type knowledgeEntityIndex struct {
	explicitByEvidence map[string][]string
	inferredByEvidence map[string][]string
	pages              map[string]core.KnowledgeSearchResult
	identities         map[string][]string
	entitySources      map[string]map[string]bool
	evidenceSources    map[string]map[string]bool
	evidenceText       map[string]string
}

func buildKnowledgeEntityIndex(projectPath string) (knowledgeEntityIndex, error) {
	index := knowledgeEntityIndex{
		explicitByEvidence: map[string][]string{},
		inferredByEvidence: map[string][]string{},
		pages:              map[string]core.KnowledgeSearchResult{},
		identities:         map[string][]string{},
		entitySources:      map[string]map[string]bool{},
		evidenceSources:    map[string]map[string]bool{},
		evidenceText:       map[string]string{},
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil && !os.IsNotExist(err) {
		return index, err
	}
	identityPaths := map[string][]string{}
	entitiesBySource := map[string][]string{}
	for _, page := range pages {
		pagePath := normalizeKnowledgePath(page.Path)
		index.evidenceText[pagePath] = strings.TrimSpace(page.Title + "\n" + page.Body)
		if index.evidenceSources[pagePath] == nil {
			index.evidenceSources[pagePath] = map[string]bool{}
		}
		for _, source := range page.Sources {
			index.evidenceSources[pagePath][normalizeKnowledgePath(source)] = true
		}
		if !strings.EqualFold(page.Type, "entity") && !strings.HasPrefix(normalizeKnowledgePath(page.Path), "wiki/entities/") {
			continue
		}
		index.pages[page.Path] = core.KnowledgeSearchResult{Path: page.Path, Title: page.Title, Kind: "entity"}
		identities := append([]string{page.Title, strings.TrimSuffix(filepath.Base(page.Path), filepath.Ext(page.Path))}, frontmatterStringList(page.Frontmatter, "aliases")...)
		index.identities[page.Path] = identities
		index.entitySources[page.Path] = map[string]bool{}
		for _, identity := range identities {
			key := knowledgeLinkID(identity)
			identityPaths[key] = appendUniqueKnowledgeString(identityPaths[key], page.Path)
		}
		for _, source := range page.Sources {
			normalized := normalizeKnowledgePath(source)
			index.entitySources[page.Path][normalized] = true
			index.inferredByEvidence[normalized] = appendUniqueKnowledgeString(index.inferredByEvidence[normalized], page.Path)
			entitiesBySource[normalized] = appendUniqueKnowledgeString(entitiesBySource[normalized], page.Path)
		}
	}
	rawRoot := filepath.Join(projectPath, "raw", "sources")
	if _, statErr := os.Stat(rawRoot); statErr == nil {
		if walkErr := filepath.WalkDir(rawRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !utf8.Valid(data) {
				return nil
			}
			rel, relErr := filepath.Rel(projectPath, path)
			if relErr != nil {
				return relErr
			}
			rel = normalizeKnowledgePath(rel)
			index.evidenceText[rel] = string(data)
			index.evidenceSources[rel] = map[string]bool{rel: true}
			return nil
		}); walkErr != nil {
			return index, walkErr
		}
	}
	for _, page := range pages {
		for _, source := range page.Sources {
			path := normalizeKnowledgePath(page.Path)
			index.inferredByEvidence[path] = appendUniqueKnowledgeStrings(index.inferredByEvidence[path], entitiesBySource[normalizeKnowledgePath(source)]...)
		}
	}
	for _, page := range pages {
		if !strings.HasPrefix(normalizeKnowledgePath(page.Path), "wiki/sources/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(page.Path)))
		if err != nil {
			return index, err
		}
		var entities []string
		for _, link := range extractKnowledgeLinks(string(data)) {
			entities = appendUniqueKnowledgeStrings(entities, identityPaths[knowledgeLinkID(link)]...)
		}
		pagePath := normalizeKnowledgePath(page.Path)
		index.explicitByEvidence[pagePath] = appendUniqueKnowledgeStrings(index.explicitByEvidence[pagePath], entities...)
		for _, source := range page.Sources {
			sourcePath := normalizeKnowledgePath(source)
			index.explicitByEvidence[sourcePath] = appendUniqueKnowledgeStrings(index.explicitByEvidence[sourcePath], entities...)
		}
	}
	manifest, err := manifestfile.Load(projectPath)
	if err != nil {
		return index, err
	}
	for key, entry := range manifest.Sources {
		var entities []string
		for _, file := range entry.Files {
			if _, ok := index.pages[file]; ok {
				entities = appendUniqueKnowledgeString(entities, file)
			}
			entities = appendUniqueKnowledgeStrings(entities, index.inferredByEvidence[normalizeKnowledgePath(file)]...)
			entities = appendUniqueKnowledgeStrings(entities, index.explicitByEvidence[normalizeKnowledgePath(file)]...)
		}
		for _, evidence := range []string{key, entry.OriginalPath, entry.RawPath, entry.OriginalRawPath, entry.ContentPath} {
			path := normalizeKnowledgePath(evidence)
			index.inferredByEvidence[path] = appendUniqueKnowledgeStrings(index.inferredByEvidence[path], entities...)
		}
	}
	return index, nil
}

func attributedKnowledgeEntities(index knowledgeEntityIndex, result core.KnowledgeSearchResult) []string {
	if strings.EqualFold(result.Kind, "wiki-graph") {
		return nil
	}
	if _, ok := index.pages[result.Path]; ok && !strings.EqualFold(result.Kind, "wiki-graph") {
		return []string{result.Path}
	}
	var paths []string
	haystack := strings.ToLower(result.Title + "\n" + result.Snippet)
	for path, identities := range index.identities {
		for _, identity := range identities {
			identity = strings.ToLower(strings.TrimSpace(identity))
			if len([]rune(identity)) >= 2 && strings.Contains(haystack, identity) {
				paths = appendUniqueKnowledgeString(paths, path)
				break
			}
		}
	}
	if len(paths) == 0 {
		paths = appendUniqueKnowledgeStrings(paths, index.explicitByEvidence[normalizeKnowledgePath(result.Path)]...)
	}
	return paths
}

func inferredKnowledgeEntities(index knowledgeEntityIndex, result core.KnowledgeSearchResult) []string {
	paths := append([]string(nil), index.inferredByEvidence[normalizeKnowledgePath(result.Path)]...)
	if _, ok := index.pages[result.Path]; ok && strings.EqualFold(result.Kind, "wiki-graph") {
		paths = appendUniqueKnowledgeString(paths, result.Path)
	}
	return paths
}

func knowledgeCandidateReferenceScore(index knowledgeEntityIndex, candidatePath string, referenceGroups [][]string) int {
	if len(referenceGroups) == 0 {
		return 0
	}
	candidateIdentities := index.identities[candidatePath]
	if len(candidateIdentities) == 0 {
		return 0
	}
	sourceCount := len(index.entitySources[candidatePath])
	if sourceCount == 0 {
		return 0
	}
	breadthPenalty := maxKnowledgeInt(1, (sourceCount+3)/4)
	focused := sourceCount <= 8
	total := 0
	for _, references := range referenceGroups {
		best := 0
		strong := false
		for source := range index.entitySources[candidatePath] {
			text := index.evidenceText[normalizeKnowledgePath(source)]
			if text == "" || !knowledgeTextMentionsAnyEntity(index, text, references) {
				continue
			}
			score := 120
			if knowledgeTextHasEntityCooccurrence(text, candidateIdentities, index, references) {
				score += 900
				strong = focused
			} else if focused {
				strong = true
			}
			if index.entitySources[candidatePath] != nil {
				for _, referencePath := range references {
					if index.entitySources[referencePath][normalizeKnowledgePath(source)] {
						score += 180
						break
					}
				}
			}
			breadth := len(index.inferredByEvidence[normalizeKnowledgePath(source)])
			if breadth > 0 {
				score += maxKnowledgeInt(0, 240-breadth*8)
			}
			best = maxKnowledgeInt(best, score)
		}
		if best == 0 {
			continue
		}
		groupScore := best / breadthPenalty
		if strong {
			groupScore += 100000
		}
		total += groupScore
	}
	return total
}

func knowledgeTextHasEntityCooccurrence(text string, candidateIdentities []string, index knowledgeEntityIndex, referencePaths []string) bool {
	candidatePositions, _ := knowledgeIdentityMatch(text, candidateIdentities)
	if len(candidatePositions) == 0 {
		return false
	}
	for _, referencePath := range referencePaths {
		referencePositions, _ := knowledgeIdentityMatch(text, index.identities[referencePath])
		if minimumKnowledgePositionDistance(candidatePositions, referencePositions) <= 640 {
			return true
		}
	}
	return false
}

func knowledgeTextMentionsAnyEntity(index knowledgeEntityIndex, text string, paths []string) bool {
	for _, path := range paths {
		positions, _ := knowledgeIdentityMatch(text, index.identities[path])
		if len(positions) > 0 {
			return true
		}
	}
	return false
}

func discoverKnowledgeRequirementResults(ctx context.Context, projectPath, projectID, requirement string, index knowledgeEntityIndex, store SearchEvidenceStore, embeddingProvider EmbeddingProvider) ([]core.KnowledgeSearchResult, error) {
	primary, err := SearchProjectDocuments(ctx, projectPath, projectID, requirement, 40, store, embeddingProvider)
	if err != nil {
		return nil, err
	}
	expandedQuery := expandedKnowledgeRequirementQuery(index, requirement)
	if expandedQuery == requirement {
		return primary, nil
	}
	plan, err := normalizeKnowledgeSearchPlan(expandedQuery, 50)
	if err != nil {
		return nil, err
	}
	expanded, err := searchProjectFiles(projectPath, plan)
	if err != nil {
		return nil, err
	}
	return fuseKnowledgeRankings([][]core.KnowledgeSearchResult{primary, expanded}, 50), nil
}

func expandedKnowledgeRequirementQuery(index knowledgeEntityIndex, requirement string) string {
	references := referencedKnowledgeEntities(index, requirement)
	if len(references) == 0 {
		return requirement
	}
	wantsCJK := len(knowledgeCJKRuns(requirement)) > 0
	aliases := make([]string, 0, 12)
	for _, path := range references {
		for _, identity := range index.identities[path] {
			identity = strings.TrimSpace(identity)
			if !usableKnowledgeIdentity(identity) || containsKnowledgeIdentity(requirement, identity) {
				continue
			}
			if wantsCJK != (len(knowledgeCJKRuns(identity)) > 0) {
				continue
			}
			aliases = appendUniqueKnowledgeString(aliases, identity)
			if len(aliases) >= 12 {
				break
			}
		}
		if len(aliases) >= 12 {
			break
		}
	}
	if len(aliases) == 0 {
		return requirement
	}
	return strings.TrimSpace(requirement + " " + strings.Join(aliases, " "))
}

func attributedKnowledgeRequirementEntities(projectPath string, index knowledgeEntityIndex, result core.KnowledgeSearchResult, requirement string) ([]string, error) {
	content := strings.TrimSpace(result.Title + "\n" + result.Snippet)
	if rel, err := normalizeKnowledgeToolPath(result.Path); err == nil {
		if data, readErr := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(rel))); readErr == nil {
			content = string(data)
			if strings.HasPrefix(normalizeKnowledgePath(rel), "wiki/") {
				content = wiki.ParseWikiPage("knowledge-discover", rel, content).Body
			}
		}
	}
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	references := referencedKnowledgeEntities(index, requirement)
	referenceSet := make(map[string]bool, len(references))
	for _, path := range references {
		referenceSet[path] = true
	}
	terms := knowledgeQueryTerms(requirement)
	eventTerms := knowledgeRequirementEventTerms(index, references, terms)
	type candidateScore struct {
		path  string
		score int
	}
	scores := map[string]int{}
	for _, segment := range knowledgeEvidenceSegments(content) {
		anchorPositions := []int(nil)
		if len(references) > 0 {
			for _, path := range references {
				positions, _ := knowledgeIdentityMatch(segment, index.identities[path])
				anchorPositions = append(anchorPositions, positions...)
			}
		} else {
			anchorPositions = knowledgeTermPositions(segment, terms)
		}
		if len(anchorPositions) == 0 {
			continue
		}
		eventPositions := knowledgeTermPositions(segment, eventTerms)
		for path, identities := range index.identities {
			if referenceSet[path] {
				continue
			}
			positions, identityLength := knowledgeIdentityMatch(segment, knowledgeAttributionIdentities(index, path, result.Path, identities))
			implicit := path == result.Path
			if len(positions) == 0 && !implicit {
				continue
			}
			if len(positions) == 0 {
				positions = anchorPositions
			}
			distance := minimumKnowledgePositionDistance(positions, anchorPositions)
			score := 200 + minKnowledgeInt(identityLength, 40)*10 + maxKnowledgeInt(0, 160-minKnowledgeInt(distance, 160))
			if len(eventPositions) > 0 {
				score += 1000
			}
			if implicit {
				score += 300
			}
			if score > scores[path] {
				scores[path] = score
			}
		}
	}
	items := make([]candidateScore, 0, len(scores))
	for path, score := range scores {
		items = append(items, candidateScore{path: path, score: score})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].path < items[j].path
	})
	if len(items) > 4 {
		items = items[:4]
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.path)
	}
	return paths, nil
}

func referencedKnowledgeEntities(index knowledgeEntityIndex, requirement string) []string {
	var paths []string
	for path, identities := range index.identities {
		for _, identity := range identities {
			if usableKnowledgeIdentity(identity) && containsKnowledgeIdentity(requirement, identity) {
				paths = append(paths, path)
				break
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func usableKnowledgeIdentity(identity string) bool {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return false
	}
	length := len([]rune(identity))
	if len(knowledgeCJKRuns(identity)) > 0 {
		return length >= 2
	}
	return length >= 3
}

func containsKnowledgeIdentity(content, identity string) bool {
	positions, _ := knowledgeIdentityMatch(content, []string{identity})
	return len(positions) > 0
}

func knowledgeRequirementEventTerms(index knowledgeEntityIndex, references []string, terms []knowledgeSearchTerm) []knowledgeSearchTerm {
	if len(references) == 0 {
		return terms
	}
	var identities []string
	for _, path := range references {
		for _, identity := range index.identities[path] {
			identity = strings.ToLower(strings.TrimSpace(identity))
			if usableKnowledgeIdentity(identity) {
				identities = appendUniqueKnowledgeString(identities, identity)
			}
		}
	}
	eventTerms := make([]knowledgeSearchTerm, 0, len(terms))
	for _, term := range terms {
		if len([]rune(term.text)) < 2 {
			continue
		}
		identityOnly := false
		for _, identity := range identities {
			if strings.Contains(identity, term.text) {
				identityOnly = true
				break
			}
		}
		if !identityOnly {
			eventTerms = append(eventTerms, term)
		}
	}
	return eventTerms
}

func knowledgeEvidenceSegments(content string) []string {
	segments := strings.FieldsFunc(content, func(r rune) bool {
		switch r {
		case '\n', '\r', '。', '！', '？', '；', '.', '!', '?', ';':
			return true
		default:
			return false
		}
	})
	filtered := segments[:0]
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment != "" {
			filtered = append(filtered, segment)
		}
	}
	return filtered
}

func knowledgeAttributionIdentities(index knowledgeEntityIndex, entityPath, evidencePath string, identities []string) []string {
	if knowledgeEntitySupportsEvidence(index, entityPath, evidencePath) {
		return identities
	}
	strong := make([]string, 0, len(identities))
	for _, identity := range identities {
		length := len([]rune(strings.TrimSpace(identity)))
		if len(knowledgeCJKRuns(identity)) > 0 {
			if length < 3 {
				continue
			}
		} else if length < 5 {
			continue
		}
		strong = append(strong, identity)
	}
	return strong
}

func knowledgeEntitySupportsEvidence(index knowledgeEntityIndex, entityPath, evidencePath string) bool {
	sources := index.entitySources[entityPath]
	if len(sources) == 0 {
		return false
	}
	evidencePath = normalizeKnowledgePath(evidencePath)
	if sources[evidencePath] {
		return true
	}
	for source := range index.evidenceSources[evidencePath] {
		if sources[source] {
			return true
		}
	}
	return false
}

func knowledgeTermPositions(content string, terms []knowledgeSearchTerm) []int {
	lower := strings.ToLower(content)
	var positions []int
	for _, term := range terms {
		if len([]rune(term.text)) < 2 {
			continue
		}
		for offset := 0; offset < len(lower); {
			index := strings.Index(lower[offset:], term.text)
			if index < 0 {
				break
			}
			position := offset + index
			positions = append(positions, position)
			offset = position + len(term.text)
		}
	}
	return positions
}

func knowledgeIdentityMatch(content string, identities []string) ([]int, int) {
	lower := strings.ToLower(content)
	var positions []int
	longest := 0
	for _, identity := range identities {
		identity = strings.ToLower(strings.TrimSpace(identity))
		if !usableKnowledgeIdentity(identity) {
			continue
		}
		for offset := 0; offset < len(lower); {
			index := strings.Index(lower[offset:], identity)
			if index < 0 {
				break
			}
			position := offset + index
			end := position + len(identity)
			if knowledgeIdentityBoundary(lower, identity, position, end) {
				positions = append(positions, position)
				longest = maxKnowledgeInt(longest, len([]rune(identity)))
			}
			offset = end
		}
	}
	return positions, longest
}

func knowledgeIdentityBoundary(content, identity string, start, end int) bool {
	if identity == "" || len(knowledgeCJKRuns(identity)) > 0 {
		return true
	}
	if start > 0 {
		before, _ := utf8.DecodeLastRuneInString(content[:start])
		if unicode.IsLetter(before) || unicode.IsDigit(before) {
			return false
		}
	}
	if end < len(content) {
		after, _ := utf8.DecodeRuneInString(content[end:])
		if unicode.IsLetter(after) || unicode.IsDigit(after) {
			return false
		}
	}
	return true
}

func minimumKnowledgePositionDistance(left, right []int) int {
	if len(left) == 0 || len(right) == 0 {
		return 1000
	}
	minimum := 1000
	for _, leftPosition := range left {
		for _, rightPosition := range right {
			distance := leftPosition - rightPosition
			if distance < 0 {
				distance = -distance
			}
			minimum = minKnowledgeInt(minimum, distance)
		}
	}
	return minimum
}

func containsKnowledgePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func knowledgeLinkID(value string) string {
	value = strings.TrimSpace(strings.Split(strings.Split(value, "|")[0], "#")[0])
	value = strings.TrimSuffix(filepath.ToSlash(value), ".md")
	value = strings.TrimPrefix(value, "wiki/")
	return strings.ToLower(strings.Trim(value, "/"))
}

func appendUniqueKnowledgeString(items []string, value string) []string {
	value = strings.TrimSpace(value)
	for _, item := range items {
		if item == value {
			return items
		}
	}
	if value != "" {
		items = append(items, value)
	}
	return items
}

func appendUniqueKnowledgeStrings(items []string, values ...string) []string {
	for _, value := range values {
		items = appendUniqueKnowledgeString(items, value)
	}
	return items
}

func searchKnowledgeCodeGraph(ctx context.Context, projectPath, projectID string, store GraphEvidenceStore, query string, limit int) ([]KnowledgeDocument, error) {
	if store != nil && strings.TrimSpace(projectID) != "" {
		evidence, err := store.SearchGraphEvidence(ctx, projectID, query, limit)
		if err != nil {
			return nil, err
		}
		if len(evidence) > 0 {
			docs := make([]KnowledgeDocument, 0, len(evidence))
			for _, item := range evidence {
				docs = append(docs, KnowledgeDocument{Path: item.Path, Title: item.Title, Kind: "code-graph", Content: item.Content})
			}
			return docs, nil
		}
	}
	terms := knowledgeQueryTerms(query)
	type match struct {
		path, title, content string
		score                int
	}
	var matches []match
	root := filepath.Join(projectPath, "raw", "code-graphs")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Base(path) != "graph.json" {
			return walkErr
		}
		rel, _ := filepath.Rel(projectPath, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		repoID := "code"
		if len(parts) >= 3 {
			repoID = parts[2]
		}
		snapshot, err := codegraph.ImportGraphify(repoID, "", path, "")
		if err != nil {
			return err
		}
		for _, node := range snapshot.Nodes {
			content := fmt.Sprintf("repo=%s id=%s kind=%s label=%s source_file=%s props=%v", repoID, node.ID, node.Kind, node.Label, node.SourceFile, node.Props)
			score := scoreKnowledgeContent(content, terms, filepath.ToSlash(rel), "code-graph")
			if score > 0 {
				matches = append(matches, match{path: filepath.ToSlash(rel) + "#" + node.ID, title: node.Label, content: content, score: score})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	docs := make([]KnowledgeDocument, 0, len(matches))
	for _, item := range matches {
		docs = append(docs, KnowledgeDocument{Path: item.path, Title: item.title, Kind: "code-graph", Content: item.content})
	}
	return docs, nil
}

func appendKnowledgeDocuments(base, extra []KnowledgeDocument) []KnowledgeDocument {
	seen := map[string]bool{}
	for _, doc := range base {
		seen[doc.Path] = true
	}
	for _, doc := range extra {
		if doc.Path != "" && !seen[doc.Path] {
			seen[doc.Path] = true
			base = append(base, doc)
		}
	}
	return base
}

func frontmatterStringList(frontmatter map[string]any, key string) []string {
	value, ok := frontmatter[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				items = append(items, text)
			}
		}
		return items
	case string:
		text := strings.TrimSpace(strings.Trim(typed, "[]"))
		if text == "" {
			return nil
		}
		parts := strings.Split(text, ",")
		items := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.Trim(strings.TrimSpace(part), `"'`); part != "" {
				items = append(items, part)
			}
		}
		return items
	default:
		return nil
	}
}

func minKnowledgeInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxKnowledgeInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
