package compiler

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type failIfCalledProvider struct{ calls int }

func (p *failIfCalledProvider) Analyze(AnalysisInput) (string, error) {
	p.calls++
	return "", errors.New("provider must not be called")
}

func (p *failIfCalledProvider) Generate(string, AnalysisInput) (string, error) {
	p.calls++
	return "", errors.New("provider must not be called")
}

func TestExtractSourceTextReadsDOCXAsMarkdown(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Alpha heading</w:t></w:r></w:p>
    <w:p><w:r><w:t>Beta paragraph</w:t></w:r></w:p>
  </w:body>
</w:document>`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	extracted, err := extractSourceText("notes.docx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	text := string(extracted.Text)
	if extracted.RawName != "notes.md" {
		t.Fatalf("raw name=%s", extracted.RawName)
	}
	if !strings.Contains(text, "# notes") || !strings.Contains(text, "Alpha heading") || !strings.Contains(text, "Beta paragraph") {
		t.Fatalf("unexpected extracted markdown:\n%s", text)
	}
}

func TestValidateLLMWikiArchivesRichOriginalBesideExtractedMarkdown(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "rich-source"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Rich alpha evidence</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "manual.docx")
	if err := os.WriteFile(source, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.RawPath, "/extracted.md") {
		t.Fatalf("raw path=%s", result.RawPath)
	}
	manifest, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := manifest.Sources[sourceManifestKey(source)]
	if entry.ArchivePath == "" || !strings.HasSuffix(entry.OriginalRawPath, "/original/manual.docx") || entry.ContentPath != result.RawPath {
		t.Fatalf("manifest entry=%+v", entry)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.OriginalRawPath))); err != nil {
		t.Fatalf("original missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.ContentPath))); err != nil {
		t.Fatalf("extracted markdown missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(entry.ContentPath)), []byte("tampered derived text"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &failIfCalledProvider{}
	repaired, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Provider: provider, SkipUnchanged: true})
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.Skipped || provider.calls != 0 {
		t.Fatalf("derived repair must not spend LLM work: result=%+v calls=%d", repaired, provider.calls)
	}
	repairedContent, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.ContentPath)))
	if err != nil || !strings.Contains(string(repairedContent), "Rich alpha evidence") {
		t.Fatalf("derived content was not repaired: %s err=%v", repairedContent, err)
	}
}

func TestValidateLLMWikiWritesMultiPageWiki(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "validate"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "oauth-notes.md")
	if err := os.WriteFile(source, []byte(`# OAuth Notes

Token validation calls the auth service.
AuthService validates JWT claims and rejects expired sessions.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root,
		SourcePath:  source,
		Title:       "OAuth Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 3 {
		t.Fatalf("files=%v", result.Files)
	}
	for _, rel := range []string{
		"wiki/sources/oauth-notes.md",
		"wiki/concepts/token-validation.md",
		"wiki/entities/authservice.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
		content := string(data)
		if !strings.Contains(content, "sources:") {
			t.Fatalf("%s missing sources frontmatter", rel)
		}
	}

	concept, err := os.ReadFile(filepath.Join(root, "wiki/concepts/token-validation.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(concept), "[[authservice]]") {
		t.Fatal("concept page missing authservice wikilink")
	}
	reviews, err := os.ReadFile(filepath.Join(root, "wiki", "reviews.md"))
	if err != nil {
		t.Fatal(err)
	}
	reviewText := string(reviews)
	if !strings.Contains(reviewText, "review-needed | Follow up on OAuth Notes") || !strings.Contains(reviewText, "SEARCH: OAuth Notes background research") {
		t.Fatalf("reviews missing persisted review block:\n%s", reviewText)
	}
	indexData, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	indexText := string(indexData)
	if !entryAppearsAfterHeading(indexText, "## Concepts", "wiki/concepts/token-validation.md") {
		t.Fatalf("concept entry not under Concepts:\n%s", indexText)
	}
	if !entryAppearsAfterHeading(indexText, "## Entities", "wiki/entities/authservice.md") {
		t.Fatalf("entity entry not under Entities:\n%s", indexText)
	}

	queryResults, err := service.QueryWiki(root, "token validation auth service", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queryResults) < 3 {
		t.Fatalf("expected at least 3 query results, got %d", len(queryResults))
	}

	issues, err := service.LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.Type == "broken-link" {
			t.Fatalf("unexpected broken link: %+v", issue)
		}
	}
}

func entryAppearsAfterHeading(content, heading, entry string) bool {
	headingIndex := strings.Index(content, heading)
	entryIndex := strings.Index(content, entry)
	return headingIndex >= 0 && entryIndex > headingIndex
}

func TestValidateLLMWikiUsesFilenameSlugForChineseChapterTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "xiyouji"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "chapter-001.txt")
	if err := os.WriteFile(source, []byte("《西游记》\n《》目录 第一回　灵根育孕源流出　心性修持大道生\n花果山石猴出世。"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root,
		SourcePath:  source,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"wiki/sources/chapter-001.md":          false,
		"wiki/concepts/chapter-001-concept.md": false,
		"wiki/entities/chapter-001-entity.md":  false,
	}
	for _, file := range result.Files {
		if _, ok := want[file]; ok {
			want[file] = true
		}
	}
	for file, seen := range want {
		if !seen {
			t.Fatalf("missing %s from files %v", file, result.Files)
		}
	}
}

func TestValidateLLMWikiPathBuildsXiyoujiCorpusWiki(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "xiyouji"}); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join("..", "..", "tst", "xiyouji-chapters")
	if _, err := os.Stat(sourceDir); err != nil {
		t.Fatal(err)
	}

	result, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: root,
		SourcePath:  sourceDir,
		Provider:    MockProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 100 || result.FileCount != 300 || result.ReviewCount != 100 || result.SkippedCount != 0 {
		t.Fatalf("unexpected corpus result: %+v", result)
	}
	for _, rel := range []string{
		"wiki/sources/chapter-001.md",
		"wiki/concepts/chapter-054-concept.md",
		"wiki/entities/chapter-100-entity.md",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
	}
	issues, err := service.LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.Type == "broken-link" {
			t.Fatalf("unexpected broken link after corpus ingest: %+v", issue)
		}
	}
	queryResults, err := service.QueryWiki(root, "花果山", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(queryResults) == 0 {
		t.Fatal("expected query results for corpus term")
	}
	if isAggregateCorpusPath(queryResults[0].Path) {
		t.Fatalf("aggregate page should not dominate corpus query: %+v", queryResults[0])
	}
	if !strings.Contains(queryResults[0].Path, "chapter-") {
		t.Fatalf("expected chapter page as top corpus query result, got %+v", queryResults[0])
	}
}

func TestValidateLLMWikiPathSkipsUnchangedSources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "skip"}); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "oauth-notes.md")
	if err := os.WriteFile(source, []byte("# OAuth Notes\n\nToken validation calls the auth service."), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: root,
		SourcePath:  sourceDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceCount != 1 || first.FileCount == 0 || first.SkippedCount != 0 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	second, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath:   root,
		SourcePath:    sourceDir,
		SkipUnchanged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SourceCount != 1 || second.FileCount != 0 || second.ReviewCount != 0 || second.SkippedCount != 1 {
		t.Fatalf("unexpected skip result: %+v", second)
	}
	if len(second.Results) != 1 || !second.Results[0].Skipped {
		t.Fatalf("expected skipped result detail: %+v", second.Results)
	}
	manifestPath := filepath.Join(root, ".kbcore", "source-manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "oauth-notes.md") {
		t.Fatalf("manifest missing source entry:\n%s", manifest)
	}
}

func TestSkipUnchangedRecompilesStalePipelineEntryOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "pipeline-migration"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nPersistent wiki evidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source}); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	key := sourceManifestKey(source)
	entry := manifest.Sources[key]
	entry.PipelineVersion = core.SourceManifestPipelineVersion - 1
	manifest.Sources[key] = entry
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}

	migrated, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root, SourcePath: source, SkipUnchanged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Skipped {
		t.Fatal("stale pipeline entry was incorrectly skipped")
	}
	manifest, err = loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Sources[key].PipelineVersion; got != core.SourceManifestPipelineVersion {
		t.Fatalf("pipeline_version=%d", got)
	}

	skipped, err := ValidateLLMWiki(ValidateOptions{
		ProjectPath: root, SourcePath: source, SkipUnchanged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !skipped.Skipped {
		t.Fatal("current pipeline entry should be skipped")
	}
}

func isAggregateCorpusPath(path string) bool {
	return path == "wiki/index.md" || path == "wiki/log.md" || path == "wiki/overview.md"
}

func TestValidateLLMWikiArchivesOverwrittenPages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "versions"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "oauth-notes.md")
	if err := os.WriteFile(source, []byte("# OAuth Notes\n\nToken validation calls the auth service."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Title: "OAuth Notes"}); err != nil {
		t.Fatal(err)
	}
	conceptPath := filepath.Join(root, "wiki", "concepts", "token-validation.md")
	manual := "---\ntype: concept\ntitle: Token Validation\nsources:\n  - raw/sources/manual.md\n---\n\n# Token Validation\n\nManual edit.\n"
	if err := os.WriteFile(conceptPath, []byte(manual), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Title: "OAuth Notes"}); err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(root, ".kbcore", "page-versions", "wiki", "concepts", "token-validation")
	entries, err := os.ReadDir(versionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected archived page version")
	}
	archived, err := os.ReadFile(filepath.Join(versionDir, entries[len(entries)-1].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(archived), "Manual edit.") {
		t.Fatalf("archive did not preserve overwritten content:\n%s", archived)
	}
}

func TestMergePreservedPageSourcesUnionsExistingProvenance(t *testing.T) {
	root := t.TempDir()
	for _, source := range []string{"raw/sources/chapter-001.txt", "raw/sources/chapter-006.txt"} {
		abs := filepath.Join(root, filepath.FromSlash(source))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unownedRaw := filepath.Join(root, "raw", "sources", "chapter-999-invented.txt")
	if err := os.WriteFile(unownedRaw, []byte("physically present but not manifested"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"chapter-001": {RawPath: "raw/sources/chapter-001.txt"},
	}}
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "entities", "sun-wukong.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`---
type: entity
title: Sun Wukong
sources:
  - raw/sources/chapter-001.txt
aliases:
  - Monkey King
---

# Sun Wukong

Old evidence.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := ParsedBlocks{Files: []FileBlock{{Path: "wiki/entities/sun-wukong.md", Content: `---
type: entity
title: Sun Wukong
sources:
  - raw/sources/chapter-006.txt
  - raw/sources/chapter-999-invented.txt
aliases:
  - Great Sage
---

# Sun Wukong

Merged body.
`}}}
	if err := mergePreservedPageSources(root, "raw/sources/chapter-006.txt", &blocks); err != nil {
		t.Fatal(err)
	}
	frontmatter, err := parseGeneratedFrontmatter(blocks.Files[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	sources := frontmatterStrings(frontmatter["sources"])
	if len(sources) != 2 || sources[0] != "raw/sources/chapter-001.txt" || sources[1] != "raw/sources/chapter-006.txt" {
		t.Fatalf("sources=%v content=\n%s", sources, blocks.Files[0].Content)
	}
	if !strings.Contains(blocks.Files[0].Content, "Merged body.") {
		t.Fatalf("candidate body was not preserved:\n%s", blocks.Files[0].Content)
	}
}

func TestValidateGeneratedBlocksForTaskLimitsOnlyNewNonSummaryPages(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "wiki", "entities", "existing.md")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	page := func(path, kind string) FileBlock {
		return FileBlock{Path: path, Content: fmt.Sprintf("---\ntype: %s\ntitle: %s\nsources:\n  - raw/sources/a.txt\n---\n\n# %s\n", kind, filepath.Base(path), filepath.Base(path))}
	}
	blocks := ParsedBlocks{Files: []FileBlock{
		page("wiki/sources/a.md", "source-summary"),
		page("wiki/entities/existing.md", "entity"),
		page("wiki/entities/new-a.md", "entity"),
		page("wiki/concepts/new-b.md", "concept"),
	}}
	if err := validateGeneratedBlocksForTask(ValidateOptions{ProjectPath: root, MaxFilesPerTask: 12, MaxNewPagesPerSource: 1}, blocks, "raw/sources/a.txt"); err == nil || !strings.Contains(err.Error(), "maximum per source is 1") {
		t.Fatalf("error=%v", err)
	}
	if err := validateGeneratedBlocksForTask(ValidateOptions{ProjectPath: root, MaxFilesPerTask: 12, MaxNewPagesPerSource: 2, AllowedUpdatePaths: []string{"wiki/entities/existing.md"}}, blocks, "raw/sources/a.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGeneratedBlocksRejectsUnsuppliedExistingPage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "wiki", "entities", "existing.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: "wiki/sources/a.md", Content: "---\ntype: source-summary\ntitle: A\nsources: [raw/sources/a.txt]\n---\n\n# A\n"},
		{Path: "wiki/entities/existing.md", Content: "---\ntype: entity\ntitle: Existing\nsources: [raw/sources/a.txt]\n---\n\n# Existing\n"},
	}}
	err := validateGeneratedBlocksForTask(ValidateOptions{ProjectPath: root}, blocks, "raw/sources/a.txt")
	if err == nil || !strings.Contains(err.Error(), "was not supplied") {
		t.Fatalf("error=%v", err)
	}
}

func TestValidateGeneratedBlocksRejectsUnsuppliedForeignSourceSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "wiki", "sources", "foreign.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntype: source-summary\ntitle: Foreign\nsources: [raw/sources/foreign.txt]\n---\n\n# Foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := ParsedBlocks{Files: []FileBlock{{
		Path:    "wiki/sources/foreign.md",
		Content: "---\ntype: source-summary\ntitle: Current\nsources: [raw/sources/current.txt]\n---\n\n# Current\n",
	}}}
	err := validateGeneratedBlocksForTask(ValidateOptions{ProjectPath: root}, blocks, "raw/sources/current.txt")
	if err == nil || !strings.Contains(err.Error(), "was not supplied") {
		t.Fatalf("error=%v", err)
	}
}

func TestFilterUnplannedNewPagesKeepsSourceSummaryAndDowngradesLinks(t *testing.T) {
	root := t.TempDir()
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: "wiki/sources/a.md", Content: "---\ntype: source-summary\ntitle: A\nsources: [raw/sources/a.txt]\n---\n\n# A\n\nSee [[Unplanned]].\n"},
		{Path: "wiki/entities/unplanned.md", Content: "---\ntype: entity\ntitle: Unplanned\nsources: [raw/sources/a.txt]\n---\n\n# Unplanned\n"},
	}}
	opts := ValidateOptions{ProjectPath: root, AnalysisPlan: "## Wiki Plan\n- CREATE NEW | wiki/entities/approved.md | durability=central | evidence=x"}
	filterUnplannedNewPages(opts, &blocks)
	if len(blocks.Files) != 1 || blocks.Files[0].Path != "wiki/sources/a.md" {
		t.Fatalf("files=%+v", blocks.Files)
	}
	if strings.Contains(blocks.Files[0].Content, "[[Unplanned]]") || !strings.Contains(blocks.Files[0].Content, "See Unplanned") {
		t.Fatalf("summary=%s", blocks.Files[0].Content)
	}
	if len(blocks.Reviews) != 1 || blocks.Reviews[0].Type != "review-needed" || !strings.Contains(blocks.Reviews[0].Title, "unplanned.md") {
		t.Fatalf("reviews=%+v", blocks.Reviews)
	}
}

func TestNewlyAppearedUnsuppliedPathIsOptimisticConflict(t *testing.T) {
	root := t.TempDir()
	rel := "wiki/entities/shared.md"
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := ParsedBlocks{Files: []FileBlock{{Path: rel, Content: "candidate"}}}
	if got := newlyAppearedUnsuppliedPaths(ValidateOptions{ProjectPath: root}, map[string]string{}, blocks); len(got) != 1 || got[0] != rel {
		t.Fatalf("new conflict=%v", got)
	}
	if got := newlyAppearedUnsuppliedPaths(ValidateOptions{ProjectPath: root}, map[string]string{rel: "old"}, blocks); len(got) != 0 {
		t.Fatalf("preexisting path must not look newly appeared: %v", got)
	}
	if got := newlyAppearedUnsuppliedPaths(ValidateOptions{ProjectPath: root, AllowedUpdatePaths: []string{rel}}, map[string]string{}, blocks); len(got) != 0 {
		t.Fatalf("allowed update must not conflict: %v", got)
	}
}

func TestFilterUnsuppliedExistingPagesKeepsSummaryAndAuthorizedUpdates(t *testing.T) {
	root := t.TempDir()
	blocks := ParsedBlocks{Files: []FileBlock{
		{Path: "wiki/sources/a.md", Content: "---\ntype: source-summary\ntitle: A\nsources: [raw/sources/a.txt]\n---\n\n# A\n\nSee [[Unsafe]] and [[Safe]].\n"},
		{Path: "wiki/entities/unsafe.md", Content: "---\ntype: entity\ntitle: Unsafe\nsources: [raw/sources/a.txt]\n---\n\n# Unsafe\n"},
		{Path: "wiki/entities/safe.md", Content: "---\ntype: entity\ntitle: Safe\nsources: [raw/sources/a.txt]\n---\n\n# Safe\n"},
	}}
	for _, rel := range []string{"wiki/entities/unsafe.md", "wiki/entities/safe.md"} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("existing"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := ValidateOptions{ProjectPath: root, AllowedUpdatePaths: []string{"wiki/entities/safe.md"}}
	snapshot := map[string]string{"wiki/entities/unsafe.md": "old", "wiki/entities/safe.md": "old"}
	filterUnsuppliedExistingPages(opts, snapshot, &blocks)
	if len(blocks.Files) != 2 || blocks.Files[0].Path != "wiki/sources/a.md" || blocks.Files[1].Path != "wiki/entities/safe.md" {
		t.Fatalf("files=%+v", blocks.Files)
	}
	if !strings.Contains(blocks.Files[0].Content, "[[Unsafe]]") {
		t.Fatalf("valid link to the existing canonical page was changed: %s", blocks.Files[0].Content)
	}
	if !strings.Contains(blocks.Files[0].Content, "[[Safe]]") {
		t.Fatalf("authorized link was changed: %s", blocks.Files[0].Content)
	}
	if len(blocks.Reviews) != 1 || !strings.Contains(blocks.Reviews[0].Title, "unsafe.md") {
		t.Fatalf("reviews=%+v", blocks.Reviews)
	}
}

func TestSourcePathSHA256StreamsAndRejectsOversizedSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.md")
	content := []byte("streamed source")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sourcePathSHA256(path)
	if err != nil || got != sourceHash(content) {
		t.Fatalf("hash=%q err=%v", got, err)
	}
	large := filepath.Join(root, "large.md")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(core.MaxSourceBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := sourcePathSHA256(large); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error=%v", err)
	}
}

func TestValidateLLMWikiReimportsChangedLogicalSourceAsNewVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "versions"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "chapter.md")
	if err := os.WriteFile(source, []byte("# Versioned Chapter\n\nFirst immutable version."), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Title: "Versioned Chapter"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("# Versioned Chapter\n\nSecond corrected version with new evidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Title: "Versioned Chapter"})
	if err != nil {
		t.Fatal(err)
	}
	if first.RawPath == second.RawPath || first.SHA256 == second.SHA256 {
		t.Fatalf("source version did not change: first=%+v second=%+v", first, second)
	}
	value, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := value.Sources[sourceManifestKey(source)]
	if len(entry.Versions) != 1 || entry.Versions[0].RawPath != first.RawPath || entry.RawPath != second.RawPath {
		t.Fatalf("version lineage=%+v current=%+v", entry.Versions, entry)
	}
	firstBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(first.RawPath)))
	if err != nil || !strings.Contains(string(firstBytes), "First immutable version") {
		t.Fatalf("prior immutable source changed: %q err=%v", firstBytes, err)
	}
	secondBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(second.RawPath)))
	if err != nil || !strings.Contains(string(secondBytes), "Second corrected version") {
		t.Fatalf("current immutable source=%q err=%v", secondBytes, err)
	}
	summary, err := os.ReadFile(filepath.Join(root, "wiki", "sources", "versioned-chapter.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := wiki.ParseWikiPage("test", "wiki/sources/versioned-chapter.md", string(summary))
	if len(page.Sources) != 1 || page.Sources[0] != second.RawPath {
		t.Fatalf("summary sources=%v want current=%s", page.Sources, second.RawPath)
	}
	concept, err := os.ReadFile(filepath.Join(root, "wiki", "concepts", "versioned-chapter-concept.md"))
	if err != nil {
		t.Fatal(err)
	}
	conceptPage := wiki.ParseWikiPage("test", "wiki/concepts/versioned-chapter-concept.md", string(concept))
	if !containsExactString(conceptPage.Sources, first.RawPath) || !containsExactString(conceptPage.Sources, second.RawPath) {
		t.Fatalf("shared page lost version provenance: %v", conceptPage.Sources)
	}
}

func TestSourceTransactionRollsBackPartialMarkdownAndManifestOnFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "rollback"}); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "oauth.md")
	if err := os.WriteFile(source, []byte("# OAuth\n\nToken validation calls the auth service."), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceCommitTestHook = func(stage, path string) error {
		if stage == "page" && strings.Contains(path, "authservice") {
			return fmt.Errorf("injected commit failure")
		}
		return nil
	}
	defer func() { sourceCommitTestHook = nil }()
	if _, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Title: "OAuth"}); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("error=%v", err)
	}
	for _, rel := range []string{"wiki/sources/oauth.md", "wiki/concepts/token-validation.md", "wiki/entities/authservice.md"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("partial page remains %s: %v", rel, err)
		}
	}
	indexAfter, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil || string(indexAfter) != string(indexBefore) {
		t.Fatalf("index changed after rollback: err=%v\n%s", err, indexAfter)
	}
	manifest, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources) != 0 {
		t.Fatalf("manifest=%+v", manifest)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".kbcore", "transactions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unfinished transactions=%v", entries)
	}
}

func containsExactString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExistingPagesForAnalysisExcludesCompilerManagedAndSourceSummaryPages(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"wiki/index.md":             "# Index\n",
		"wiki/overview.md":          "# Overview\n",
		"wiki/log.md":               "# Log\n",
		"wiki/reviews.md":           "# Reviews\n",
		"wiki/sources/chapter-1.md": "---\ntype: source-summary\ntitle: Chapter One\nsources: [raw/sources/chapter-1.txt]\n---\n# Chapter One\n",
		"wiki/entities/hero.md":     "---\ntype: entity\ntitle: Hero\nsources: [raw/sources/chapter-1.txt]\n---\n# Hero\n",
	}
	for path, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	existing, dependencies := existingPagesForAnalysis(root, "update wiki/index.md wiki/overview.md wiki/log.md wiki/reviews.md wiki/sources/chapter-1.md and wiki/entities/hero.md")
	if len(dependencies) != 1 || dependencies[0] != "wiki/entities/hero.md" {
		t.Fatalf("dependencies = %#v, want only mergeable entity page", dependencies)
	}
	if strings.Contains(existing, "Chapter One") || !strings.Contains(existing, "# Hero") {
		t.Fatalf("unexpected existing page context:\n%s", existing)
	}
}

func TestSanitizeGeneratedAliasCollisionsRemovesAliasButNotPage(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "wiki", "entities", "sun-wukong.md")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("---\ntype: entity\ntitle: 孙悟空\naliases: [美猴王]\nsources: [raw/sources/chapter-1.txt]\n---\n# 孙悟空\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := ParsedBlocks{Files: []FileBlock{{
		Path:    "wiki/concepts/great-sage.md",
		Content: "---\ntype: concept\ntitle: 齐天大圣\naliases: [孙悟空, 大圣称号]\nsources: [raw/sources/chapter-4.txt]\n---\n# 齐天大圣\n",
	}}}
	if conflicts := canonicalIdentityConflictPaths(root, blocks); len(conflicts) == 0 {
		t.Fatal("expected alias collision before sanitizing")
	}

	sanitizeGeneratedAliasCollisions(root, &blocks)
	if conflicts := canonicalIdentityConflictPaths(root, blocks); len(conflicts) != 0 {
		t.Fatalf("conflicts after sanitizing: %v", conflicts)
	}
	frontmatter, err := parseGeneratedFrontmatter(blocks.Files[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	aliases := frontmatterStrings(frontmatter["aliases"])
	if len(aliases) != 1 || aliases[0] != "大圣称号" {
		t.Fatalf("aliases=%v", aliases)
	}
	if len(blocks.Reviews) != 1 || blocks.Reviews[0].Type != "duplicate" || !strings.Contains(blocks.Reviews[0].Body, "孙悟空") {
		t.Fatalf("reviews=%+v", blocks.Reviews)
	}
}

func TestAppendReviewNormalizesAndMergesOpenReviewEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "reviews"}); err != nil {
		t.Fatal(err)
	}
	review := ReviewBlock{Type: "Suggestion", Title: "Merge duplicate identity", Body: "First evidence."}
	if err := appendReview(root, "One", "raw/sources/one.md", review, []string{"wiki/entities/one.md"}); err != nil {
		t.Fatal(err)
	}
	if err := appendReview(root, "Two", "raw/sources/two.md", review, []string{"wiki/entities/two.md"}); err != nil {
		t.Fatal(err)
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: root, ProjectID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Type != "review-needed" || len(items[0].SourcePaths) != 2 || len(items[0].AffectedPages) != 2 {
		t.Fatalf("items=%+v", items)
	}
}

func TestResolveSatisfiedPageReviewsMatchesRequestedTitleNotEvidenceMentions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "reviews"}); err != nil {
		t.Fatal(err)
	}
	if err := appendReview(root, "Garden", "raw/sources/garden.md", ReviewBlock{
		Type: "missing-page", Title: "Compost Blend A", Body: "Used by the North Garden Rose Bed.",
	}, []string{"wiki/entities/north-garden-rose-bed.md"}); err != nil {
		t.Fatal(err)
	}
	if err := appendReview(root, "Garden", "raw/sources/garden.md", ReviewBlock{
		Type: "review-needed", Title: "North Garden Rose Bed durability", Body: "Create the entity page when accepted.",
	}, nil); err != nil {
		t.Fatal(err)
	}
	files := []FileBlock{{Path: "wiki/entities/north-garden-rose-bed.md", Content: `---
type: entity
title: North Garden Rose Bed
sources:
  - raw/sources/garden.md
---

# North Garden Rose Bed
`}}
	if err := resolveSatisfiedPageReviews(root, files); err != nil {
		t.Fatal(err)
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: root, ProjectID: "compiler"})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, item := range items {
		statuses[item.Title] = item.Status
	}
	if statuses["Compost Blend A"] != "open" || statuses["North Garden Rose Bed durability"] != "resolved" {
		t.Fatalf("statuses=%v", statuses)
	}
}

func TestCanonicalIdentityConflictsRequireExistingPageReuse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "wiki", "entities", "sanzang.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntype: entity\ntitle: Sanzang\naliases:\n  - Xuanzang\nsources: []\n---\n\n# Sanzang\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	duplicate := ParsedBlocks{Files: []FileBlock{{Path: "wiki/entities/xuanzang.md", Content: "---\ntype: entity\ntitle: Xuanzang\naliases:\n  - Sanzang\nsources: []\n---\n\n# Xuanzang\n"}}}
	conflicts := canonicalIdentityConflictPaths(root, duplicate)
	if len(conflicts) != 1 || conflicts[0] != "wiki/entities/sanzang.md" {
		t.Fatalf("conflicts=%v", conflicts)
	}
	duplicate.Files[0].Path = "wiki/entities/sanzang.md"
	if conflicts := canonicalIdentityConflictPaths(root, duplicate); len(conflicts) != 0 {
		t.Fatalf("canonical update conflicts=%v", conflicts)
	}
}

type unresolvedLinkProvider struct{ calls int }

func (unresolvedLinkProvider) Analyze(input AnalysisInput) (string, error) {
	return "## Wiki Plan\n- wiki/sources/broken-links.md", nil
}

func (p *unresolvedLinkProvider) Generate(_ string, input AnalysisInput) (string, error) {
	p.calls++
	return fmt.Sprintf(`---FILE: wiki/sources/broken-links.md
---
type: source-summary
title: Broken Links
sources:
  - %q
---

# Broken Links

The model keeps emitting [[missing-target|a useful phrase]].
`, input.SourceRel), nil
}

func TestValidateLLMWikiDowngradesUnresolvedLinksBeforeLLMRepair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "broken-links"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Broken Links\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &unresolvedLinkProvider{}
	result, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	if provider.calls != 1 {
		t.Fatalf("unresolved links should be downgraded without an LLM repair: calls=%d", provider.calls)
	}
	page, err := os.ReadFile(filepath.Join(root, "wiki", "sources", "broken-links.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "[[missing-target") || !strings.Contains(string(page), "a useful phrase") {
		t.Fatalf("page=%s", page)
	}
	reviews, err := os.ReadFile(filepath.Join(root, "wiki", "reviews.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviews), "missing-page") || !strings.Contains(string(reviews), "missing-target") {
		t.Fatalf("reviews=%s", reviews)
	}
}

type missingSummaryProvider struct{ generateCalls int }

func (p *missingSummaryProvider) Analyze(input AnalysisInput) (string, error) {
	return "## Wiki Plan\n- wiki/entities/only-entity.md", nil
}

func (p *missingSummaryProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	p.generateCalls++
	if strings.Contains(analysis, "SOURCE SUMMARY RECOVERY REQUIRED") {
		return fmt.Sprintf(`---FILE: wiki/sources/recovered.md
---
type: source-summary
title: Recovered Source
sources:
  - %q
---

# Recovered Source

LLM-written recovery summary.
`, input.SourceRel), nil
	}
	return fmt.Sprintf(`---FILE: wiki/entities/only-entity.md
---
type: entity
title: Only Entity
sources:
  - %q
---

# Only Entity

Entity evidence.
`, input.SourceRel), nil
}

func TestValidateLLMWikiRecoversOnlyMissingSourceSummary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "summary-recovery"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &missingSummaryProvider{}
	result, err := ValidateLLMWiki(ValidateOptions{ProjectPath: root, SourcePath: source, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if provider.generateCalls != 3 || len(result.Files) != 2 {
		t.Fatalf("calls=%d result=%+v", provider.generateCalls, result)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "sources", "recovered.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "entities", "only-entity.md")); err != nil {
		t.Fatal(err)
	}
}
