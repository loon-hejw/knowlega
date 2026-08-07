package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hejw/qm-backend/internal/data"
	"github.com/jackc/pgx/v5"
)

type ScopeRef struct {
	OrgID           string
	ExternalScopeID string
	Kind            string
	Name            string
}

type ScopeStatus struct {
	Scope         data.KnowledgeScope
	WikiPageCount int64
	SourceCount   int64
	Ready         bool
}

type Document struct {
	Path    string
	Title   string
	Kind    string
	Content string
}

type SearchResult struct {
	Path    string
	Title   string
	Snippet string
	Score   int
	Kind    string
}

type QueryResult struct {
	ID                      string
	Question                string
	Answer                  string
	Citations               []Document
	SuggestedWritebackTitle string
	IncompleteReason        string
	SearchResults           []SearchResult
	QueryPlan               string
	CanWriteBack            bool
}

type IngestResult struct {
	RawPath          string
	SkippedUnchanged bool
	GeneratedPaths   []string
	Reviews          []string
}

type Engine struct {
	root    string
	scopes  *data.KnowledgeScopeRepository
	queries *data.KnowledgeQueryRepository
}

func New(root string, scopes *data.KnowledgeScopeRepository, queries *data.KnowledgeQueryRepository) (*Engine, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("knowledge root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Engine{root: abs, scopes: scopes, queries: queries}, nil
}

func (e *Engine) EnsureScope(ctx context.Context, ref ScopeRef) (ScopeStatus, error) {
	if err := validateScope(ref); err != nil {
		return ScopeStatus{}, err
	}
	projectID := stableID(ref.OrgID, ref.Kind, ref.ExternalScopeID)
	root, err := e.scopeRoot(projectID)
	if err != nil {
		return ScopeStatus{}, err
	}
	if err := initializeWorkspace(root, ref); err != nil {
		return ScopeStatus{}, err
	}
	scope, err := e.scopes.Ensure(ctx, data.KnowledgeScope{OrgID: ref.OrgID, ExternalScopeID: ref.ExternalScopeID, Kind: ref.Kind, ProjectID: projectID, ProjectName: scopeName(ref), RootPath: root})
	if err != nil {
		return ScopeStatus{}, err
	}
	return e.status(scope)
}

func (e *Engine) GetStatus(ctx context.Context, ref ScopeRef) (ScopeStatus, error) {
	if err := validateScope(ref); err != nil {
		return ScopeStatus{}, err
	}
	scope, err := e.scopes.Get(ctx, ref.OrgID, ref.ExternalScopeID, ref.Kind)
	if err != nil {
		return ScopeStatus{}, err
	}
	return e.status(scope)
}

func (e *Engine) Search(ctx context.Context, ref ScopeRef, query string, limit int) ([]SearchResult, error) {
	status, err := e.GetStatus(ctx, ref)
	if err != nil {
		return nil, err
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil, errors.New("query is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	results := []SearchResult{}
	for _, dir := range []string{"wiki", "raw/sources"} {
		base := filepath.Join(status.Scope.RootPath, dir)
		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			text := string(content)
			lower := strings.ToLower(text)
			score := 0
			for _, term := range terms {
				score += strings.Count(lower, term)
			}
			if score == 0 {
				return nil
			}
			rel, relErr := filepath.Rel(status.Scope.RootPath, path)
			if relErr != nil {
				return nil
			}
			kind := "source"
			if strings.HasPrefix(filepath.ToSlash(rel), "wiki/") {
				kind = "wiki"
			}
			results = append(results, SearchResult{Path: filepath.ToSlash(rel), Title: titleFor(filepath.ToSlash(rel), text), Snippet: snippet(lower, text, terms[0]), Score: score, Kind: kind})
			return nil
		})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Path < results[j].Path
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (e *Engine) ReadDocument(ctx context.Context, ref ScopeRef, requestedPath string) (Document, error) {
	status, err := e.GetStatus(ctx, ref)
	if err != nil {
		return Document{}, err
	}
	path, err := normalizeDocumentPath(requestedPath)
	if err != nil {
		return Document{}, err
	}
	absolute, err := safeJoin(status.Scope.RootPath, path)
	if err != nil {
		return Document{}, err
	}
	contents, err := os.ReadFile(absolute)
	if err != nil {
		return Document{}, err
	}
	kind := "source"
	if strings.HasPrefix(path, "wiki/") {
		kind = "wiki"
	}
	return Document{Path: path, Title: titleFor(path, string(contents)), Kind: kind, Content: string(contents)}, nil
}

func (e *Engine) Query(ctx context.Context, ref ScopeRef, question, conversationContext string, limit int, agent QueryAgent) (QueryResult, error) {
	if strings.TrimSpace(question) == "" {
		return QueryResult{}, errors.New("question is required")
	}
	if agent == nil {
		return QueryResult{}, errors.New("knowledge query requires an LLM Wiki query agent")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	status, err := e.GetStatus(ctx, ref)
	if err != nil {
		return QueryResult{}, err
	}
	readPaths := []string{"purpose.md", "schema.md", "wiki/index.md", "wiki/overview.md"}
	documents := make([]Document, 0, 4+limit)
	for _, path := range readPaths {
		document, err := e.readWorkspaceDocument(status.Scope.RootPath, path)
		if err != nil {
			return QueryResult{}, err
		}
		documents = append(documents, document)
	}
	seen := map[string]bool{}
	for _, path := range readPaths {
		seen[path] = true
	}
	actions := []QueryAction{{Kind: "search", Query: question}, {Kind: "final"}}
	if planner, ok := agent.(QueryPlanner); ok {
		actions, err = planner.Plan(ctx, QueryPlanningPrompt{Question: question, ConversationContext: conversationContext, Navigation: append([]Document(nil), documents...)})
		if err != nil {
			return QueryResult{}, err
		}
	}
	results := []SearchResult{}
	planSteps := []string{"read: purpose.md, schema.md, wiki/index.md, wiki/overview.md"}
queryLoop:
	for index, action := range actions {
		if index >= 6 {
			break
		}
		switch action.Kind {
		case "final":
			planSteps = append(planSteps, "final")
			break queryLoop
		case "read":
			document, err := e.ReadDocument(ctx, ref, action.Path)
			if err != nil {
				return QueryResult{}, err
			}
			if !seen[document.Path] {
				documents = append(documents, document)
				seen[document.Path] = true
			}
			planSteps = append(planSteps, "read: "+document.Path)
		case "search":
			query := strings.TrimSpace(action.Query)
			if query == "" {
				return QueryResult{}, errors.New("knowledge planner search action requires a query")
			}
			candidates, err := e.Search(ctx, ref, query, limit)
			if err != nil {
				return QueryResult{}, err
			}
			results = append(results, candidates...)
			readCandidates := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				if seen[candidate.Path] {
					continue
				}
				document, err := e.ReadDocument(ctx, ref, candidate.Path)
				if err != nil {
					return QueryResult{}, err
				}
				documents = append(documents, document)
				seen[document.Path] = true
				readCandidates = append(readCandidates, document.Path)
			}
			planSteps = append(planSteps, "search: "+query+"; read: "+strings.Join(readCandidates, ", "))
		default:
			return QueryResult{}, fmt.Errorf("knowledge planner returned unsupported action %q", action.Kind)
		}
	}
	nonNavigationRead := false
	for _, document := range documents {
		if !isNavigationPath(document.Path) {
			nonNavigationRead = true
			break
		}
	}
	if !nonNavigationRead {
		return QueryResult{}, errors.New("knowledge planner must read non-navigation evidence before final answer")
	}
	draft, err := agent.Answer(ctx, QueryPrompt{Question: question, ConversationContext: conversationContext, Documents: documents})
	if err != nil {
		return QueryResult{}, err
	}
	byPath := make(map[string]Document, len(documents))
	for _, document := range documents {
		byPath[document.Path] = document
	}
	citations := make([]Document, 0, len(draft.CitationPaths))
	for _, path := range draft.CitationPaths {
		document, ok := byPath[path]
		if !ok {
			return QueryResult{}, fmt.Errorf("knowledge LLM cited unread document %q", path)
		}
		citations = append(citations, document)
	}
	if len(citations) == 0 && strings.TrimSpace(draft.IncompleteReason) == "" {
		return QueryResult{}, errors.New("knowledge LLM answer requires citations from read evidence")
	}
	plan := strings.Join(planSteps, "\n")
	canWriteBack := false
	for _, citation := range citations {
		if !isNavigationPath(citation.Path) {
			canWriteBack = true
			break
		}
	}
	if e.queries == nil {
		return QueryResult{}, errors.New("knowledge query log store is required")
	}
	paths := make([]string, 0, len(citations))
	for _, citation := range citations {
		paths = append(paths, citation.Path)
	}
	run, err := e.queries.Create(ctx, data.KnowledgeQueryRun{OrgID: ref.OrgID, ExternalScopeID: ref.ExternalScopeID, Kind: ref.Kind, Question: question, Answer: draft.Answer, CitationPaths: paths, QueryPlan: plan, CanWriteBack: canWriteBack})
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{ID: run.ID, Question: question, Answer: draft.Answer, Citations: citations, SuggestedWritebackTitle: draft.SuggestedWritebackTitle, IncompleteReason: draft.IncompleteReason, SearchResults: results, QueryPlan: plan, CanWriteBack: canWriteBack}, nil
}

func (e *Engine) SaveQueryAnswer(ctx context.Context, ref ScopeRef, queryID, title string) (string, error) {
	if strings.TrimSpace(queryID) == "" || strings.TrimSpace(title) == "" {
		return "", errors.New("query_id and title are required")
	}
	if e.queries == nil {
		return "", errors.New("knowledge query log store is required")
	}
	query, err := e.queries.Get(ctx, queryID, ref.OrgID, ref.ExternalScopeID, ref.Kind)
	if err != nil {
		return "", err
	}
	if !query.CanWriteBack {
		return "", errors.New("query answer cannot be written back without non-navigation evidence")
	}
	status, err := e.GetStatus(ctx, ref)
	if err != nil {
		return "", err
	}
	citations := make([]Document, 0, len(query.CitationPaths))
	for _, path := range query.CitationPaths {
		document, err := e.readCitation(status.Scope.RootPath, path)
		if err != nil {
			return "", err
		}
		citations = append(citations, document)
	}
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return '-'
	}, title), "-")
	if slug == "" {
		return "", errors.New("title must contain letters or numbers")
	}
	path := "wiki/syntheses/" + slug + ".md"
	title = strings.TrimSpace(title)
	var builder strings.Builder
	builder.WriteString("---\ntitle: ")
	builder.WriteString(strconv.Quote(strings.ReplaceAll(title, "\n", " ")))
	builder.WriteString("\ntype: synthesis\nsources:\n")
	for _, citation := range citations {
		builder.WriteString("  - ")
		builder.WriteString(citation.Path)
		builder.WriteString("\n")
	}
	builder.WriteString("---\n\n# ")
	builder.WriteString(title)
	builder.WriteString("\n\n## Question\n\n")
	builder.WriteString(query.Question)
	builder.WriteString("\n\n## Answer\n\n")
	builder.WriteString(query.Answer)
	builder.WriteString("\n\n## Evidence\n\n")
	for _, citation := range citations {
		builder.WriteString("- [[")
		builder.WriteString(citation.Title)
		builder.WriteString("]] (" + citation.Path + ")\n")
	}
	builder.WriteString("\n## Query Plan\n\n")
	builder.WriteString(query.QueryPlan)
	builder.WriteString("\n")
	if err := e.versionedWrite(status.Scope.RootPath, path, []byte(builder.String())); err != nil {
		return "", err
	}
	if err := e.updateNavigationForSynthesis(status.Scope.RootPath, title); err != nil {
		return "", err
	}
	return path, nil
}

func (e *Engine) Ingest(ctx context.Context, ref ScopeRef, sourceName string, contents []byte, agent CompilerAgent) (IngestResult, error) {
	if len(contents) == 0 {
		return IngestResult{}, errors.New("source content is required")
	}
	if agent == nil {
		return IngestResult{}, errors.New("knowledge ingest requires an LLM Wiki compiler agent")
	}
	status, err := e.GetStatus(ctx, ref)
	if err != nil {
		return IngestResult{}, err
	}
	rawPath, err := sourcePath(sourceName, contents)
	if err != nil {
		return IngestResult{}, err
	}
	rawAbsolute, err := safeJoin(status.Scope.RootPath, rawPath)
	if err != nil {
		return IngestResult{}, err
	}
	processed, err := manifestHasSource(status.Scope.RootPath, rawPath)
	if err != nil {
		return IngestResult{}, err
	}
	if processed {
		return IngestResult{RawPath: rawPath, SkippedUnchanged: true}, nil
	}
	if _, err := os.Stat(rawAbsolute); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(rawAbsolute, contents, 0o440); err != nil {
			return IngestResult{}, err
		}
	} else if err != nil {
		return IngestResult{}, err
	}
	navigation := make([]Document, 0, 4)
	for _, path := range []string{"purpose.md", "schema.md", "wiki/index.md", "wiki/overview.md"} {
		document, err := e.readWorkspaceDocument(status.Scope.RootPath, path)
		if err != nil {
			return IngestResult{}, err
		}
		navigation = append(navigation, document)
	}
	draft, err := agent.Compile(ctx, CompilePrompt{SourcePath: rawPath, SourceText: string(contents), Documents: navigation})
	if err != nil {
		return IngestResult{}, err
	}
	if err := validateCompilation(draft, rawPath); err != nil {
		return IngestResult{}, err
	}
	generated := make([]string, 0, len(draft.Pages))
	for _, page := range draft.Pages {
		absolute, err := safeJoin(status.Scope.RootPath, page.Path)
		if err != nil {
			return IngestResult{}, err
		}
		if previous, err := os.ReadFile(absolute); err == nil {
			if err := archivePage(status.Scope.RootPath, page.Path, previous); err != nil {
				return IngestResult{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return IngestResult{}, err
		}
		if err := os.WriteFile(absolute, []byte(page.Content), 0o640); err != nil {
			return IngestResult{}, err
		}
		generated = append(generated, page.Path)
	}
	if err := appendManifest(status.Scope.RootPath, rawPath, generated); err != nil {
		return IngestResult{}, err
	}
	if len(draft.Reviews) > 0 {
		if err := appendReviews(status.Scope.RootPath, rawPath, draft.Reviews); err != nil {
			return IngestResult{}, err
		}
	}
	return IngestResult{RawPath: rawPath, GeneratedPaths: generated, Reviews: draft.Reviews}, nil
}

func (e *Engine) scopeRoot(projectID string) (string, error) {
	return safeJoin(e.root, filepath.Join("scopes", projectID))
}

func (e *Engine) readWorkspaceDocument(root, requestedPath string) (Document, error) {
	absolute, err := safeJoin(root, requestedPath)
	if err != nil {
		return Document{}, err
	}
	contents, err := os.ReadFile(absolute)
	if err != nil {
		return Document{}, err
	}
	kind := "navigation"
	if strings.HasPrefix(requestedPath, "wiki/") {
		kind = "wiki"
	}
	return Document{Path: requestedPath, Title: titleFor(requestedPath, string(contents)), Kind: kind, Content: string(contents)}, nil
}

func (e *Engine) readCitation(root, path string) (Document, error) {
	if isNavigationPath(path) {
		return Document{}, errors.New("navigation documents cannot be used as writeback evidence")
	}
	if !strings.HasPrefix(path, "wiki/") && !strings.HasPrefix(path, "raw/sources/") {
		return Document{}, errors.New("citation path must be under wiki/ or raw/sources/")
	}
	return e.readWorkspaceDocument(root, path)
}

func (e *Engine) versionedWrite(root, path string, contents []byte) error {
	absolute, err := safeJoin(root, path)
	if err != nil {
		return err
	}
	if previous, err := os.ReadFile(absolute); err == nil {
		if err := archivePage(root, path, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(absolute, contents, 0o640)
}

func (e *Engine) updateNavigationForSynthesis(root, title string) error {
	for _, path := range []string{"wiki/index.md", "wiki/overview.md"} {
		document, err := e.readWorkspaceDocument(root, path)
		if err != nil {
			return err
		}
		link := "[[" + title + "]]"
		if strings.Contains(document.Content, link) {
			continue
		}
		updated := document.Content
		if path == "wiki/index.md" {
			marker := "## Syntheses"
			if index := strings.Index(updated, marker); index >= 0 {
				insertAt := index + len(marker)
				updated = updated[:insertAt] + "\n\n- " + link + updated[insertAt:]
			} else {
				updated += "\n\n## Syntheses\n\n- " + link + "\n"
			}
		} else {
			updated += "\n\nLatest synthesis: " + link + "\n"
		}
		if err := e.versionedWrite(root, path, []byte(updated)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) status(scope data.KnowledgeScope) (ScopeStatus, error) {
	pages, err := countFiles(filepath.Join(scope.RootPath, "wiki"))
	if err != nil {
		return ScopeStatus{}, err
	}
	sources, err := countFiles(filepath.Join(scope.RootPath, "raw", "sources"))
	if err != nil {
		return ScopeStatus{}, err
	}
	return ScopeStatus{Scope: scope, WikiPageCount: pages, SourceCount: sources, Ready: scope.Status == "active"}, nil
}

func initializeWorkspace(root string, ref ScopeRef) error {
	for _, dir := range []string{"raw/sources", "wiki/sources", "wiki/concepts", "wiki/entities", "wiki/syntheses", ".kbcore/page-versions"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			return err
		}
	}
	files := map[string]string{
		"purpose.md":       fmt.Sprintf("# Purpose\n\nPersistent LLM-maintained knowledge workspace for %s.\n", scopeName(ref)),
		"schema.md":        "# Schema\n\nGenerated wiki pages use YAML frontmatter, source provenance, and [[wikilinks]].\n",
		"wiki/index.md":    "---\ntitle: Knowledge Index\ntype: index\n---\n\n# Knowledge Index\n\n## Sources\n\n## Concepts\n\n## Entities\n\n## Syntheses\n",
		"wiki/overview.md": "---\ntitle: Overview\ntype: overview\n---\n\n# Overview\n\nNo sources have been compiled yet.\n",
		"wiki/log.md":      "# Operational Log\n",
		"wiki/reviews.md":  "# Reviews\n",
	}
	for path, content := range files {
		absolute := filepath.Join(root, path)
		if _, err := os.Stat(absolute); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(absolute, []byte(content), 0o640); err != nil {
			return err
		}
	}
	return nil
}

func validateScope(ref ScopeRef) error {
	if strings.TrimSpace(ref.OrgID) == "" || strings.TrimSpace(ref.ExternalScopeID) == "" || strings.TrimSpace(ref.Kind) == "" {
		return errors.New("org_id, external_scope_id, and scope kind are required")
	}
	return nil
}

func stableID(values ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(hash[:16])
}

func scopeName(ref ScopeRef) string {
	if strings.TrimSpace(ref.Name) != "" {
		return strings.TrimSpace(ref.Name)
	}
	return ref.Kind + ":" + ref.ExternalScopeID
}

func safeJoin(root, relative string) (string, error) {
	path := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace root")
	}
	return path, nil
}

func normalizeDocumentPath(requestedPath string) (string, error) {
	path := filepath.ToSlash(strings.TrimSpace(strings.ReplaceAll(requestedPath, "\\", "/")))
	if path == "" {
		return "", errors.New("document path is required")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes workspace root")
	}
	if !strings.HasPrefix(clean, "wiki/") && !strings.HasPrefix(clean, "raw/sources/") {
		return "", errors.New("path must be under wiki/ or raw/sources/")
	}
	return clean, nil
}

func countFiles(root string) (int64, error) {
	var count int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

func titleFor(path, content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "title:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "title:")), "\"'")
		}
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func snippet(lower, original, term string) string {
	position := strings.Index(lower, term)
	if position < 0 {
		return ""
	}
	start := max(0, position-120)
	end := min(len(original), position+len(term)+180)
	return strings.TrimSpace(original[start:end])
}

func queryTerms(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

func isNavigationPath(path string) bool {
	return path == "purpose.md" || path == "schema.md" || path == "wiki/index.md" || path == "wiki/overview.md" || path == "wiki/log.md" || path == "wiki/reviews.md"
}

func sourcePath(name string, contents []byte) (string, error) {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." {
		return "", errors.New("source name is required")
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, base)
	base = strings.Trim(base, "-_")
	if base == "" {
		base = "source"
	}
	hash := sha256.Sum256(contents)
	return "raw/sources/" + base + "-" + hex.EncodeToString(hash[:8]) + ".md", nil
}

func validateCompilation(draft CompileDraft, rawPath string) error {
	if len(draft.Pages) == 0 {
		return errors.New("knowledge LLM compilation returned no pages")
	}
	required := map[string]bool{"wiki/index.md": false, "wiki/overview.md": false, "wiki/log.md": false}
	hasSourceSummary := false
	seen := map[string]bool{}
	for _, page := range draft.Pages {
		if seen[page.Path] || !strings.HasPrefix(page.Path, "wiki/") || !strings.HasSuffix(page.Path, ".md") {
			return fmt.Errorf("invalid generated wiki path %q", page.Path)
		}
		seen[page.Path] = true
		if !strings.HasPrefix(page.Content, "---\n") || !strings.Contains(page.Content, "\nsources:") || !strings.Contains(page.Content, rawPath) || !strings.Contains(page.Content, "[[") {
			return fmt.Errorf("generated page %q lacks required frontmatter, source provenance, or wikilink", page.Path)
		}
		if _, ok := required[page.Path]; ok {
			required[page.Path] = true
		}
		if strings.HasPrefix(page.Path, "wiki/sources/") {
			hasSourceSummary = true
		}
	}
	for path, present := range required {
		if !present {
			return fmt.Errorf("knowledge LLM compilation must update %s", path)
		}
	}
	if !hasSourceSummary {
		return errors.New("knowledge LLM compilation must generate a source summary")
	}
	return nil
}

func archivePage(root, path string, previous []byte) error {
	rel := strings.TrimPrefix(filepath.ToSlash(path), "wiki/")
	archive, err := safeJoin(root, filepath.Join(".kbcore/page-versions", rel+"."+time.Now().UTC().Format("20060102T150405.000000000Z")+".md"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(archive), 0o750); err != nil {
		return err
	}
	return os.WriteFile(archive, previous, 0o640)
}

func appendManifest(root, rawPath string, generated []string) error {
	type manifestEntry struct {
		SHA256         string   `json:"sha256"`
		GeneratedPages []string `json:"generated_pages"`
	}
	type manifest struct {
		Sources map[string]manifestEntry `json:"sources"`
	}
	path, err := safeJoin(root, ".kbcore/source-manifest.json")
	if err != nil {
		return err
	}
	state := manifest{Sources: map[string]manifestEntry{}}
	if contents, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(contents, &state); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Sources == nil {
		state.Sources = map[string]manifestEntry{}
	}
	raw, err := safeJoin(root, rawPath)
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(raw)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(contents)
	state.Sources[rawPath] = manifestEntry{SHA256: hex.EncodeToString(hash[:]), GeneratedPages: generated}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o640)
}

func manifestHasSource(root, rawPath string) (bool, error) {
	path, err := safeJoin(root, ".kbcore/source-manifest.json")
	if err != nil {
		return false, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state struct {
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(contents, &state); err != nil {
		return false, err
	}
	_, ok := state.Sources[rawPath]
	return ok, nil
}

func appendReviews(root, rawPath string, reviews []string) error {
	path, err := safeJoin(root, "wiki/reviews.md")
	if err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("\n## ")
	builder.WriteString(rawPath)
	builder.WriteString("\n")
	for _, review := range reviews {
		builder.WriteString("- ")
		builder.WriteString(strings.TrimSpace(review))
		builder.WriteString("\n")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(builder.String())
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func IsMissing(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
