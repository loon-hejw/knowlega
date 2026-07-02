package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type ValidateOptions struct {
	ProjectPath   string
	SourcePath    string
	Title         string
	Provider      Provider
	SkipUnchanged bool
}

type ValidateResult struct {
	RawPath     string
	Analysis    string
	Files       []string
	ReviewCount int
	Skipped     bool
	SHA256      string
}

type BatchValidateResult struct {
	Results      []ValidateResult
	SourceCount  int
	FileCount    int
	ReviewCount  int
	SkippedCount int
}

func ValidateLLMWiki(opts ValidateOptions) (ValidateResult, error) {
	if opts.Provider == nil {
		opts.Provider = MockProvider{}
	}
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return ValidateResult{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.SourcePath) == "" {
		return ValidateResult{}, fmt.Errorf("source path is required")
	}
	sourceBytes, err := os.ReadFile(opts.SourcePath)
	if err != nil {
		return ValidateResult{}, err
	}
	hash := sourceHash(sourceBytes)
	sourceTitle := opts.Title
	if sourceTitle == "" {
		sourceTitle = inferSourceTitle(opts.SourcePath, string(sourceBytes))
	}
	manifest, err := loadSourceManifest(opts.ProjectPath)
	if err != nil {
		return ValidateResult{}, err
	}
	manifestKey := sourceManifestKey(opts.SourcePath)
	if opts.SkipUnchanged {
		if entry, ok := manifest.Sources[manifestKey]; ok && entry.SHA256 == hash {
			return ValidateResult{
				RawPath:     entry.RawPath,
				Files:       entry.Files,
				ReviewCount: entry.ReviewCount,
				Skipped:     true,
				SHA256:      hash,
			}, nil
		}
	}
	sourceRel, err := copyRawSource(opts.ProjectPath, opts.SourcePath, sourceBytes)
	if err != nil {
		return ValidateResult{}, err
	}
	input := AnalysisInput{
		SourceTitle: sourceTitle,
		SourceRel:   sourceRel,
		SourceText:  string(sourceBytes),
		Purpose:     readOptional(filepath.Join(opts.ProjectPath, "purpose.md")),
		Schema:      readOptional(filepath.Join(opts.ProjectPath, "schema.md")),
		Index:       readOptional(filepath.Join(opts.ProjectPath, "wiki", "index.md")),
		Overview:    readOptional(filepath.Join(opts.ProjectPath, "wiki", "overview.md")),
	}
	analysis, err := opts.Provider.Analyze(input)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("analysis: %w", err)
	}
	generation, err := opts.Provider.Generate(analysis, input)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("generation: %w", err)
	}
	blocks, err := ParseBlocks(generation)
	if err != nil {
		return ValidateResult{}, err
	}
	var written []string
	for _, file := range blocks.Files {
		if err := wiki.WriteVersionedPage(opts.ProjectPath, file.Path, []byte(file.Content), "validate-llmwiki: "+sourceTitle); err != nil {
			return ValidateResult{}, err
		}
		written = append(written, file.Path)
	}
	if err := updateAggregates(opts.ProjectPath, sourceTitle, sourceRel, written, blocks.Reviews); err != nil {
		return ValidateResult{}, err
	}
	manifest.Sources[manifestKey] = SourceManifestEntry{
		OriginalPath: manifestKey,
		SHA256:       hash,
		RawPath:      sourceRel,
		Title:        sourceTitle,
		Files:        written,
		ReviewCount:  len(blocks.Reviews),
		UpdatedAt:    time.Now().Format(time.RFC3339),
	}
	if err := saveSourceManifest(opts.ProjectPath, manifest); err != nil {
		return ValidateResult{}, err
	}
	return ValidateResult{
		RawPath:     sourceRel,
		Analysis:    analysis,
		Files:       written,
		ReviewCount: len(blocks.Reviews),
		SHA256:      hash,
	}, nil
}

func ValidateLLMWikiPath(opts ValidateOptions) (BatchValidateResult, error) {
	info, err := os.Stat(opts.SourcePath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	if !info.IsDir() {
		result, err := ValidateLLMWiki(opts)
		if err != nil {
			return BatchValidateResult{}, err
		}
		return BatchValidateResult{
			Results:      []ValidateResult{result},
			SourceCount:  1,
			FileCount:    filesWritten(result),
			ReviewCount:  reviewsWritten(result),
			SkippedCount: skippedCount(result),
		}, nil
	}
	var sources []string
	err = filepath.WalkDir(opts.SourcePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".txt" || ext == ".md" {
			sources = append(sources, path)
		}
		return nil
	})
	if err != nil {
		return BatchValidateResult{}, err
	}
	sort.Strings(sources)
	var batch BatchValidateResult
	for _, source := range sources {
		result, err := ValidateLLMWiki(ValidateOptions{
			ProjectPath:   opts.ProjectPath,
			SourcePath:    source,
			Provider:      opts.Provider,
			SkipUnchanged: opts.SkipUnchanged,
		})
		if err != nil {
			return BatchValidateResult{}, fmt.Errorf("validate %s: %w", source, err)
		}
		batch.Results = append(batch.Results, result)
		batch.SourceCount++
		batch.FileCount += filesWritten(result)
		batch.ReviewCount += reviewsWritten(result)
		batch.SkippedCount += skippedCount(result)
	}
	return batch, nil
}

func filesWritten(result ValidateResult) int {
	if result.Skipped {
		return 0
	}
	return len(result.Files)
}

func reviewsWritten(result ValidateResult) int {
	if result.Skipped {
		return 0
	}
	return result.ReviewCount
}

func skippedCount(result ValidateResult) int {
	if result.Skipped {
		return 1
	}
	return 0
}

func inferSourceTitle(path, content string) string {
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
		break
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func copyRawSource(projectPath, sourcePath string, data []byte) (string, error) {
	projectRawRoot, err := filepath.Abs(filepath.Join(projectPath, "raw", "sources"))
	if err != nil {
		return "", err
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(projectRawRoot, sourceAbs); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		projectAbs, err := filepath.Abs(projectPath)
		if err != nil {
			return "", err
		}
		projectRel, err := filepath.Rel(projectAbs, sourceAbs)
		if err != nil {
			return "", err
		}
		return filepath.ToSlash(projectRel), nil
	}
	hash := sourceHash(data)
	name := filepath.Base(sourcePath)
	rel := filepath.ToSlash(filepath.Join("raw", "sources", hash[:12]+"-"+name))
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return "", err
		}
	}
	return rel, nil
}

func sourceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type SourceManifest struct {
	Version int                            `json:"version"`
	Sources map[string]SourceManifestEntry `json:"sources"`
}

type SourceManifestEntry struct {
	OriginalPath string   `json:"original_path"`
	SHA256       string   `json:"sha256"`
	RawPath      string   `json:"raw_path"`
	Title        string   `json:"title"`
	Files        []string `json:"files"`
	ReviewCount  int      `json:"review_count"`
	UpdatedAt    string   `json:"updated_at"`
}

func loadSourceManifest(projectPath string) (SourceManifest, error) {
	manifest := SourceManifest{
		Version: 1,
		Sources: map[string]SourceManifestEntry{},
	}
	data, err := os.ReadFile(sourceManifestPath(projectPath))
	if os.IsNotExist(err) {
		return manifest, nil
	}
	if err != nil {
		return SourceManifest{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return manifest, nil
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return SourceManifest{}, fmt.Errorf("read source manifest: %w", err)
	}
	if manifest.Version == 0 {
		manifest.Version = 1
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]SourceManifestEntry{}
	}
	return manifest, nil
}

func LoadSourceManifestEntries(projectPath, projectID string) ([]core.SourceManifestEntry, error) {
	manifest, err := loadSourceManifest(projectPath)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]core.SourceManifestEntry, 0, len(keys))
	for _, key := range keys {
		entry := manifest.Sources[key]
		originalPath := entry.OriginalPath
		if strings.TrimSpace(originalPath) == "" {
			originalPath = key
		}
		updatedAt := time.Time{}
		if strings.TrimSpace(entry.UpdatedAt) != "" {
			parsed, parseErr := time.Parse(time.RFC3339, entry.UpdatedAt)
			if parseErr != nil {
				return nil, parseErr
			}
			updatedAt = parsed
		}
		entries = append(entries, core.SourceManifestEntry{
			ID:           core.StableID(projectID, "source-manifest", originalPath),
			ProjectID:    projectID,
			OriginalPath: originalPath,
			SHA256:       entry.SHA256,
			RawPath:      entry.RawPath,
			Title:        entry.Title,
			Files:        append([]string(nil), entry.Files...),
			ReviewCount:  entry.ReviewCount,
			UpdatedAt:    updatedAt,
		})
	}
	return entries, nil
}

func saveSourceManifest(projectPath string, manifest SourceManifest) error {
	path := sourceManifestPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sourceManifestPath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "source-manifest.json")
}

func sourceManifestKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(abs)
}

func readOptional(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func updateAggregates(projectPath, title string, sourceRel string, files []string, reviews []ReviewBlock) error {
	for _, file := range files {
		if file == "wiki/index.md" || file == "wiki/log.md" || file == "wiki/overview.md" {
			continue
		}
		entryTitle := titleForEntry(filepath.Join(projectPath, filepath.FromSlash(file)), title)
		if err := wiki.AppendIndexEntry(projectPath, indexSectionForWikiFile(file), entryTitle, file); err != nil {
			return err
		}
		if err := appendOverviewEntry(projectPath, "Recent LLM Wiki Updates", entryTitle, file, pageTypeForOverview(filepath.Join(projectPath, filepath.FromSlash(file)))); err != nil {
			return err
		}
	}
	for _, review := range reviews {
		if err := appendReview(projectPath, title, sourceRel, review, files); err != nil {
			return err
		}
	}
	logEntry := fmt.Sprintf("\n## [%s] ingest | %s\n\nLLM Wiki validation flow wrote %d file(s).\n", today(), title, len(files))
	return appendUnique(filepath.Join(projectPath, "wiki", "log.md"), logEntry)
}

func indexSectionForWikiFile(rel string) string {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "wiki/sources/"):
		return "Sources"
	case strings.HasPrefix(rel, "wiki/concepts/"):
		return "Concepts"
	case strings.HasPrefix(rel, "wiki/entities/"):
		return "Entities"
	case strings.HasPrefix(rel, "wiki/syntheses/"):
		return "Syntheses"
	case strings.HasPrefix(rel, "wiki/code/"):
		return "Code"
	default:
		return "Other"
	}
}

func appendReview(projectPath, sourceTitle, sourceRel string, review ReviewBlock, files []string) error {
	path := filepath.Join(projectPath, "wiki", "reviews.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("# Reviews\n\nOpen LLM review items, contradictions, missing pages, and human judgment calls.\n"), 0o644); err != nil {
			return err
		}
	}
	var affected strings.Builder
	for _, file := range files {
		fmt.Fprintf(&affected, "- `%s`\n", file)
	}
	entry := fmt.Sprintf("\n## [%s] %s | %s\n\n"+
		"- Source: `%s`\n"+
		"- Source title: %s\n"+
		"- Status: open\n\n"+
		"### Affected Pages\n\n"+
		"%s"+
		"### Detail\n\n"+
		"%s\n",
		today(), review.Type, review.Title, sourceRel, sourceTitle, affected.String(), strings.TrimSpace(review.Body))
	return appendUnique(path, entry)
}

func appendOverviewEntry(projectPath, section, title, rel, kind string) error {
	path := filepath.Join(projectPath, "wiki", "overview.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("# Overview\n\n"), 0o644); err != nil {
			return err
		}
	}
	slug := strings.TrimSuffix(filepath.Base(rel), ".md")
	entry := fmt.Sprintf("- %s: [[%s|%s]] (`%s`)\n", kind, slug, title, filepath.ToSlash(rel))
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
		if _, err := f.WriteString("\n## " + section + "\n\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(entry)
	return err
}

func pageTypeForOverview(absPath string) string {
	content := readOptional(absPath)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "type:") {
			pageType := strings.TrimSpace(strings.TrimPrefix(line, "type:"))
			pageType = strings.Trim(pageType, `"`)
			if pageType != "" {
				return pageType
			}
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

func titleForEntry(absPath, fallback string) string {
	if content := readOptional(absPath); content != "" {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "title:") {
				title := strings.TrimSpace(strings.TrimPrefix(line, "title:"))
				title = strings.Trim(title, `"`)
				if title != "" {
					return title
				}
			}
			if line == "---" || line == "" {
				continue
			}
			if strings.HasPrefix(line, "# ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "# "))
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	base := strings.TrimSuffix(filepath.Base(absPath), ".md")
	return core.Slug(base)
}

func appendUnique(path, text string) error {
	current, _ := os.ReadFile(path)
	if strings.Contains(string(current), text) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

func today() string {
	return time.Now().Format("2006-01-02")
}
