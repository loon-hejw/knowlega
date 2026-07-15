package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
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
	ProjectPath                string
	ProjectName                string
	SourcePath                 string
	ReuseExisting              bool
	ExpectedGenerationContract string
	Compile                    ProjectCompileFunc
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
		sources, err := validateBootstrapProject(projectPath, opts.SourcePath, opts.ExpectedGenerationContract)
		if err == nil {
			if purposeErr := wiki.ValidatePurposeReady(projectPath); purposeErr != nil {
				return PrepareProjectResult{}, permanentBootstrapError("project purpose is not ready: %v", purposeErr)
			}
			return PrepareProjectResult{Reused: true, SourceCount: sources}, nil
		}
		if opts.Compile == nil {
			return PrepareProjectResult{}, permanentBootstrapError("existing project is incomplete; no files were changed: %w", err)
		}
		if resumeErr := validateBootstrapResumeCandidate(projectPath, opts.SourcePath, opts.ExpectedGenerationContract); resumeErr != nil {
			return PrepareProjectResult{}, permanentBootstrapError("existing project is incomplete and cannot be resumed safely; no files were changed: %w", resumeErr)
		}
		if purposeErr := wiki.ValidatePurposeReady(projectPath); purposeErr != nil {
			return PrepareProjectResult{}, permanentBootstrapError("project purpose is not ready: %v", purposeErr)
		}
		result, compileErr := opts.Compile(projectPath, opts.SourcePath)
		if compileErr != nil {
			return PrepareProjectResult{}, fmt.Errorf("resume bootstrap LLM Wiki (generated evidence was preserved): %w", compileErr)
		}
		if _, validateErr := validateBootstrapProject(projectPath, opts.SourcePath, opts.ExpectedGenerationContract); validateErr != nil {
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
	if purposeErr := wiki.ValidatePurposeReady(projectPath); purposeErr != nil {
		return PrepareProjectResult{}, permanentBootstrapError("project purpose is not ready: %v", purposeErr)
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

// RestoreBootstrapTracker seeds durable completion before workers enumerate
// unchanged sources, so readiness progress reflects the manifest immediately.
func RestoreBootstrapTracker(tracker *BootstrapTracker, projectPath, sourcePath, expectedContract string) error {
	if tracker == nil {
		return nil
	}
	manifest, err := loadBootstrapManifest(projectPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type restoredTaskState struct {
		Status           string   `json:"status"`
		ConflictAttempts int      `json:"conflict_attempts"`
		ConflictPaths    []string `json:"conflict_paths"`
	}
	stateStatus := map[string]restoredTaskState{}
	data, stateErr := os.ReadFile(filepath.Join(projectPath, ".kbcore", "bootstrap-state.json"))
	if stateErr == nil {
		var state struct {
			Sources map[string]restoredTaskState `json:"sources"`
		}
		if json.Unmarshal(data, &state) == nil {
			for source, item := range state.Sources {
				stateStatus[filepath.ToSlash(source)] = item
			}
		}
	}
	sources, err := bootstrapSourceFiles(sourcePath)
	if err != nil {
		return err
	}
	for _, source := range sources {
		abs, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(abs)
		if state, exists := stateStatus[key]; exists && (state.Status == "conflicted" || (state.Status == "processing" && state.ConflictAttempts > 0 && len(state.ConflictPaths) > 0)) {
			tracker.SeedQueuedConflict(abs)
		}
		entry, ok := manifest[key]
		if !ok || entry.PipelineVersion < core.SourceManifestPipelineVersion || (expectedContract != "" && entry.GenerationContractSHA256 != expectedContract) {
			continue
		}
		if state, exists := stateStatus[key]; exists && state.Status != "settled" {
			continue
		}
		hash, err := sourceFileSHA256(source)
		if err != nil || hash != entry.SHA256 {
			continue
		}
		tracker.SeedCompleted(abs, len(entry.Files), entry.ReviewCount)
	}
	return nil
}

func validateBootstrapResumeCandidate(projectPath, sourcePath, expectedContract string) error {
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
	hasStaleContract := false
	for key, entry := range manifest {
		source, ok := allowed[filepath.ToSlash(key)]
		if !ok {
			return fmt.Errorf("source manifest contains an entry outside the configured bootstrap corpus: %s", key)
		}
		actualHash, err := sourceFileSHA256(source)
		if err != nil {
			return err
		}
		if entry.SHA256 != actualHash {
			return fmt.Errorf("source manifest hash is stale for %s", key)
		}
		if entry.PipelineVersion < core.SourceManifestPipelineVersion {
			hasStalePipeline = true
		}
		if expectedContract != "" && entry.GenerationContractSHA256 != expectedContract {
			hasStaleContract = true
		}
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(entry.RawPath))); err != nil {
			return fmt.Errorf("raw source %s: %w", entry.RawPath, err)
		}
		if !entryHasSourceSummary(entry, pageByPath) {
			return fmt.Errorf("source manifest entry %s has no valid source-summary page", key)
		}
	}
	if len(manifest) >= len(sources) && !hasStalePipeline && !hasStaleContract {
		if taskStateErr := validateBootstrapTaskStateSettled(projectPath); taskStateErr != nil {
			return nil
		}
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
	return validateBootstrapProject(projectPath, sourcePath, "")
}

func validateBootstrapProject(projectPath, sourcePath, expectedContract string) (int, error) {
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
		actualHash, err := sourceFileSHA256(source)
		if err != nil {
			return 0, err
		}
		if entry.SHA256 != actualHash {
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
		if expectedContract != "" && entry.GenerationContractSHA256 != expectedContract {
			return 0, fmt.Errorf("source manifest generation contract is stale for %s", filepath.ToSlash(abs))
		}
	}
	if err := validateBootstrapTaskStateSettled(projectPath); err != nil {
		return 0, err
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

func validateBootstrapTaskStateSettled(projectPath string) error {
	intentDir := filepath.Join(projectPath, ".kbcore", "bootstrap-intents")
	if entries, err := os.ReadDir(intentDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
				return fmt.Errorf("bootstrap task state has a pending commit intent: %s", entry.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("bootstrap commit intents: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(projectPath, ".kbcore", "bootstrap-state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bootstrap task state: %w", err)
	}
	var state struct {
		Sources map[string]struct {
			Status string `json:"status"`
		} `json:"sources"`
		PendingImpacts map[string][]string `json:"pending_impacts"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("bootstrap task state: %w", err)
	}
	if len(state.PendingImpacts) > 0 {
		return fmt.Errorf("bootstrap task state has %d pending impact check(s)", len(state.PendingImpacts))
	}
	for source, task := range state.Sources {
		if task.Status != "settled" {
			return fmt.Errorf("bootstrap source task is not settled: %s status=%s", source, task.Status)
		}
	}
	return nil
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
	manifest, err := manifestfile.Load(projectPath)
	if err != nil {
		return nil, err
	}
	out := make(map[string]core.SourceManifestEntry, len(manifest.Sources))
	for key, value := range manifest.Sources {
		original := value.OriginalPath
		if strings.TrimSpace(original) == "" {
			original = key
		}
		out[filepath.ToSlash(original)] = core.SourceManifestEntry{
			OriginalPath:             original,
			PipelineVersion:          value.PipelineVersion,
			SHA256:                   value.SHA256,
			RawPath:                  value.RawPath,
			Files:                    value.Files,
			GenerationContractSHA256: value.GenerationContractSHA256,
			NewPageBudget:            value.NewPageBudget,
			NewPageCount:             value.NewPageCount,
			CreatedPages:             value.CreatedPages,
			ReviewCount:              value.ReviewCount,
		}
	}
	return out, nil
}
