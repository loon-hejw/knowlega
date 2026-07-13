package compiler

import (
	"archive/zip"
	"bytes"
	"errors"
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
	overview, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overview), "Recent LLM Wiki Updates") || !strings.Contains(string(overview), "wiki/concepts/token-validation.md") {
		t.Fatalf("overview missing validation updates:\n%s", overview)
	}
	reviews, err := os.ReadFile(filepath.Join(root, "wiki", "reviews.md"))
	if err != nil {
		t.Fatal(err)
	}
	reviewText := string(reviews)
	if !strings.Contains(reviewText, "suggestion | Follow up on OAuth Notes") || !strings.Contains(reviewText, "SEARCH: OAuth Notes background research") {
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
	if err := os.WriteFile(conceptPath, []byte("# manual edit\n"), 0o644); err != nil {
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
	if !strings.Contains(string(archived), "# manual edit") {
		t.Fatalf("archive did not preserve overwritten content:\n%s", archived)
	}
}
