package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type PermanentBootstrapError struct{ Err error }

func (e PermanentBootstrapError) Error() string { return e.Err.Error() }
func (e PermanentBootstrapError) Unwrap() error { return e.Err }

func IsPermanentBootstrapError(err error) bool {
	var target PermanentBootstrapError
	return errors.As(err, &target)
}

func permanentBootstrapError(format string, args ...any) error {
	return PermanentBootstrapError{Err: fmt.Errorf(format, args...)}
}

type PrepareProjectOptions struct {
	ProjectPath   string
	ProjectName   string
	SourcePath    string
	ReuseExisting bool
	Compile       ProjectCompileFunc
}

type ProjectCompileFunc func(projectPath, sourcePath string) (ProjectCompileResult, error)

type ProjectCompileResult struct {
	SourceCount  int
	FileCount    int
	ReviewCount  int
	WrittenPaths []string
}

type PrepareProjectResult struct {
	Reused       bool
	Resumed      bool
	SourceCount  int
	FileCount    int
	ReviewCount  int
	WrittenPaths []string
}

func PrepareProject(opts PrepareProjectOptions) (PrepareProjectResult, error) {
	projectPath := strings.TrimSpace(opts.ProjectPath)
	if projectPath == "" {
		return PrepareProjectResult{}, permanentBootstrapError("project path is required")
	}
	info, err := os.Lstat(projectPath)
	if err == nil {
		if !info.IsDir() {
			return PrepareProjectResult{}, permanentBootstrapError("configured project path exists but is not a directory: %s", projectPath)
		}
		if !opts.ReuseExisting {
			return PrepareProjectResult{}, permanentBootstrapError("configured project already exists and project.bootstrap.reuse_existing is false: %s", projectPath)
		}
		sources, err := ValidateBootstrapProject(projectPath, opts.SourcePath)
		if err == nil {
			return PrepareProjectResult{Reused: true, SourceCount: sources}, nil
		}
		if opts.Compile == nil {
			return PrepareProjectResult{}, permanentBootstrapError("existing project is incomplete; no files were changed: %w", err)
		}
		if resumeErr := validateBootstrapResumeCandidate(projectPath, opts.SourcePath); resumeErr != nil {
			return PrepareProjectResult{}, permanentBootstrapError("existing project is incomplete and cannot be resumed safely; no files were changed: %w", resumeErr)
		}
		result, compileErr := opts.Compile(projectPath, opts.SourcePath)
		if compileErr != nil {
			return PrepareProjectResult{}, fmt.Errorf("resume bootstrap LLM Wiki (generated evidence was preserved): %w", compileErr)
		}
		if _, validateErr := ValidateBootstrapProject(projectPath, opts.SourcePath); validateErr != nil {
			return PrepareProjectResult{}, permanentBootstrapError("validate resumed project (generated evidence was preserved): %w", validateErr)
		}
		return PrepareProjectResult{
			Resumed:      true,
			SourceCount:  result.SourceCount,
			FileCount:    result.FileCount,
			ReviewCount:  result.ReviewCount,
			WrittenPaths: result.WrittenPaths,
		}, nil
	}
	if !os.IsNotExist(err) {
		return PrepareProjectResult{}, permanentBootstrapError("inspect configured project path: %w", err)
	}
	if opts.Compile == nil {
		return PrepareProjectResult{}, permanentBootstrapError("LLM ingest provider is required to initialize project")
	}
	if err := wiki.InitProject(wiki.ProjectOptions{Path: projectPath, Name: opts.ProjectName}); err != nil {
		return PrepareProjectResult{}, permanentBootstrapError("initialize project: %w", err)
	}
	batch, err := opts.Compile(projectPath, opts.SourcePath)
	if err != nil {
		return PrepareProjectResult{}, fmt.Errorf("bootstrap LLM Wiki (generated evidence was preserved): %w", err)
	}
	if _, err := ValidateBootstrapProject(projectPath, opts.SourcePath); err != nil {
		return PrepareProjectResult{}, permanentBootstrapError("validate initialized project (generated evidence was preserved): %w", err)
	}
	return PrepareProjectResult{
		SourceCount:  batch.SourceCount,
		FileCount:    batch.FileCount,
		ReviewCount:  batch.ReviewCount,
		WrittenPaths: batch.WrittenPaths,
	}, nil
}

func CountBootstrapSources(path string) (int, error) {
	sources, err := bootstrapSourceFiles(path)
	return len(sources), err
}

func validateBootstrapResumeCandidate(projectPath, sourcePath string) error {
	if err := validateBootstrapScaffold(projectPath); err != nil {
		return err
	}
	sources, err := bootstrapSourceFiles(sourcePath)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("bootstrap source contains no .txt or .md files")
	}
	manifestPath := filepath.Join(projectPath, ".kbcore", "source-manifest.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	manifest, err := loadBootstrapManifest(projectPath)
	if err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	if len(manifest) > len(sources) {
		return fmt.Errorf("source manifest contains more entries than the configured bootstrap corpus")
	}
	allowed := make(map[string]string, len(sources))
	for _, source := range sources {
		abs, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		allowed[filepath.ToSlash(abs)] = source
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "bootstrap-resume"})
	if err != nil {
		return fmt.Errorf("scan wiki pages: %w", err)
	}
	pageByPath := make(map[string]core.WikiPage, len(pages))
	for _, page := range pages {
		pageByPath[page.Path] = page
	}
	hasStalePipeline := false
	for key, entry := range manifest {
		source, ok := allowed[filepath.ToSlash(key)]
		if !ok {
			return fmt.Errorf("source manifest contains an entry outside the configured bootstrap corpus: %s", key)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if entry.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("source manifest hash is stale for %s", key)
		}
		if entry.PipelineVersion < core.SourceManifestPipelineVersion {
			hasStalePipeline = true
		}
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(entry.RawPath))); err != nil {
			return fmt.Errorf("raw source %s: %w", entry.RawPath, err)
		}
		if !entryHasSourceSummary(entry, pageByPath) {
			return fmt.Errorf("source manifest entry %s has no valid source-summary page", key)
		}
	}
	if len(manifest) >= len(sources) && !hasStalePipeline {
		return fmt.Errorf("source manifest is not partial; refusing to rewrite a structurally invalid project")
	}
	return nil
}

func validateBootstrapScaffold(projectPath string) error {
	for _, rel := range []string{
		"purpose.md", "schema.md", "wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md",
		"raw/sources", "wiki/sources", "wiki/syntheses",
	} {
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("required project path %s: %w", rel, err)
		}
	}
	return nil
}

func ValidateBootstrapProject(projectPath, sourcePath string) (int, error) {
	if err := validateBootstrapScaffold(projectPath); err != nil {
		return 0, err
	}
	sources, err := bootstrapSourceFiles(sourcePath)
	if err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		return 0, fmt.Errorf("bootstrap source contains no .txt or .md files")
	}
	entries, err := loadBootstrapManifest(projectPath)
	if err != nil {
		return 0, fmt.Errorf("source manifest: %w", err)
	}
	manifest := entries
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "bootstrap-validation"})
	if err != nil {
		return 0, fmt.Errorf("scan wiki pages: %w", err)
	}
	pageByPath := make(map[string]core.WikiPage, len(pages))
	for _, page := range pages {
		pageByPath[page.Path] = page
	}
	for _, source := range sources {
		abs, err := filepath.Abs(source)
		if err != nil {
			return 0, err
		}
		entry, ok := manifest[filepath.ToSlash(abs)]
		if !ok {
			return 0, fmt.Errorf("source manifest is missing %s", filepath.ToSlash(abs))
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(data)
		if entry.SHA256 != hex.EncodeToString(sum[:]) {
			return 0, fmt.Errorf("source manifest hash is stale for %s", filepath.ToSlash(abs))
		}
		if strings.TrimSpace(entry.RawPath) == "" {
			return 0, fmt.Errorf("source manifest raw path is missing for %s", filepath.ToSlash(abs))
		}
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(entry.RawPath))); err != nil {
			return 0, fmt.Errorf("raw source %s: %w", entry.RawPath, err)
		}
		if !entryHasSourceSummary(entry, pageByPath) {
			return 0, fmt.Errorf("source manifest entry %s has no valid source-summary page", filepath.ToSlash(abs))
		}
		if entry.PipelineVersion < core.SourceManifestPipelineVersion {
			return 0, fmt.Errorf("source manifest pipeline is stale for %s", filepath.ToSlash(abs))
		}
	}
	issues, err := LintWiki(projectPath)
	if err != nil {
		return 0, fmt.Errorf("structural lint: %w", err)
	}
	// Structural lint findings describe wiki health and maintenance work; they
	// must not make an otherwise readable, fully checkpointed project
	// permanently unavailable. Parse/IO failures above remain fatal.
	_ = issues
	return len(sources), nil
}

func entryHasSourceSummary(entry core.SourceManifestEntry, pages map[string]core.WikiPage) bool {
	for _, path := range entry.Files {
		page, ok := pages[filepath.ToSlash(path)]
		if !ok || page.Type != "source-summary" || !strings.HasPrefix(page.Path, "wiki/sources/") {
			continue
		}
		for _, source := range page.Sources {
			if filepath.ToSlash(source) == filepath.ToSlash(entry.RawPath) {
				return true
			}
		}
	}
	return false
}

func bootstrapSourceFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap source: %w", err)
	}
	if !info.IsDir() {
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".txt" && ext != ".md" {
			return nil, fmt.Errorf("bootstrap source file must be .txt or .md")
		}
		return []string{path}, nil
	}
	var sources []string
	err = filepath.WalkDir(path, func(candidate string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		ext := strings.ToLower(filepath.Ext(candidate))
		if ext == ".txt" || ext == ".md" {
			sources = append(sources, candidate)
		}
		return nil
	})
	sort.Strings(sources)
	return sources, err
}

func loadBootstrapManifest(projectPath string) (map[string]core.SourceManifestEntry, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, ".kbcore", "source-manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Sources map[string]struct {
			OriginalPath    string   `json:"original_path"`
			PipelineVersion int      `json:"pipeline_version"`
			SHA256          string   `json:"sha256"`
			RawPath         string   `json:"raw_path"`
			Files           []string `json:"files"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	out := make(map[string]core.SourceManifestEntry, len(manifest.Sources))
	for key, value := range manifest.Sources {
		original := value.OriginalPath
		if strings.TrimSpace(original) == "" {
			original = key
		}
		out[filepath.ToSlash(original)] = core.SourceManifestEntry{
			OriginalPath:    original,
			PipelineVersion: value.PipelineVersion,
			SHA256:          value.SHA256,
			RawPath:         value.RawPath,
			Files:           value.Files,
		}
	}
	return out, nil
}
