package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
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
	data, err := os.ReadFile(opts.SourcePath)
	if err != nil {
		return IngestResult{}, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	name := filepath.Base(opts.SourcePath)
	slug := core.Slug(name)
	if opts.Title == "" {
		opts.Title = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if opts.Kind == "" {
		opts.Kind = "source"
	}

	rawDir := filepath.Join(opts.ProjectPath, "raw", "sources")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return IngestResult{}, err
	}
	rawRel := filepath.Join("raw", "sources", hash[:12]+"-"+name)
	rawAbs := filepath.Join(opts.ProjectPath, rawRel)
	if _, err := os.Stat(rawAbs); os.IsNotExist(err) {
		if err := writeFileAtomic(rawAbs, data); err != nil {
			return IngestResult{}, err
		}
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
	if err := upsertIngestSourceManifest(opts.ProjectPath, opts.SourcePath, rawRel, opts.Title, hash, []string{filepath.ToSlash(wikiRel)}); err != nil {
		return IngestResult{}, err
	}

	return IngestResult{RawPath: rawRel, WikiPath: wikiRel, SHA256: hash}, nil
}

type ingestSourceManifest struct {
	Version int                                  `json:"version"`
	Sources map[string]ingestSourceManifestEntry `json:"sources"`
}

type ingestSourceManifestEntry struct {
	OriginalPath string   `json:"original_path"`
	SHA256       string   `json:"sha256"`
	RawPath      string   `json:"raw_path"`
	Title        string   `json:"title"`
	Files        []string `json:"files"`
	ReviewCount  int      `json:"review_count"`
	UpdatedAt    string   `json:"updated_at"`
}

func upsertIngestSourceManifest(projectPath, sourcePath, rawRel, title, hash string, files []string) error {
	manifest, err := loadIngestSourceManifest(projectPath)
	if err != nil {
		return err
	}
	key := sourceManifestKey(sourcePath)
	manifest.Sources[key] = ingestSourceManifestEntry{
		OriginalPath: key,
		SHA256:       hash,
		RawPath:      filepath.ToSlash(rawRel),
		Title:        title,
		Files:        files,
		ReviewCount:  0,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	return saveIngestSourceManifest(projectPath, manifest)
}

func loadIngestSourceManifest(projectPath string) (ingestSourceManifest, error) {
	manifest := ingestSourceManifest{
		Version: 1,
		Sources: map[string]ingestSourceManifestEntry{},
	}
	data, err := os.ReadFile(ingestSourceManifestPath(projectPath))
	if os.IsNotExist(err) {
		return manifest, nil
	}
	if err != nil {
		return ingestSourceManifest{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return manifest, nil
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ingestSourceManifest{}, fmt.Errorf("read source manifest: %w", err)
	}
	if manifest.Version == 0 {
		manifest.Version = 1
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]ingestSourceManifestEntry{}
	}
	return manifest, nil
}

func saveIngestSourceManifest(projectPath string, manifest ingestSourceManifest) error {
	path := ingestSourceManifestPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

func ingestSourceManifestPath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "source-manifest.json")
}

func sourceManifestKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(abs)
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
