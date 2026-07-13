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
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/sourcearchive"
	"github.com/hejw/knowledge-core/internal/wiki"
)

var wikiPersistMu sync.Mutex

type ValidateOptions struct {
	ProjectPath   string
	SourcePath    string
	Title         string
	Provider      Provider
	SkipUnchanged bool
	OnProgress    func(ValidateProgress)
	OnCommitted   func(ValidateResult) error
	Concurrency   int
	SourceIndex   int
	SourceTotal   int
	Manifest      *SourceManifest
}

type ValidateProgress struct {
	Phase       string
	SourcePath  string
	SourceTitle string
	Index       int
	Total       int
	Files       int
	Reviews     int
	Duration    time.Duration
	Error       string
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

type analyzedSource struct {
	opts        ValidateOptions
	started     time.Time
	title       string
	rawRel      string
	hash        string
	manifestKey string
	extracted   extractedSource
	archive     *sourcearchive.Metadata
	input       AnalysisInput
	analysis    string
}

func progressEmitter(opts ValidateOptions, started time.Time) func(string, string, ValidateResult, error) {
	return func(phase, title string, result ValidateResult, err error) {
		if opts.OnProgress == nil {
			return
		}
		progress := ValidateProgress{
			Phase: phase, SourcePath: opts.SourcePath, SourceTitle: title,
			Index: opts.SourceIndex, Total: opts.SourceTotal,
			Files: len(result.Files), Reviews: result.ReviewCount, Duration: time.Since(started),
		}
		if err != nil {
			progress.Error = err.Error()
		}
		opts.OnProgress(progress)
	}
}

func ValidateLLMWiki(opts ValidateOptions) (ValidateResult, error) {
	work, skipped, err := analyzeLLMWikiSource(opts)
	if err != nil || skipped != nil {
		if skipped != nil {
			return *skipped, nil
		}
		return ValidateResult{}, err
	}
	return generateAndPersistLLMWikiSource(work)
}

func analyzeLLMWikiSource(opts ValidateOptions) (*analyzedSource, *ValidateResult, error) {
	started := time.Now()
	emit := progressEmitter(opts, started)
	if opts.Provider == nil {
		opts.Provider = MockProvider{}
	}
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return nil, nil, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.SourcePath) == "" {
		return nil, nil, fmt.Errorf("source path is required")
	}
	if existing, ok, findErr := sourcearchive.FindBySourcePath(opts.ProjectPath, opts.SourcePath); findErr != nil {
		return nil, nil, findErr
	} else if ok {
		opts.SourcePath = filepath.Join(opts.ProjectPath, filepath.FromSlash(existing.Metadata.OriginalRawPath))
	}
	originalBytes, err := os.ReadFile(opts.SourcePath)
	if err != nil {
		return nil, nil, err
	}
	hash := sourceHash(originalBytes)
	archive, legacyRaw, err := ensureSourceArchive(opts.ProjectPath, opts.SourcePath, originalBytes)
	if err != nil {
		return nil, nil, err
	}
	manifest := SourceManifest{}
	if opts.Manifest != nil {
		manifest = *opts.Manifest
	} else {
		manifest, err = loadSourceManifest(opts.ProjectPath)
		if err != nil {
			return nil, nil, err
		}
	}
	manifestKey := sourceManifestKey(opts.SourcePath)
	if archive != nil {
		for key, entry := range manifest.Sources {
			if filepath.ToSlash(entry.ArchivePath) == archive.ArchivePath {
				manifestKey = key
				break
			}
		}
	}
	archiveNeedsRepair := false
	if archive != nil && archive.ContentPath != archive.OriginalRawPath {
		content, readErr := os.ReadFile(filepath.Join(opts.ProjectPath, filepath.FromSlash(archive.ContentPath)))
		if readErr != nil || sourceHash(content) != archive.ContentSHA256 {
			archiveNeedsRepair = true
		}
	}
	if opts.SkipUnchanged {
		if entry, ok := manifest.Sources[manifestKey]; ok && entry.SHA256 == hash && entry.PipelineVersion >= core.SourceManifestPipelineVersion {
			if archiveNeedsRepair && archive != nil {
				extracted, extractErr := extractSourceText(opts.SourcePath, originalBytes)
				if extractErr != nil {
					return nil, nil, extractErr
				}
				updated, updateErr := sourcearchive.SetExtracted(opts.ProjectPath, archive.OriginalRawPath, extracted.Text, extracted.Extractor)
				if updateErr != nil {
					return nil, nil, updateErr
				}
				archive = &updated.Metadata
				entry.RawPath = archive.ContentPath
				entry.ContentPath = archive.ContentPath
				entry.ContentSHA256 = archive.ContentSHA256
				wikiPersistMu.Lock()
				latest, loadErr := loadSourceManifest(opts.ProjectPath)
				if loadErr == nil {
					latest.Sources[manifestKey] = entry
					loadErr = saveSourceManifest(opts.ProjectPath, latest)
					manifest = latest
				}
				wikiPersistMu.Unlock()
				if loadErr != nil {
					return nil, nil, loadErr
				}
				if opts.Manifest != nil {
					*opts.Manifest = manifest
				}
			}
			sourceTitle := firstNonEmpty(opts.Title, entry.Title, strings.TrimSuffix(filepath.Base(opts.SourcePath), filepath.Ext(opts.SourcePath)))
			result := ValidateResult{
				RawPath:     entry.RawPath,
				Files:       entry.Files,
				ReviewCount: entry.ReviewCount,
				Skipped:     true,
				SHA256:      hash,
			}
			emit("skipped", sourceTitle, result, nil)
			return nil, &result, nil
		}
	}
	extracted, err := extractSourceText(opts.SourcePath, originalBytes)
	if err != nil {
		return nil, nil, err
	}
	sourceBytes := extracted.Text
	sourceTitle := opts.Title
	if sourceTitle == "" {
		sourceTitle = inferSourceTitle(opts.SourcePath, string(sourceBytes))
	}
	var sourceRel string
	if archive != nil {
		if extracted.Extractor != "direct" {
			updated, updateErr := sourcearchive.SetExtracted(opts.ProjectPath, archive.OriginalRawPath, sourceBytes, extracted.Extractor)
			if updateErr != nil {
				return nil, nil, updateErr
			}
			archive = &updated.Metadata
		}
		sourceRel = archive.ContentPath
	} else if legacyRaw {
		sourceRel, err = copyRawSourceWithName(opts.ProjectPath, opts.SourcePath, sourceBytes, extracted.RawName, hash)
	} else {
		return nil, nil, fmt.Errorf("source archive was not created for %s", opts.SourcePath)
	}
	if err != nil {
		return nil, nil, err
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
	emit("analysis", sourceTitle, ValidateResult{}, nil)
	analysis, err := opts.Provider.Analyze(input)
	if err != nil {
		wrapped := fmt.Errorf("analysis: %w", err)
		emit("failed", sourceTitle, ValidateResult{}, wrapped)
		return nil, nil, wrapped
	}
	return &analyzedSource{
		opts: opts, started: started, title: sourceTitle, rawRel: sourceRel,
		hash: hash, manifestKey: manifestKey, extracted: extracted, archive: archive, input: input, analysis: analysis,
	}, nil, nil
}

func generateAndPersistLLMWikiSource(work *analyzedSource) (ValidateResult, error) {
	opts := work.opts
	emit := progressEmitter(opts, work.started)
	// Generation is serialized and refreshes shared navigation/existing pages,
	// so every source merges against the latest committed wiki state.
	wikiPersistMu.Lock()
	defer wikiPersistMu.Unlock()
	work.input.Purpose = readOptional(filepath.Join(opts.ProjectPath, "purpose.md"))
	work.input.Schema = readOptional(filepath.Join(opts.ProjectPath, "schema.md"))
	work.input.Index = readOptional(filepath.Join(opts.ProjectPath, "wiki", "index.md"))
	work.input.Overview = readOptional(filepath.Join(opts.ProjectPath, "wiki", "overview.md"))
	work.input.ExistingPages = existingPagesForAnalysis(opts.ProjectPath, work.analysis)
	emit("generation", work.title, ValidateResult{}, nil)
	generation, err := opts.Provider.Generate(work.analysis, work.input)
	if err != nil {
		wrapped := fmt.Errorf("generation: %w", err)
		emit("failed", work.title, ValidateResult{}, wrapped)
		return ValidateResult{}, wrapped
	}
	manifest, err := loadSourceManifest(opts.ProjectPath)
	if err != nil {
		return ValidateResult{}, err
	}
	if opts.Manifest != nil {
		*opts.Manifest = manifest
	}
	emit("persisting", work.title, ValidateResult{}, nil)
	blocks, err := ParseBlocks(generation)
	if err == nil {
		err = ValidateGeneratedBlocks(opts.ProjectPath, blocks, work.rawRel)
	}
	if err != nil {
		// Models sometimes return an otherwise useful page update without
		// repeating its YAML frontmatter. Give the provider one focused format
		// repair attempt before failing the whole source and retrying the batch.
		emit("generation_repair", work.title, ValidateResult{}, err)
		repairAnalysis := work.analysis + "\n\nFORMAT REPAIR REQUIRED: " + err.Error() + " Regenerate the complete output from scratch. Every ---FILE block must contain valid YAML frontmatter and preserve all existing-page evidence and sources. Return only valid ---FILE and ---REVIEW blocks."
		generation, repairErr := opts.Provider.Generate(repairAnalysis, work.input)
		if repairErr != nil {
			return ValidateResult{}, fmt.Errorf("%w; format repair: %v", err, repairErr)
		}
		blocks, err = ParseBlocks(generation)
		if err == nil {
			err = ValidateGeneratedBlocks(opts.ProjectPath, blocks, work.rawRel)
		}
		if err != nil {
			return ValidateResult{}, fmt.Errorf("%w; format repair: %v", err, err)
		}
	}
	var written []string
	for _, file := range blocks.Files {
		if err := wiki.WriteVersionedPage(opts.ProjectPath, file.Path, []byte(file.Content), "validate-llmwiki: "+work.title); err != nil {
			return ValidateResult{}, err
		}
		written = append(written, file.Path)
	}
	if err := updateAggregates(opts.ProjectPath, work.title, work.rawRel, written, blocks.Reviews); err != nil {
		return ValidateResult{}, err
	}
	entry := SourceManifestEntry{
		OriginalPath:    sourceManifestKey(opts.SourcePath),
		PipelineVersion: core.SourceManifestPipelineVersion,
		SHA256:          work.hash,
		RawPath:         work.rawRel,
		Title:           work.title,
		Files:           written,
		ReviewCount:     len(blocks.Reviews),
		UpdatedAt:       time.Now().Format(time.RFC3339),
		Extraction: &SourceExtractionMetadata{
			SourceExt:   work.extracted.SourceExt,
			Extractor:   work.extracted.Extractor,
			ExtractedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if work.archive != nil {
		entry.OriginalPath = sourceManifestKey(opts.SourcePath)
		entry.ArchivePath = work.archive.ArchivePath
		entry.OriginalRawPath = work.archive.OriginalRawPath
		entry.ContentPath = work.archive.ContentPath
		entry.OriginalSHA256 = work.archive.OriginalSHA256
		entry.ContentSHA256 = work.archive.ContentSHA256
	}
	manifest.Sources[work.manifestKey] = entry
	if err := saveSourceManifest(opts.ProjectPath, manifest); err != nil {
		return ValidateResult{}, err
	}
	if opts.Manifest != nil {
		*opts.Manifest = manifest
	}
	result := ValidateResult{
		RawPath:     work.rawRel,
		Analysis:    work.analysis,
		Files:       written,
		ReviewCount: len(blocks.Reviews),
		SHA256:      work.hash,
	}
	if opts.OnCommitted != nil {
		if err := opts.OnCommitted(result); err != nil {
			return ValidateResult{}, fmt.Errorf("post-commit sync: %w", err)
		}
	}
	emit("completed", work.title, result, nil)
	return result, nil
}

func ValidateLLMWikiPath(opts ValidateOptions) (BatchValidateResult, error) {
	info, err := os.Stat(opts.SourcePath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	if !info.IsDir() {
		if opts.SourceIndex <= 0 {
			opts.SourceIndex = 1
		}
		if opts.SourceTotal <= 0 {
			opts.SourceTotal = 1
		}
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
	manifest, err := loadSourceManifest(opts.ProjectPath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	var batch BatchValidateResult
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency == 1 {
		for index, source := range sources {
			result, err := ValidateLLMWiki(ValidateOptions{
				ProjectPath:   opts.ProjectPath,
				SourcePath:    source,
				Provider:      opts.Provider,
				SkipUnchanged: opts.SkipUnchanged,
				OnProgress:    opts.OnProgress,
				OnCommitted:   opts.OnCommitted,
				SourceIndex:   index + 1,
				SourceTotal:   len(sources),
				Manifest:      &manifest,
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
	type item struct {
		index   int
		work    *analyzedSource
		skipped *ValidateResult
		err     error
	}
	jobs := make(chan int)
	results := make(chan item, len(sources))
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				source := sources[index]
				work, skipped, err := analyzeLLMWikiSource(ValidateOptions{ProjectPath: opts.ProjectPath, SourcePath: source, Provider: opts.Provider, SkipUnchanged: opts.SkipUnchanged, OnProgress: opts.OnProgress, OnCommitted: opts.OnCommitted, SourceIndex: index + 1, SourceTotal: len(sources)})
				results <- item{index: index, work: work, skipped: skipped, err: err}
			}
		}()
	}
	go func() {
		for i := range sources {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	prepared := make([]item, len(sources))
	for r := range results {
		prepared[r.index] = r
	}
	var firstErr error
	for index, preparedItem := range prepared {
		if preparedItem.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("validate %s: %w", sources[index], preparedItem.err)
			}
			continue
		}
		var result ValidateResult
		if preparedItem.skipped != nil {
			result = *preparedItem.skipped
		} else {
			var err error
			preparedItem.work.opts.Manifest = &manifest
			result, err = generateAndPersistLLMWikiSource(preparedItem.work)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("validate %s: %w", sources[index], err)
				}
				continue
			}
		}
		batch.Results = append(batch.Results, result)
		batch.SourceCount++
		batch.FileCount += filesWritten(result)
		batch.ReviewCount += reviewsWritten(result)
		batch.SkippedCount += skippedCount(result)
	}
	if firstErr != nil {
		return batch, firstErr
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
	return copyRawSourceWithName(projectPath, sourcePath, data, filepath.Base(sourcePath), sourceHash(data))
}

func copyRawSourceWithName(projectPath, sourcePath string, data []byte, rawName string, hash string) (string, error) {
	projectRawRoot, err := filepath.Abs(filepath.Join(projectPath, "raw", "sources"))
	if err != nil {
		return "", err
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", err
	}
	if filepath.Base(rawName) == filepath.Base(sourcePath) {
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
	}
	name := filepath.Base(rawName)
	if name == "." || name == "/" || strings.TrimSpace(name) == "" {
		name = filepath.Base(sourcePath)
	}
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
	OriginalPath    string                    `json:"original_path"`
	PipelineVersion int                       `json:"pipeline_version,omitempty"`
	SHA256          string                    `json:"sha256"`
	RawPath         string                    `json:"raw_path"`
	ArchivePath     string                    `json:"archive_path,omitempty"`
	OriginalRawPath string                    `json:"original_raw_path,omitempty"`
	ContentPath     string                    `json:"content_path,omitempty"`
	OriginalSHA256  string                    `json:"original_sha256,omitempty"`
	ContentSHA256   string                    `json:"content_sha256,omitempty"`
	Title           string                    `json:"title"`
	Files           []string                  `json:"files"`
	ReviewCount     int                       `json:"review_count"`
	UpdatedAt       string                    `json:"updated_at"`
	Extraction      *SourceExtractionMetadata `json:"extraction,omitempty"`
}

type SourceExtractionMetadata struct {
	SourceExt   string `json:"source_ext"`
	Extractor   string `json:"extractor"`
	ExtractedAt string `json:"extracted_at"`
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
			ID:              core.StableID(projectID, "source-manifest", originalPath),
			ProjectID:       projectID,
			OriginalPath:    originalPath,
			PipelineVersion: entry.PipelineVersion,
			SHA256:          entry.SHA256,
			RawPath:         entry.RawPath,
			ArchivePath:     entry.ArchivePath,
			OriginalRawPath: entry.OriginalRawPath,
			ContentPath:     entry.ContentPath,
			OriginalSHA256:  entry.OriginalSHA256,
			ContentSHA256:   entry.ContentSHA256,
			Title:           entry.Title,
			Files:           append([]string(nil), entry.Files...),
			ReviewCount:     entry.ReviewCount,
			UpdatedAt:       updatedAt,
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

func ensureSourceArchive(projectPath, sourcePath string, originalBytes []byte) (*sourcearchive.Metadata, bool, error) {
	if archive, ok, err := sourcearchive.FindBySourcePath(projectPath, sourcePath); err != nil {
		return nil, false, err
	} else if ok {
		hash := sourceHash(originalBytes)
		if archive.Metadata.OriginalSHA256 != hash {
			return nil, false, fmt.Errorf("source archive original hash mismatch: %s", archive.Metadata.OriginalRawPath)
		}
		metadata := archive.Metadata
		return &metadata, false, nil
	}
	rawRoot, err := filepath.Abs(filepath.Join(projectPath, "raw", "sources"))
	if err != nil {
		return nil, false, err
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, false, err
	}
	if rel, relErr := filepath.Rel(rawRoot, sourceAbs); relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Legacy flat/nested raw sources remain readable until the explicit
		// source-layout migration moves them into archives.
		return nil, true, nil
	}
	archive, err := sourcearchive.ImportBytes(sourcearchive.ImportOptions{
		ProjectPath:      projectPath,
		RelativePath:     filepath.Base(sourcePath),
		OriginalLocation: sourceAbs,
	}, originalBytes)
	if err != nil {
		return nil, false, err
	}
	metadata := archive.Metadata
	return &metadata, false, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func readOptional(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func existingPagesForAnalysis(projectPath, analysis string) string {
	paths := map[string]bool{}
	for rest := analysis; ; {
		start := strings.Index(rest, "wiki/")
		if start < 0 {
			break
		}
		rest = rest[start:]
		end := strings.Index(rest, ".md")
		if end < 0 {
			break
		}
		candidate := strings.Trim(rest[:end+3], "`'\"()[]{}<>,;:")
		if validateWikiFilePath(candidate) == nil {
			paths[filepath.ToSlash(candidate)] = true
		}
		rest = rest[end+3:]
	}
	if pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath}); err == nil {
		for _, page := range pages {
			if page.Title != "" && len([]rune(page.Title)) >= 2 && strings.Contains(analysis, page.Title) {
				paths[page.Path] = true
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	if len(ordered) > 12 {
		ordered = ordered[:12]
	}
	var b strings.Builder
	for _, path := range ordered {
		content := readOptional(filepath.Join(projectPath, filepath.FromSlash(path)))
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n---EXISTING PAGE: %s\n%s\n", path, content)
		if b.Len() >= 48000 {
			break
		}
	}
	return b.String()
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
