package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
	"github.com/hejw/knowledge-core/internal/sourcearchive"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type IngestOptions struct {
	ProjectPath string
	SourcePath  string
	Title       string
	Kind        string
}

type IngestResult struct {
	RawPath  string
	WikiPath string
	SHA256   string
}

func IngestSource(opts IngestOptions) (IngestResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return IngestResult{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.SourcePath) == "" {
		return IngestResult{}, fmt.Errorf("source path is required")
	}
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return IngestResult{}, err
	}
	defer release()
	sourcePath := opts.SourcePath
	archive, archived, err := sourcearchive.FindBySourcePath(opts.ProjectPath, sourcePath)
	if err != nil {
		return IngestResult{}, err
	}
	if archived {
		sourcePath = filepath.Join(opts.ProjectPath, filepath.FromSlash(archive.Metadata.ContentPath))
	}
	data, err := readBoundedSourceFile(sourcePath)
	if err != nil {
		return IngestResult{}, err
	}
	originalData := data
	if archived && archive.Metadata.ContentPath != archive.Metadata.OriginalRawPath {
		originalData, err = readBoundedSourceFile(filepath.Join(opts.ProjectPath, filepath.FromSlash(archive.Metadata.OriginalRawPath)))
		if err != nil {
			return IngestResult{}, err
		}
	}
	sum := sha256.Sum256(originalData)
	hash := hex.EncodeToString(sum[:])
	name := filepath.Base(opts.SourcePath)
	if archived {
		name = archive.Metadata.OriginalName
	}
	slug := core.Slug(name)
	if opts.Title == "" {
		opts.Title = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if opts.Kind == "" {
		opts.Kind = "source"
	}

	rawRel := ""
	if archived {
		rawRel = archive.Metadata.ContentPath
	} else if sourceInsideRawSources(opts.ProjectPath, opts.SourcePath) {
		projectAbs, _ := filepath.Abs(opts.ProjectPath)
		sourceAbs, _ := filepath.Abs(opts.SourcePath)
		rel, relErr := filepath.Rel(projectAbs, sourceAbs)
		if relErr != nil {
			return IngestResult{}, relErr
		}
		rawRel = filepath.ToSlash(rel)
	} else {
		archive, err = sourcearchive.ImportBytes(sourcearchive.ImportOptions{
			ProjectPath:      opts.ProjectPath,
			RelativePath:     name,
			OriginalLocation: opts.SourcePath,
		}, originalData)
		if err != nil {
			return IngestResult{}, err
		}
		archived = true
		rawRel = archive.Metadata.ContentPath
	}

	body := renderSourceSummaryBody(opts.Title, rawRel, hash, string(data))
	page := wiki.RenderPage(wiki.Page{
		Title:       opts.Title,
		Type:        "source-summary",
		Sources:     []string{rawRel},
		GeneratedBy: "knowledge-core",
		Extra: map[string]string{
			"source_kind": opts.Kind,
			"sha256":      hash,
		},
		Body: body,
	})

	wikiRel := filepath.Join("wiki", "sources", slug+".md")
	wikiAbs := filepath.Join(opts.ProjectPath, wikiRel)
	if err := os.MkdirAll(filepath.Dir(wikiAbs), 0o755); err != nil {
		return IngestResult{}, err
	}
	if err := wiki.WriteVersionedPage(opts.ProjectPath, filepath.ToSlash(wikiRel), []byte(page), "ingest: "+opts.Title); err != nil {
		return IngestResult{}, err
	}
	_ = appendLog(opts.ProjectPath, "ingest", opts.Title, fmt.Sprintf("Source `%s` compiled to `%s`.", rawRel, wikiRel))
	_ = appendIndex(opts.ProjectPath, "Sources", opts.Title, wikiRel)
	_ = appendOverview(opts.ProjectPath, "Recent Source Updates", opts.Title, wikiRel, "source-summary")
	if err := upsertIngestSourceManifest(opts.ProjectPath, opts.SourcePath, rawRel, opts.Title, hash, []string{filepath.ToSlash(wikiRel)}, archiveMetadata(archive, archived)); err != nil {
		return IngestResult{}, err
	}
	if err := RefreshRelationsArtifact(opts.ProjectPath); err != nil {
		return IngestResult{}, fmt.Errorf("refresh generated relations: %w", err)
	}

	return IngestResult{RawPath: rawRel, WikiPath: wikiRel, SHA256: hash}, nil
}

func upsertIngestSourceManifest(projectPath, sourcePath, rawRel, title, hash string, files []string, archive *sourcearchive.Metadata) error {
	value, err := manifestfile.Load(projectPath)
	if err != nil {
		return err
	}
	key := manifestfile.Key(sourcePath)
	previous, existed := value.Sources[key]
	entry := manifestfile.Entry{
		OriginalPath: key,
		SHA256:       hash,
		RawPath:      filepath.ToSlash(rawRel),
		Title:        title,
		Files:        files,
		ReviewCount:  0,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if archive != nil {
		entry.OriginalPath = key
		entry.ArchivePath = archive.ArchivePath
		entry.OriginalRawPath = archive.OriginalRawPath
		entry.ContentPath = archive.ContentPath
		entry.OriginalSHA256 = archive.OriginalSHA256
		entry.ContentSHA256 = archive.ContentSHA256
	}
	if existed && previous.SHA256 == hash {
		entry.PipelineVersion = previous.PipelineVersion
		entry.GenerationContractSHA256 = previous.GenerationContractSHA256
		entry.NewPageBudget = previous.NewPageBudget
		entry.NewPageCount = previous.NewPageCount
		entry.CreatedPages = append([]string(nil), previous.CreatedPages...)
		entry.ReviewCount = previous.ReviewCount
		entry.Extraction = previous.Extraction
		entry.Versions = append([]manifestfile.SourceVersion(nil), previous.Versions...)
		entry.Files = mergePathLists(previous.Files, files)
	} else if existed {
		manifestfile.AppendPriorVersion(previous, &entry)
	}
	value.Sources[key] = entry
	for _, path := range files {
		manifestfile.RegisterPageOwner(&value, path, "source", key)
	}
	return manifestfile.Save(projectPath, value)
}

func archiveMetadata(archive sourcearchive.Archive, ok bool) *sourcearchive.Metadata {
	if !ok {
		return nil
	}
	metadata := archive.Metadata
	return &metadata
}

func sourceInsideRawSources(projectPath, sourcePath string) bool {
	rawRoot, err := filepath.Abs(filepath.Join(projectPath, "raw", "sources"))
	if err != nil {
		return false
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rawRoot, sourceAbs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sourceManifestKey(path string) string {
	return manifestfile.Key(path)
}

func mergePathLists(values ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range values {
		for _, value := range list {
			value = filepath.ToSlash(strings.TrimSpace(value))
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func renderSourceSummaryBody(title, rawRel, hash, content string) string {
	const maxExcerpt = 2400
	excerpt := strings.TrimSpace(content)
	if len(excerpt) > maxExcerpt {
		excerpt = excerpt[:maxExcerpt] + "\n\n..."
	}
	return fmt.Sprintf(
		"# %s\n\n"+
			"## Source\n\n"+
			"- Path: `%s`\n"+
			"- SHA256: `%s`\n\n"+
			"## Initial Summary\n\n"+
			"This page is an initial deterministic source summary. A future LLM compiler pass should extract entities, concepts, contradictions, and cross-links from this source.\n\n"+
			"## Excerpt\n\n"+
			"%s\n",
		title, rawRel, hash, fenced(excerpt),
	)
}

func fenced(text string) string {
	if strings.Contains(text, "```") {
		return "<pre>\n" + text + "\n</pre>"
	}
	return "```text\n" + text + "\n```"
}

func appendLog(projectPath, op, title, body string) error {
	path := filepath.Join(projectPath, "wiki", "log.md")
	line := fmt.Sprintf("\n## [%s] %s | %s\n\n%s\n", time.Now().Format("2006-01-02"), op, title, body)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, line)
	return err
}

func appendIndex(projectPath, section, title, rel string) error {
	return wiki.AppendIndexEntry(projectPath, section, title, rel)
}

func appendOverview(projectPath, section, title, rel, kind string) error {
	path := filepath.Join(projectPath, "wiki", "overview.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("# Overview\n\n"), 0o644); err != nil {
			return err
		}
	}
	entry := fmt.Sprintf("- %s: [[%s|%s]] (`%s`)\n", kind, strings.TrimSuffix(filepath.Base(rel), ".md"), title, filepath.ToSlash(rel))
	current, _ := os.ReadFile(path)
	if strings.Contains(string(current), entry) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if !strings.Contains(string(current), "## "+section) {
		if _, err := io.WriteString(f, "\n## "+section+"\n\n"); err != nil {
			return err
		}
	}
	_, err = io.WriteString(f, entry)
	return err
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
