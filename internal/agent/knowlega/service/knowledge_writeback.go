package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type KnowledgeWritebackOptions struct {
	Context           context.Context
	ProjectPath       string
	ProjectID         string
	Title             string
	Submission        core.KnowledgeSubmission
	WikiStore         WikiPageStore
	EmbeddingProvider EmbeddingProvider
}

type KnowledgeWritebackResult struct {
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Written   bool     `json:"written"`
	Synced    bool     `json:"synced"`
	WikiPaths []string `json:"wiki_paths"`
}

// WriteKnowledgeSubmission persists only an explicitly submitted, complete,
// evidence-backed synthesis. Repeating identical content is idempotent.
func WriteKnowledgeSubmission(opts KnowledgeWritebackOptions) (KnowledgeWritebackResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return KnowledgeWritebackResult{}, fmt.Errorf("project path is required")
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if strings.TrimSpace(opts.Submission.Answer) == "" || strings.TrimSpace(opts.Submission.Question) == "" {
		return KnowledgeWritebackResult{}, fmt.Errorf("complete submitted question and answer are required")
	}
	if opts.Submission.Status != "complete" || len(opts.Submission.UnresolvedRequirementIDs) > 0 {
		return KnowledgeWritebackResult{}, fmt.Errorf("only a complete knowledge submission can be written back")
	}
	if !submissionHasDurableCitation(opts.Submission.Citations) {
		return KnowledgeWritebackResult{}, fmt.Errorf("knowledge writeback requires a non-aggregate evidence citation")
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "Knowledge synthesis: " + strings.TrimSpace(opts.Submission.Question)
	}
	slug := core.Slug(title)
	if slug == "untitled" {
		slug = "synthesis-" + core.StableID(opts.Submission.Question, opts.Submission.Answer)[:12]
	}
	rel := filepath.ToSlash(filepath.Join("wiki", "syntheses", slug+".md"))
	abs := filepath.Join(opts.ProjectPath, filepath.FromSlash(rel))
	created := time.Now().Format("2006-01-02")
	if existing, err := os.ReadFile(abs); err == nil {
		created = knowledgeSynthesisCreatedDate(string(existing), created)
	}
	content := renderKnowledgeSynthesis(title, opts.Submission, created)
	if existing, err := os.ReadFile(abs); err == nil && string(existing) == content {
		paths := knowledgeWritebackWikiPaths(rel)
		return KnowledgeWritebackResult{Path: rel, Title: title, Written: false, WikiPaths: paths}, nil
	}
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return KnowledgeWritebackResult{}, err
	}
	defer release()
	if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(content), "knowledge-writeback: "+opts.Submission.Question); err != nil {
		return KnowledgeWritebackResult{}, err
	}
	if err := registerPageOwnership(opts.ProjectPath, rel, "knowledge"); err != nil {
		return KnowledgeWritebackResult{}, err
	}
	if err := appendIndex(opts.ProjectPath, "Syntheses", title, rel); err != nil {
		return KnowledgeWritebackResult{}, err
	}
	if err := appendOverview(opts.ProjectPath, "Recent Syntheses", title, rel, "synthesis"); err != nil {
		return KnowledgeWritebackResult{}, err
	}
	if err := appendKnowledgeLogOnce(opts.ProjectPath, title, rel, opts.Submission); err != nil {
		return KnowledgeWritebackResult{}, err
	}
	if err := RefreshRelationsArtifact(opts.ProjectPath); err != nil {
		return KnowledgeWritebackResult{}, fmt.Errorf("refresh generated relations: %w", err)
	}
	paths := knowledgeWritebackWikiPaths(rel)
	result := KnowledgeWritebackResult{Path: rel, Title: title, Written: true, WikiPaths: paths}
	if opts.WikiStore != nil {
		if _, err := SyncWikiPagePathsToStore(opts.Context, WikiSyncOptions{ProjectPath: opts.ProjectPath, ProjectID: opts.ProjectID, Store: opts.WikiStore, EmbeddingProvider: opts.EmbeddingProvider}, paths); err != nil {
			return KnowledgeWritebackResult{}, err
		}
		result.Synced = true
	}
	return result, nil
}

func submissionHasDurableCitation(citations []core.KnowledgeCitation) bool {
	for _, citation := range citations {
		if strings.TrimSpace(citation.Path) != "" && !IsAggregateKnowledgePath(citation.Path) {
			return true
		}
	}
	return false
}

func renderKnowledgeSynthesis(title string, submission core.KnowledgeSubmission, created string) string {
	sources := make([]string, 0, len(submission.Citations))
	seen := map[string]bool{}
	for _, citation := range submission.Citations {
		if citation.Path != "" && !seen[citation.Path] {
			seen[citation.Path] = true
			sources = append(sources, citation.Path)
		}
	}
	var citations strings.Builder
	for _, citation := range submission.Citations {
		fmt.Fprintf(&citations, "- `%s` (%s) %s\n", citation.Path, citation.Kind, citation.Title)
	}
	return wiki.RenderPage(wiki.Page{
		Title: title, Type: "synthesis", Sources: sources, GeneratedBy: "knowledge-core",
		Extra: map[string]string{"question": submission.Question, "created": created},
		Body:  fmt.Sprintf("# %s\n\n## Question\n\n%s\n\n## Answer\n\n%s\n\n## Citations\n\n%s", title, submission.Question, strings.TrimSpace(submission.Answer), citations.String()),
	})
}

func knowledgeSynthesisCreatedDate(content, fallback string) string {
	page := wiki.ParseWikiPage("knowledge-writeback", "wiki/synthesis.md", content)
	if value, ok := page.Frontmatter["created"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func appendKnowledgeLogOnce(projectPath, title, rel string, submission core.KnowledgeSubmission) error {
	sum := sha256.Sum256([]byte(rel + "\x00" + submission.Question + "\x00" + submission.Answer))
	marker := "knowledge-writeback:" + hex.EncodeToString(sum[:8])
	logPath := filepath.Join(projectPath, "wiki", "log.md")
	if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), marker) {
		return nil
	}
	return appendLog(projectPath, "knowledge", title, fmt.Sprintf("%s Explicit synthesis for `%s` written to `%s`.", marker, submission.Question, rel))
}

func knowledgeWritebackWikiPaths(path string) []string {
	return []string{path, "wiki/index.md", "wiki/overview.md", "wiki/log.md"}
}
