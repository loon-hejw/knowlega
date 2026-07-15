package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
	"github.com/hejw/knowledge-core/internal/projectlock"
	"github.com/hejw/knowledge-core/internal/promptbudget"
	"github.com/hejw/knowledge-core/internal/sourcearchive"
	"github.com/hejw/knowledge-core/internal/wiki"
	"gopkg.in/yaml.v3"
)

var wikiPersistMu sync.Mutex
var sourceCommitTestHook func(stage, path string) error

type ValidateOptions struct {
	ProjectPath          string
	SourcePath           string
	Title                string
	Provider             Provider
	SkipUnchanged        bool
	OnProgress           func(ValidateProgress)
	OnCommitted          func(ValidateResult) error
	OnLLMCall            func(LLMCallMetric)
	Concurrency          int
	LLMConcurrency       int
	MaxTaskAttempts      int
	MaxConflictAttempts  int
	MaxImpactAttempts    int
	MaxFilesPerTask      int
	MaxNewPagesPerSource int
	UpdateOnly           bool
	ImpactAssessor       ImpactAssessor
	TaskAttempt          int
	ManagedTask          bool
	SourceIndex          int
	SourceTotal          int
	Manifest             *SourceManifest
	AllowedUpdatePaths   []string
	AnalysisPlan         string
	ProjectLockHeld      bool
}

type LLMCallMetric struct {
	Kind      string
	ID        string
	Started   bool
	StartedAt time.Time
	Duration  time.Duration
	Failed    bool
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
	Attempt     int
	Reason      string
}

type ValidateResult struct {
	RawPath      string
	Analysis     string
	Files        []string
	ReviewCount  int
	Skipped      bool
	SHA256       string
	Dependencies []string
	PageChanges  []PageChange
	CommitIntent string
}

type BatchValidateResult struct {
	Results      []ValidateResult
	SourceCount  int
	FileCount    int
	ReviewCount  int
	SkippedCount int
}

type analyzedSource struct {
	opts         ValidateOptions
	started      time.Time
	title        string
	rawRel       string
	hash         string
	manifestKey  string
	extracted    extractedSource
	archive      *sourcearchive.Metadata
	input        AnalysisInput
	analysis     string
	dependencies []string
	pageSnapshot map[string]string
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
			Attempt: opts.TaskAttempt,
		}
		if err != nil {
			progress.Error = err.Error()
		}
		opts.OnProgress(progress)
	}
}

func ValidateLLMWiki(opts ValidateOptions) (ValidateResult, error) {
	if !opts.ProjectLockHeld {
		lock, err := projectlock.Acquire(opts.ProjectPath)
		if err != nil {
			return ValidateResult{}, err
		}
		defer lock.Release()
		if _, err := wiki.RecoverFileTransactions(opts.ProjectPath); err != nil {
			return ValidateResult{}, fmt.Errorf("recover source transactions: %w", err)
		}
		opts.ProjectLockHeld = true
	}
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
	originalBytes, err := readBoundedSource(opts.SourcePath)
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
	purpose := readOptional(filepath.Join(opts.ProjectPath, "purpose.md"))
	schema := readOptional(filepath.Join(opts.ProjectPath, "schema.md"))
	previousEntry, hasPreviousEntry := manifest.Sources[manifestKey]
	policy, generationContract := generationPolicyFor(opts, previousEntry, hasPreviousEntry, hash, purpose, schema)
	if opts.SkipUnchanged {
		if entry, ok := manifest.Sources[manifestKey]; ok && entry.SHA256 == hash && entry.PipelineVersion >= core.SourceManifestPipelineVersion && entry.GenerationContractSHA256 == generationContract {
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
		SourceTitle:      sourceTitle,
		SourceRel:        sourceRel,
		SourceText:       string(sourceBytes),
		Purpose:          purpose,
		Schema:           schema,
		Index:            readOptional(filepath.Join(opts.ProjectPath, "wiki", "index.md")),
		Overview:         readOptional(filepath.Join(opts.ProjectPath, "wiki", "overview.md")),
		GenerationPolicy: policy,
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

func readBoundedSource(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > core.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	data, err := io.ReadAll(io.LimitReader(file, core.MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > core.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	return data, nil
}

func sourcePathSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > core.MaxSourceBytes {
		return "", fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, core.MaxSourceBytes+1))
	if err != nil {
		return "", err
	}
	if written > core.MaxSourceBytes {
		return "", fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readSourceExcerpt(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > core.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	if info.Size() <= maxBytes {
		return io.ReadAll(io.LimitReader(file, maxBytes+1))
	}
	headSize := maxBytes / 2
	tailSize := maxBytes - headSize
	head := make([]byte, headSize)
	if _, err := io.ReadFull(file, head); err != nil {
		return nil, err
	}
	if _, err := file.Seek(-tailSize, io.SeekEnd); err != nil {
		return nil, err
	}
	tail := make([]byte, tailSize)
	if _, err := io.ReadFull(file, tail); err != nil {
		return nil, err
	}
	value := append(head, []byte("\n...[source excerpt truncated]...\n")...)
	value = append(value, tail...)
	return []byte(strings.ToValidUTF8(string(value), "�")), nil
}

func generateAndPersistLLMWikiSource(work *analyzedSource) (ValidateResult, error) {
	opts := work.opts
	opts.AnalysisPlan = work.analysis
	emit := progressEmitter(opts, work.started)
	// Capture a consistent evidence snapshot, then release the commit lock while
	// the slow LLM generation runs. Complete source tasks generate optimistically
	// in parallel; the snapshot is checked immediately before the atomic commit
	// and only stale source tasks are requeued.
	wikiPersistMu.Lock()
	work.input.Purpose = readOptional(filepath.Join(opts.ProjectPath, "purpose.md"))
	work.input.Schema = readOptional(filepath.Join(opts.ProjectPath, "schema.md"))
	work.input.Index = readOptional(filepath.Join(opts.ProjectPath, "wiki", "index.md"))
	work.input.Overview = readOptional(filepath.Join(opts.ProjectPath, "wiki", "overview.md"))
	work.input.ExistingPages, work.dependencies = existingPagesForAnalysis(opts.ProjectPath, work.analysis)
	summaryUpdatePaths := sourceSummaryUpdatePaths(opts.ProjectPath, work.manifestKey)
	opts.AllowedUpdatePaths = sortedUniqueStrings(append(append([]string(nil), work.dependencies...), summaryUpdatePaths...))
	work.input.GenerationPolicy = effectiveGenerationPolicy(work.input.GenerationPolicy, len(work.dependencies))
	work.pageSnapshot = snapshotWikiPageHashes(opts.ProjectPath)
	wikiPersistMu.Unlock()
	emit("generation", work.title, ValidateResult{}, nil)
	generation, err := opts.Provider.Generate(work.analysis, work.input)
	if err != nil {
		wrapped := fmt.Errorf("generation: %w", err)
		emit("failed", work.title, ValidateResult{}, wrapped)
		return ValidateResult{}, wrapped
	}
	emit("persisting", work.title, ValidateResult{}, nil)
	blocks, err := ParseBlocks(generation)
	if err == nil {
		blocks = canonicalizeGeneratedPaths(blocks)
		sanitizeGeneratedAliasCollisions(opts.ProjectPath, &blocks)
		filterUnplannedNewPages(opts, &blocks)
		downgradeMissingReviewWikilinks(opts.ProjectPath, &blocks)
		if conflicts := newlyAppearedUnsuppliedPaths(opts, work.pageSnapshot, blocks); len(conflicts) > 0 {
			return ValidateResult{}, sourceConflictError{Paths: conflicts, Reason: "an unsupplied wiki page appeared after the evidence snapshot"}
		}
		filterUnsuppliedExistingPages(opts, work.pageSnapshot, &blocks)
		err = validateGeneratedBlocksForPolicy(opts, blocks, work.rawRel, work.input.GenerationPolicy)
	}
	if err != nil && downgradeMissingLinksIfValid(opts, &blocks, work.rawRel, work.input.GenerationPolicy) {
		err = nil
	}
	if err != nil {
		conflictPaths := canonicalIdentityConflictPaths(opts.ProjectPath, blocks)
		if discovered := pathsAbsentFromSnapshot(work.pageSnapshot, conflictPaths); len(discovered) > 0 {
			return ValidateResult{}, sourceConflictError{Paths: discovered, Reason: "canonical merge target was discovered after the evidence snapshot"}
		}
		work.input.ExistingPages = appendCanonicalConflictPages(opts.ProjectPath, work.input.ExistingPages, blocks)
		work.dependencies = sortedUniqueStrings(append(work.dependencies, conflictPaths...))
		opts.AllowedUpdatePaths = sortedUniqueStrings(append(append([]string(nil), work.dependencies...), summaryUpdatePaths...))
		// Models sometimes return an otherwise useful page update with invalid
		// frontmatter or unresolved wikilinks. Give the provider one focused
		// validation repair before requeueing the whole source task.
		emit("generation_repair", work.title, ValidateResult{}, err)
		repairAnalysis := work.analysis + "\n\nVALIDATION REPAIR REQUIRED: " + err.Error() + "\n" + generationPolicyText(work.input.GenerationPolicy) + `
Regenerate the complete output from scratch.
- The first block must be exactly one source-summary ---FILE under wiki/sources/ for the current source. Emit it before all other pages.
- Every ---FILE block must contain valid YAML frontmatter.
- For every unresolved wikilink, either include a complete target ---FILE block when it is a valuable durable page, link to an existing supplied page/title/alias, or render the wording as plain text without [[brackets]]. Never return an unresolved wikilink.
- If the validation error reports a title or alias collision, keep the supplied canonical page at its exact existing path, merge the evidence there, and remove every competing new page block.
- Preserve all existing-page evidence. Copy every existing sources entry and add the current source; never shorten or replace provenance.
- Drop the least durable proposed new pages until every numeric generation budget above is satisfied.
- Return only valid ---FILE and ---REVIEW blocks.`
		generation, repairErr := opts.Provider.Generate(repairAnalysis, work.input)
		if repairErr != nil {
			return ValidateResult{}, fmt.Errorf("%w; format repair: %v", err, repairErr)
		}
		blocks, err = ParseBlocks(generation)
		if err == nil {
			blocks = canonicalizeGeneratedPaths(blocks)
			sanitizeGeneratedAliasCollisions(opts.ProjectPath, &blocks)
			filterUnplannedNewPages(opts, &blocks)
			downgradeMissingReviewWikilinks(opts.ProjectPath, &blocks)
			if conflicts := newlyAppearedUnsuppliedPaths(opts, work.pageSnapshot, blocks); len(conflicts) > 0 {
				return ValidateResult{}, sourceConflictError{Paths: conflicts, Reason: "an unsupplied wiki page appeared after the evidence snapshot"}
			}
			filterUnsuppliedExistingPages(opts, work.pageSnapshot, &blocks)
			err = validateGeneratedBlocksForPolicy(opts, blocks, work.rawRel, work.input.GenerationPolicy)
		}
		if err != nil && downgradeMissingLinksIfValid(opts, &blocks, work.rawRel, work.input.GenerationPolicy) {
			err = nil
		}
		if err != nil {
			if countSourceSummaryBlocks(blocks) == 0 {
				emit("generation_summary_repair", work.title, ValidateResult{}, err)
				summaryAnalysis := work.analysis + `

SOURCE SUMMARY RECOVERY REQUIRED.
Return exactly one ---FILE block and nothing else.
It must be a complete LLM-written source summary for the current source under wiki/sources/ with type: source-summary, a valid title, current raw source provenance, and a concise evidence-grounded body.
Do not return entity, concept, synthesis, or review blocks. Do not use unresolved wikilinks.`
				summaryGeneration, summaryErr := opts.Provider.Generate(summaryAnalysis, work.input)
				if summaryErr == nil {
					var summaryBlocks ParsedBlocks
					summaryBlocks, summaryErr = ParseBlocks(summaryGeneration)
					if summaryErr == nil {
						summaryBlocks = canonicalizeGeneratedPaths(summaryBlocks)
						summaryErr = validateRecoveredSourceSummary(opts.ProjectPath, summaryBlocks, work.rawRel)
					}
					if summaryErr == nil {
						blocks.Files = append([]FileBlock{summaryBlocks.Files[0]}, blocks.Files...)
						sanitizeGeneratedAliasCollisions(opts.ProjectPath, &blocks)
						err = validateGeneratedBlocksForPolicy(opts, blocks, work.rawRel, work.input.GenerationPolicy)
					}
				}
				if summaryErr != nil {
					err = fmt.Errorf("%w; source-summary recovery: %v", err, summaryErr)
				}
			}
			if err != nil && downgradeMissingLinksIfValid(opts, &blocks, work.rawRel, work.input.GenerationPolicy) {
				err = nil
			}
			if err != nil {
				emit("generation_contract_repair", work.title, ValidateResult{}, err)
				contractAnalysis := work.analysis + "\n\nGENERATION CONTRACT RECOVERY REQUIRED: " + err.Error() + "\n" + generationPolicyText(work.input.GenerationPolicy) + fmt.Sprintf(`
Regenerate the complete output from scratch and obey these absolute rules:
- Return no more than %d total ---FILE blocks.
- The first block must be exactly one source-summary for the current source.
- Return no more than %d CREATE NEW non-summary pages.
- Only paths whose complete contents are supplied under Existing pages may be treated as updates.
- If a title or alias collides with a supplied canonical page, update that exact canonical path and omit the competing page.
- Use plain text instead of a wikilink whenever you are not returning or updating its durable target page.
- Return only ---FILE and optional ---REVIEW blocks.`, work.input.GenerationPolicy.MaxFileBlocks, work.input.GenerationPolicy.RemainingNewPages)
				contractGeneration, contractErr := opts.Provider.Generate(contractAnalysis, work.input)
				if contractErr != nil {
					err = fmt.Errorf("%w; generation contract recovery: %v", err, contractErr)
				} else {
					blocks, err = ParseBlocks(contractGeneration)
					if err == nil {
						blocks = canonicalizeGeneratedPaths(blocks)
						sanitizeGeneratedAliasCollisions(opts.ProjectPath, &blocks)
						filterUnplannedNewPages(opts, &blocks)
						downgradeMissingReviewWikilinks(opts.ProjectPath, &blocks)
						if conflicts := newlyAppearedUnsuppliedPaths(opts, work.pageSnapshot, blocks); len(conflicts) > 0 {
							return ValidateResult{}, sourceConflictError{Paths: conflicts, Reason: "an unsupplied wiki page appeared after the evidence snapshot"}
						}
						filterUnsuppliedExistingPages(opts, work.pageSnapshot, &blocks)
						err = validateGeneratedBlocksForPolicy(opts, blocks, work.rawRel, work.input.GenerationPolicy)
					}
					if err != nil && downgradeMissingLinksIfValid(opts, &blocks, work.rawRel, work.input.GenerationPolicy) {
						err = nil
					}
				}
			}
			if err != nil {
				return ValidateResult{}, fmt.Errorf("generation validation repair failed: %w", err)
			}
		}
	}
	// Avoid spending a separate durability-gate LLM call on a task whose
	// evidence already became stale while generation or repair was running.
	wikiPersistMu.Lock()
	preGateConflicts := staleGeneratedPaths(opts.ProjectPath, work.pageSnapshot, work.dependencies, blocks.Files)
	wikiPersistMu.Unlock()
	if len(preGateConflicts) > 0 {
		return ValidateResult{}, sourceConflictError{Paths: preGateConflicts, Reason: "wiki evidence changed before the new-page durability gate"}
	}
	blocks, err = applyNewPageGate(opts, work, blocks)
	if err != nil {
		emit("page_gate_failed", work.title, ValidateResult{}, err)
		return ValidateResult{}, err
	}
	wikiPersistMu.Lock()
	defer wikiPersistMu.Unlock()
	if conflicts := staleGeneratedPaths(opts.ProjectPath, work.pageSnapshot, work.dependencies, blocks.Files); len(conflicts) > 0 {
		return ValidateResult{}, sourceConflictError{Paths: conflicts}
	}
	if err := mergePreservedPageSources(opts.ProjectPath, work.rawRel, &blocks); err != nil {
		return ValidateResult{}, sourceConflictError{Reason: err.Error()}
	}
	if err := validateGeneratedBlocksForPolicy(opts, blocks, work.rawRel, work.input.GenerationPolicy); err != nil {
		return ValidateResult{}, sourceConflictError{Reason: err.Error()}
	}
	if err := validatePreservedPageSources(opts.ProjectPath, blocks.Files); err != nil {
		return ValidateResult{}, sourceConflictError{Reason: err.Error()}
	}
	manifest, err := loadSourceManifest(opts.ProjectPath)
	if err != nil {
		return ValidateResult{}, err
	}
	if opts.Manifest != nil {
		*opts.Manifest = manifest
	}
	intentPath := ""
	if opts.ManagedTask {
		intentPath, err = writeBootstrapCommitIntent(opts.ProjectPath, opts.SourcePath, blocks.Files)
		if err != nil {
			return ValidateResult{}, err
		}
	}
	transactionPaths := []string{
		"wiki/index.md", "wiki/log.md", "wiki/reviews.md",
		filepath.ToSlash(filepath.Join(".kbcore", "source-manifest.json")),
	}
	for _, file := range blocks.Files {
		transactionPaths = append(transactionPaths, file.Path)
	}
	tx, err := wiki.BeginFileTransaction(opts.ProjectPath, work.manifestKey+":"+work.hash, transactionPaths)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("begin source transaction: %w", err)
	}
	transactionCommitted := false
	defer func() {
		if !transactionCommitted {
			_ = tx.Rollback()
		}
	}()
	var written []string
	var changes []PageChange
	var createdPages []string
	for _, file := range blocks.Files {
		if sourceCommitTestHook != nil {
			if err := sourceCommitTestHook("page", file.Path); err != nil {
				return ValidateResult{}, err
			}
		}
		abs := filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))
		_, statErr := os.Stat(abs)
		before := readOptional(abs)
		if err := wiki.WriteVersionedPage(opts.ProjectPath, file.Path, []byte(file.Content), "validate-llmwiki: "+work.title); err != nil {
			return ValidateResult{}, err
		}
		written = append(written, file.Path)
		if os.IsNotExist(statErr) {
			frontmatter, parseErr := parseGeneratedFrontmatter(file.Content)
			if parseErr == nil && strings.TrimSpace(fmt.Sprint(frontmatter["type"])) != "source-summary" {
				createdPages = append(createdPages, file.Path)
			}
		}
		if before != file.Content {
			changes = append(changes, PageChange{
				Path: file.Path, BeforeSHA256: textSHA256(before), AfterSHA256: textSHA256(file.Content),
				BeforeExcerpt: before, AfterExcerpt: file.Content,
			})
		}
	}
	if err := resolveSatisfiedPageReviews(opts.ProjectPath, blocks.Files); err != nil {
		return ValidateResult{}, err
	}
	if sourceCommitTestHook != nil {
		if err := sourceCommitTestHook("aggregates", "wiki/index.md"); err != nil {
			return ValidateResult{}, err
		}
	}
	if err := updateAggregates(opts.ProjectPath, work.title, work.rawRel, written, blocks.Reviews); err != nil {
		return ValidateResult{}, err
	}
	previous := manifest.Sources[work.manifestKey]
	newPageCount := len(createdPages)
	allCreatedPages := append([]string(nil), createdPages...)
	allFiles := append([]string(nil), written...)
	if previous.SHA256 == work.hash && previous.GenerationContractSHA256 == generationContractSHA256(work.input.Purpose, work.input.Schema, opts.MaxFilesPerTask, opts.MaxNewPagesPerSource) {
		newPageCount += previous.NewPageCount
		allCreatedPages = sortedUniqueStrings(append(previous.CreatedPages, createdPages...))
		allFiles = sortedUniqueStrings(append(previous.Files, written...))
	}
	entry := SourceManifestEntry{
		OriginalPath:             sourceManifestKey(opts.SourcePath),
		PipelineVersion:          core.SourceManifestPipelineVersion,
		SHA256:                   work.hash,
		RawPath:                  work.rawRel,
		Title:                    work.title,
		Files:                    allFiles,
		GenerationContractSHA256: generationContractSHA256(work.input.Purpose, work.input.Schema, opts.MaxFilesPerTask, opts.MaxNewPagesPerSource),
		NewPageBudget:            work.input.GenerationPolicy.MaxNewPagesPerSource,
		NewPageCount:             newPageCount,
		CreatedPages:             allCreatedPages,
		ReviewCount:              len(blocks.Reviews),
		UpdatedAt:                time.Now().Format(time.RFC3339),
		Extraction: sourceExtractionJSON(SourceExtractionMetadata{
			SourceExt:   work.extracted.SourceExt,
			Extractor:   work.extracted.Extractor,
			ExtractedAt: time.Now().UTC().Format(time.RFC3339),
		}),
	}
	manifestfile.AppendPriorVersion(previous, &entry)
	if work.archive != nil {
		entry.OriginalPath = sourceManifestKey(opts.SourcePath)
		entry.ArchivePath = work.archive.ArchivePath
		entry.OriginalRawPath = work.archive.OriginalRawPath
		entry.ContentPath = work.archive.ContentPath
		entry.OriginalSHA256 = work.archive.OriginalSHA256
		entry.ContentSHA256 = work.archive.ContentSHA256
	}
	manifest.Sources[work.manifestKey] = entry
	for _, path := range written {
		manifestfile.RegisterPageOwner(&manifest, path, "source", work.manifestKey)
	}
	if sourceCommitTestHook != nil {
		if err := sourceCommitTestHook("manifest", sourceManifestPath(opts.ProjectPath)); err != nil {
			return ValidateResult{}, err
		}
	}
	if err := saveSourceManifest(opts.ProjectPath, manifest); err != nil {
		return ValidateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ValidateResult{}, fmt.Errorf("commit source transaction: %w", err)
	}
	transactionCommitted = true
	if opts.Manifest != nil {
		*opts.Manifest = manifest
	}
	result := ValidateResult{
		RawPath:      work.rawRel,
		Analysis:     work.analysis,
		Files:        written,
		ReviewCount:  len(blocks.Reviews),
		SHA256:       work.hash,
		Dependencies: append([]string(nil), work.dependencies...),
		PageChanges:  changes,
		CommitIntent: intentPath,
	}
	// Markdown and its manifest are now durable. PostgreSQL remains a derived
	// index and is synchronized after the commit lock is released.
	wikiPersistMu.Unlock()
	if opts.OnCommitted != nil {
		if err := opts.OnCommitted(result); err != nil {
			wikiPersistMu.Lock()
			return result, postCommitSyncError{Result: result, Err: err}
		}
	}
	wikiPersistMu.Lock()
	emit("completed", work.title, result, nil)
	return result, nil
}

func pathsAbsentFromSnapshot(snapshot map[string]string, candidates []string) []string {
	missing := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if _, ok := snapshot[path]; !ok {
			missing = append(missing, path)
		}
	}
	return sortedUniqueStrings(missing)
}

func resolveSatisfiedPageReviews(projectPath string, files []FileBlock) error {
	identities := map[string]bool{}
	for _, file := range files {
		fm, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(fm["type"])) == "source-summary" {
			continue
		}
		for _, value := range append([]string{strings.TrimSpace(fmt.Sprint(fm["title"]))}, frontmatterStrings(fm["aliases"])...) {
			if key := normalizeIdentityName(value); len([]rune(key)) >= 2 {
				identities[key] = true
			}
		}
	}
	if len(identities) == 0 {
		return nil
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: "compiler"})
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Status != "open" || (item.Type != "missing-page" && item.Type != "review-needed") {
			continue
		}
		text := strings.ToLower(item.Title + " " + item.Description)
		durability := item.Type == "missing-page" || strings.Contains(text, "durab") || strings.Contains(text, "standalone page") || strings.Contains(text, "建页") || strings.Contains(text, "建立") || strings.Contains(text, "独立页")
		if !durability {
			continue
		}
		matched := false
		reviewTitle := normalizeIdentityName(item.Title)
		for identity := range identities {
			// Match the requested page named by the review title. Evidence text
			// often mentions several existing pages and must not resolve a
			// missing-page task merely because one of those pages was updated.
			if strings.Contains(reviewTitle, identity) {
				matched = true
				break
			}
		}
		if matched {
			if _, err := wiki.UpdateReviewItemStatusWithAction(projectPath, "compiler", item.ID, "resolved", "page-gate-accepted", time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func downgradeMissingLinksIfValid(opts ValidateOptions, blocks *ParsedBlocks, sourceRel string, policy GenerationPolicy) bool {
	missing := downgradeMissingWikilinks(opts.ProjectPath, blocks)
	if len(missing) == 0 || validateGeneratedBlocksForPolicy(opts, *blocks, sourceRel, policy) != nil {
		return false
	}
	blocks.Reviews = append(blocks.Reviews, ReviewBlock{
		Type:  "missing-page",
		Title: "Generated wikilinks require durable target pages",
		Body:  "Unresolved targets were rendered as plain text to keep structural lint clean. Create durable pages or link them to known aliases when evidence supports doing so:\n- " + strings.Join(missing, "\n- "),
	})
	return true
}

type sourceConflictError struct {
	Paths  []string
	Reason string
}

func (e sourceConflictError) Error() string {
	if len(e.Paths) > 0 {
		return "wiki evidence changed while source task was running: " + strings.Join(e.Paths, ", ")
	}
	if strings.TrimSpace(e.Reason) != "" {
		return "wiki task commit rejected: " + e.Reason
	}
	return "wiki source task conflicted"
}

type postCommitSyncError struct {
	Result ValidateResult
	Err    error
}

func (e postCommitSyncError) Error() string { return "post-commit sync: " + e.Err.Error() }
func (e postCommitSyncError) Unwrap() error { return e.Err }

func snapshotWikiPageHashes(projectPath string) map[string]string {
	out := map[string]string{}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return out
	}
	for _, page := range pages {
		content := readOptional(filepath.Join(projectPath, filepath.FromSlash(page.Path)))
		out[page.Path] = textSHA256(content)
	}
	return out
}

func staleGeneratedPaths(projectPath string, snapshot map[string]string, dependencies []string, files []FileBlock) []string {
	paths := map[string]bool{}
	for _, path := range dependencies {
		paths[filepath.ToSlash(path)] = true
	}
	for _, file := range files {
		paths[file.Path] = true
	}
	var stale []string
	for path := range paths {
		before, existedBefore := snapshot[path]
		data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(path)))
		existsNow := err == nil
		if err != nil && !os.IsNotExist(err) {
			stale = append(stale, path)
			continue
		}
		if existedBefore != existsNow || existsNow && before != textSHA256(string(data)) {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	return stale
}

func validatePreservedPageSources(projectPath string, files []FileBlock) error {
	allowed, err := manifestRawSourceAllowlist(projectPath)
	if err != nil {
		return err
	}
	for _, file := range files {
		current := readOptional(filepath.Join(projectPath, filepath.FromSlash(file.Path)))
		if strings.TrimSpace(current) == "" {
			continue
		}
		currentFM, err := parseGeneratedFrontmatter(current)
		if err != nil {
			continue
		}
		candidateFM, err := parseGeneratedFrontmatter(file.Content)
		if err != nil {
			return err
		}
		// A source-summary represents the current version of one logical source.
		// Its previous bytes remain in page-versions and the manifest version
		// history, but its live provenance must point only at the current raw
		// version. Shared cumulative pages still preserve every prior source.
		if strings.TrimSpace(fmt.Sprint(candidateFM["type"])) == "source-summary" {
			continue
		}
		candidateSources := map[string]bool{}
		for _, source := range frontmatterStrings(candidateFM["sources"]) {
			candidateSources[filepath.ToSlash(strings.TrimSpace(source))] = true
		}
		for _, source := range frontmatterStrings(currentFM["sources"]) {
			source = filepath.ToSlash(strings.TrimSpace(source))
			if allowed[source] && !candidateSources[source] {
				return fmt.Errorf("file block %s dropped existing source %s", file.Path, source)
			}
		}
	}
	return nil
}

func countSourceSummaryBlocks(blocks ParsedBlocks) int {
	count := 0
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err == nil && strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			count++
		}
	}
	return count
}

func validateRecoveredSourceSummary(projectPath string, blocks ParsedBlocks, sourceRel string) error {
	if len(blocks.Files) != 1 || len(blocks.Reviews) != 0 || countSourceSummaryBlocks(blocks) != 1 {
		return fmt.Errorf("source-summary recovery must return exactly one source-summary file and no reviews")
	}
	return ValidateGeneratedBlocks(projectPath, blocks, sourceRel)
}

// mergePreservedPageSources enforces the durable provenance invariant at the
// commit boundary. The LLM still merges page content, while the compiler makes
// it impossible for an overwrite to silently drop an earlier raw source.
func mergePreservedPageSources(projectPath, currentSource string, blocks *ParsedBlocks) error {
	currentSource = filepath.ToSlash(strings.TrimSpace(currentSource))
	currentAbs := filepath.Join(projectPath, filepath.FromSlash(currentSource))
	if !strings.HasPrefix(currentSource, "raw/sources/") {
		return fmt.Errorf("current source must be under raw/sources/: %s", currentSource)
	}
	if _, err := os.Stat(currentAbs); err != nil {
		return fmt.Errorf("current source does not exist: %s", currentSource)
	}
	allowed, err := manifestRawSourceAllowlist(projectPath)
	if err != nil {
		return err
	}
	allowed[currentSource] = true
	for index := range blocks.Files {
		file := &blocks.Files[index]
		current := readOptional(filepath.Join(projectPath, filepath.FromSlash(file.Path)))
		candidateFM, body, err := splitGeneratedFrontmatter(file.Content)
		if err != nil {
			return fmt.Errorf("file block %s: %w", file.Path, err)
		}
		seen := map[string]bool{currentSource: true}
		sources := []string{currentSource}
		pageType := strings.TrimSpace(fmt.Sprint(candidateFM["type"]))
		if pageType != "source-summary" && strings.TrimSpace(current) != "" {
			currentFM, currentErr := parseGeneratedFrontmatter(current)
			if currentErr == nil {
				for _, source := range frontmatterStrings(currentFM["sources"]) {
					source = filepath.ToSlash(strings.TrimSpace(source))
					if source == "" || seen[source] || !allowed[source] {
						continue
					}
					seen[source] = true
					sources = append(sources, source)
				}
			}
		}
		sort.Strings(sources)
		candidateFM["sources"] = sources
		title := strings.TrimSpace(fmt.Sprint(candidateFM["title"]))
		aliasSet := map[string]string{}
		for _, alias := range frontmatterStrings(candidateFM["aliases"]) {
			alias = strings.TrimSpace(alias)
			key := normalizeIdentityName(alias)
			if key == "" || key == normalizeIdentityName(title) {
				continue
			}
			aliasSet[key] = alias
		}
		aliases := make([]string, 0, len(aliasSet))
		for _, alias := range aliasSet {
			aliases = append(aliases, alias)
		}
		sort.Slice(aliases, func(i, j int) bool { return strings.ToLower(aliases[i]) < strings.ToLower(aliases[j]) })
		candidateFM["aliases"] = aliases
		frontmatterYAML, err := yaml.Marshal(candidateFM)
		if err != nil {
			return fmt.Errorf("marshal frontmatter for %s: %w", file.Path, err)
		}
		file.Content = "---\n" + strings.TrimSpace(string(frontmatterYAML)) + "\n---\n\n" + strings.TrimSpace(body) + "\n"
	}
	return nil
}

func manifestRawSourceAllowlist(projectPath string) (map[string]bool, error) {
	manifest, err := loadSourceManifest(projectPath)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(manifest.Sources))
	for _, entry := range manifest.Sources {
		source := filepath.ToSlash(strings.TrimSpace(entry.RawPath))
		if source != "" && validExistingRawSource(projectPath, source) {
			allowed[source] = true
		}
		for _, version := range entry.Versions {
			source := filepath.ToSlash(strings.TrimSpace(version.RawPath))
			if source != "" && validExistingRawSource(projectPath, source) {
				allowed[source] = true
			}
		}
	}
	return allowed, nil
}

func validExistingRawSource(projectPath, source string) bool {
	if !strings.HasPrefix(source, "raw/sources/") {
		return false
	}
	abs, err := filepath.Abs(filepath.Join(projectPath, filepath.FromSlash(source)))
	if err != nil {
		return false
	}
	rawRoot, err := filepath.Abs(filepath.Join(projectPath, "raw", "sources"))
	if err != nil || abs == rawRoot || !strings.HasPrefix(abs, rawRoot+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

func validateGeneratedBlocksForTask(opts ValidateOptions, blocks ParsedBlocks, sourceRel string) error {
	maxFiles, maxNewPages := normalizedGenerationLimits(opts.MaxFilesPerTask, opts.MaxNewPagesPerSource)
	return validateGeneratedBlocksForPolicy(opts, blocks, sourceRel, GenerationPolicy{MaxFileBlocks: maxFiles, MaxNewPagesPerSource: maxNewPages, RemainingNewPages: maxNewPages})
}

func validateGeneratedBlocksForPolicy(opts ValidateOptions, blocks ParsedBlocks, sourceRel string, policy GenerationPolicy) error {
	maxFiles := policy.MaxFileBlocks
	if len(blocks.Files) > maxFiles {
		return fmt.Errorf("generation produced %d file blocks; maximum per source task is %d", len(blocks.Files), maxFiles)
	}
	maxNewPages := policy.RemainingNewPages
	newPages := 0
	plannedNew, structuredPlan := plannedNewPagePaths(opts.AnalysisPlan)
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))); os.IsNotExist(err) {
			newPages++
			if structuredPlan && !plannedNew[file.Path] {
				return fmt.Errorf("new page %s was not approved by the analysis Wiki Plan", file.Path)
			}
		}
	}
	if newPages > maxNewPages {
		return fmt.Errorf("generation created %d new non-summary pages; maximum per source is %d", newPages, maxNewPages)
	}
	allowedUpdates := map[string]bool{}
	for _, path := range opts.AllowedUpdatePaths {
		allowedUpdates[filepath.ToSlash(strings.TrimSpace(path))] = true
	}
	for _, file := range blocks.Files {
		if _, err := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))); err == nil && !allowedUpdates[file.Path] {
			return fmt.Errorf("existing page %s was not supplied to this source task and may not be overwritten", file.Path)
		}
	}
	if conflicts := canonicalIdentityConflictPaths(opts.ProjectPath, blocks); len(conflicts) > 0 {
		return fmt.Errorf("generated page title or alias collides with canonical page(s): %s", strings.Join(conflicts, ", "))
	}
	return ValidateGeneratedBlocks(opts.ProjectPath, blocks, sourceRel)
}

func plannedNewPagePaths(analysis string) (map[string]bool, bool) {
	out := map[string]bool{}
	structured := false
	for _, line := range strings.Split(analysis, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		label := strings.TrimSpace(strings.TrimPrefix(parts[0], "-"))
		path := filepath.ToSlash(strings.TrimSpace(parts[1]))
		if !strings.HasPrefix(path, "wiki/") || !strings.HasSuffix(path, ".md") {
			continue
		}
		structured = true
		if label == "CREATE NEW" {
			out[path] = true
		}
	}
	return out, structured
}

// filterUnplannedNewPages enforces the analysis contract without spending
// another LLM call on a page the model was never authorized to create. The
// source summary and approved updates remain useful; links to discarded pages
// are downgraded and the decision stays visible in the durable review log.
func filterUnplannedNewPages(opts ValidateOptions, blocks *ParsedBlocks) {
	planned, structured := plannedNewPagePaths(opts.AnalysisPlan)
	if !structured || blocks == nil {
		return
	}
	kept := blocks.Files[:0]
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			kept = append(kept, file)
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))); err == nil || !os.IsNotExist(err) || planned[file.Path] {
			kept = append(kept, file)
			continue
		}
		blocks.Reviews = append(blocks.Reviews, ReviewBlock{
			Type:  "review-needed",
			Title: "Analysis plan rejected " + file.Path,
			Body:  "The generation proposed this new page even though the structured analysis plan did not authorize it. The page was discarded and any links to it were rendered as plain text. Reconsider it only when a later source analysis explicitly plans the durable page.",
		})
	}
	blocks.Files = kept
	downgradeMissingWikilinks(opts.ProjectPath, blocks)
}

func newlyAppearedUnsuppliedPaths(opts ValidateOptions, snapshot map[string]string, blocks ParsedBlocks) []string {
	allowed := map[string]bool{}
	for _, path := range opts.AllowedUpdatePaths {
		allowed[filepath.ToSlash(strings.TrimSpace(path))] = true
	}
	var conflicts []string
	for _, file := range blocks.Files {
		if allowed[file.Path] {
			continue
		}
		if _, existedBefore := snapshot[file.Path]; existedBefore {
			continue
		}
		if info, err := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))); err == nil && !info.IsDir() {
			conflicts = append(conflicts, file.Path)
		}
	}
	return sortedUniqueStrings(conflicts)
}

// filterUnsuppliedExistingPages discards an attempted overwrite when the page
// existed in the task snapshot but its complete contents were not supplied to
// the generation call. The model has no safe evidence to merge in that case.
// A page that appeared after the snapshot is handled first as an optimistic
// conflict by newlyAppearedUnsuppliedPaths and must never be silently dropped.
func filterUnsuppliedExistingPages(opts ValidateOptions, snapshot map[string]string, blocks *ParsedBlocks) {
	if blocks == nil {
		return
	}
	allowed := map[string]bool{}
	for _, path := range opts.AllowedUpdatePaths {
		allowed[filepath.ToSlash(strings.TrimSpace(path))] = true
	}
	kept := blocks.Files[:0]
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" || allowed[file.Path] {
			kept = append(kept, file)
			continue
		}
		if _, existedBefore := snapshot[file.Path]; !existedBefore {
			kept = append(kept, file)
			continue
		}
		blocks.Reviews = append(blocks.Reviews, ReviewBlock{
			Type:  "review-needed",
			Title: "Unsupplied existing page discarded " + file.Path,
			Body:  "The generation attempted to overwrite an existing page whose complete contents were not supplied to this source task. The unsafe update was discarded. A later task may update the page only after reading the canonical page as evidence.",
		})
	}
	blocks.Files = kept
}

func sourceSummaryUpdatePaths(projectPath, manifestKey string) []string {
	value, err := loadSourceManifest(projectPath)
	if err != nil {
		return nil
	}
	entry, ok := value.Sources[manifestKey]
	if !ok {
		return nil
	}
	var paths []string
	for _, path := range manifestfile.OwnedFiles(entry) {
		content, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		frontmatter, err := parseGeneratedFrontmatter(string(content))
		if err == nil && strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	return sortedUniqueStrings(paths)
}

// sanitizeGeneratedAliasCollisions removes only aliases whose normalized
// identity is already owned by another canonical page. A colliding title is
// never rewritten here: it remains a validation error that requires an LLM
// merge into the correct page.
func sanitizeGeneratedAliasCollisions(projectPath string, blocks *ParsedBlocks) {
	existingOwners := map[string]map[string]bool{}
	pages, _ := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	for _, page := range pages {
		if page.Type == "source-summary" || isProtectedWikiPath(page.Path) {
			continue
		}
		values := append([]string{page.Title}, frontmatterStrings(page.Frontmatter["aliases"])...)
		for _, value := range values {
			key := normalizeIdentityName(value)
			if key == "" {
				continue
			}
			if existingOwners[key] == nil {
				existingOwners[key] = map[string]bool{}
			}
			existingOwners[key][page.Path] = true
		}
	}

	batchTitles := map[string]map[string]bool{}
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			continue
		}
		key := normalizeIdentityName(strings.TrimSpace(fmt.Sprint(frontmatter["title"])))
		if key == "" {
			continue
		}
		if batchTitles[key] == nil {
			batchTitles[key] = map[string]bool{}
		}
		batchTitles[key][file.Path] = true
	}

	acceptedAliases := map[string]string{}
	var removed []string
	for index := range blocks.Files {
		file := &blocks.Files[index]
		frontmatter, body, err := splitGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			continue
		}
		titleKey := normalizeIdentityName(strings.TrimSpace(fmt.Sprint(frontmatter["title"])))
		kept := make([]string, 0)
		for _, alias := range frontmatterStrings(frontmatter["aliases"]) {
			alias = strings.TrimSpace(alias)
			key := normalizeIdentityName(alias)
			if key == "" || key == titleKey {
				continue
			}
			conflictingPaths := map[string]bool{}
			for path := range existingOwners[key] {
				if path != file.Path {
					conflictingPaths[path] = true
				}
			}
			for path := range batchTitles[key] {
				if path != file.Path {
					conflictingPaths[path] = true
				}
			}
			if owner := acceptedAliases[key]; owner != "" && owner != file.Path {
				conflictingPaths[owner] = true
			}
			if len(conflictingPaths) > 0 {
				owners := make([]string, 0, len(conflictingPaths))
				for path := range conflictingPaths {
					owners = append(owners, path)
				}
				sort.Strings(owners)
				removed = append(removed, fmt.Sprintf("%s: alias %q conflicts with %s", file.Path, alias, strings.Join(owners, ", ")))
				continue
			}
			acceptedAliases[key] = file.Path
			kept = append(kept, alias)
		}
		frontmatter["aliases"] = sortedUniqueStrings(kept)
		frontmatterYAML, marshalErr := yaml.Marshal(frontmatter)
		if marshalErr != nil {
			continue
		}
		file.Content = "---\n" + strings.TrimSpace(string(frontmatterYAML)) + "\n---\n\n" + strings.TrimSpace(body) + "\n"
	}
	if len(removed) == 0 {
		return
	}
	sort.Strings(removed)
	blocks.Reviews = append(blocks.Reviews, ReviewBlock{
		Type:  "duplicate",
		Title: "Conflicting generated aliases were removed",
		Body:  "Aliases that belonged to another canonical page were removed before persistence:\n- " + strings.Join(removed, "\n- "),
	})
}

func canonicalIdentityConflictPaths(projectPath string, blocks ParsedBlocks) []string {
	owners := map[string]map[string]bool{}
	pages, _ := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	for _, page := range pages {
		if page.Type == "source-summary" || isProtectedWikiPath(page.Path) {
			continue
		}
		values := append([]string{page.Title}, frontmatterStrings(page.Frontmatter["aliases"])...)
		for _, value := range values {
			key := normalizeIdentityName(value)
			if key == "" {
				continue
			}
			if owners[key] == nil {
				owners[key] = map[string]bool{}
			}
			owners[key][page.Path] = true
		}
	}
	conflictSet := map[string]bool{}
	batchOwners := map[string]string{}
	for _, file := range blocks.Files {
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
			continue
		}
		values := append([]string{strings.TrimSpace(fmt.Sprint(frontmatter["title"]))}, frontmatterStrings(frontmatter["aliases"])...)
		for _, value := range values {
			key := normalizeIdentityName(value)
			if key == "" {
				continue
			}
			for owner := range owners[key] {
				if owner != file.Path {
					conflictSet[owner] = true
				}
			}
			if owner, ok := batchOwners[key]; ok && owner != file.Path {
				conflictSet[owner] = true
				conflictSet[file.Path] = true
			} else {
				batchOwners[key] = file.Path
			}
		}
	}
	conflicts := make([]string, 0, len(conflictSet))
	for path := range conflictSet {
		conflicts = append(conflicts, path)
	}
	sort.Strings(conflicts)
	return conflicts
}

func appendCanonicalConflictPages(projectPath, existing string, blocks ParsedBlocks) string {
	seen := map[string]bool{}
	for _, path := range canonicalIdentityConflictPaths(projectPath, blocks) {
		if seen[path] {
			continue
		}
		seen[path] = true
		content := readOptional(filepath.Join(projectPath, filepath.FromSlash(path)))
		if strings.TrimSpace(content) != "" {
			existing += "\n---EXISTING CANONICAL PAGE: " + path + "\n" + content + "\n"
		}
	}
	return promptbudget.TrimMiddle(existing, 48000)
}

func splitGeneratedFrontmatter(content string) (map[string]any, string, error) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "---\n") {
		return nil, "", fmt.Errorf("missing YAML frontmatter")
	}
	rest := strings.TrimPrefix(trimmed, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("unterminated YAML frontmatter")
	}
	frontmatter := map[string]any{}
	if err := yaml.Unmarshal([]byte(rest[:end]), &frontmatter); err != nil {
		return nil, "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	return frontmatter, rest[end+len("\n---\n"):], nil
}

func textSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func ValidateLLMWikiPath(opts ValidateOptions) (BatchValidateResult, error) {
	lock, err := projectlock.Acquire(opts.ProjectPath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	defer lock.Release()
	if _, err := wiki.RecoverFileTransactions(opts.ProjectPath); err != nil {
		return BatchValidateResult{}, fmt.Errorf("recover source transactions: %w", err)
	}
	opts.ProjectLockHeld = true
	info, err := os.Stat(opts.SourcePath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	if !info.IsDir() {
		return validateLLMWikiSourcesConcurrent(opts, []string{opts.SourcePath})
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
	return validateLLMWikiSourcesConcurrent(opts, sources)
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

type SourceManifest = manifestfile.File
type SourceManifestEntry = manifestfile.Entry

type SourceExtractionMetadata struct {
	SourceExt   string `json:"source_ext"`
	Extractor   string `json:"extractor"`
	ExtractedAt string `json:"extracted_at"`
}

func sourceExtractionJSON(value SourceExtractionMetadata) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func loadSourceManifest(projectPath string) (SourceManifest, error) {
	return manifestfile.Load(projectPath)
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
			ID:                       core.StableID(projectID, "source-manifest", originalPath),
			ProjectID:                projectID,
			OriginalPath:             originalPath,
			PipelineVersion:          entry.PipelineVersion,
			SHA256:                   entry.SHA256,
			RawPath:                  entry.RawPath,
			ArchivePath:              entry.ArchivePath,
			OriginalRawPath:          entry.OriginalRawPath,
			ContentPath:              entry.ContentPath,
			OriginalSHA256:           entry.OriginalSHA256,
			ContentSHA256:            entry.ContentSHA256,
			Title:                    entry.Title,
			Files:                    append([]string(nil), entry.Files...),
			GenerationContractSHA256: entry.GenerationContractSHA256,
			NewPageBudget:            entry.NewPageBudget,
			NewPageCount:             entry.NewPageCount,
			CreatedPages:             append([]string(nil), entry.CreatedPages...),
			ReviewCount:              entry.ReviewCount,
			UpdatedAt:                updatedAt,
		})
	}
	return entries, nil
}

func saveSourceManifest(projectPath string, manifest SourceManifest) error {
	return manifestfile.Save(projectPath, manifest)
}

func sourceManifestPath(projectPath string) string {
	return manifestfile.Path(projectPath)
}

func sourceManifestKey(path string) string {
	return manifestfile.Key(path)
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

func existingPagesForAnalysis(projectPath, analysis string) (string, []string) {
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
		if validateWikiFilePath(candidate) == nil && eligibleExistingMergePath(candidate) {
			paths[filepath.ToSlash(candidate)] = true
		}
		rest = rest[end+3:]
	}
	if pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath}); err == nil {
		for _, page := range pages {
			if eligibleExistingMergePath(page.Path) && page.Type != "source-summary" && page.Title != "" && len([]rune(page.Title)) >= 2 && strings.Contains(analysis, page.Title) {
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
	var included []string
	for _, path := range ordered {
		content := readOptional(filepath.Join(projectPath, filepath.FromSlash(path)))
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n---EXISTING PAGE: %s\n%s\n", path, content)
		included = append(included, path)
		if b.Len() >= 48000 {
			break
		}
	}
	return b.String(), included
}

func eligibleExistingMergePath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	return !isProtectedWikiPath(path) && !strings.HasPrefix(path, "wiki/sources/")
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
	review.Type = canonicalReviewType(review.Type)
	if current, err := os.ReadFile(path); err == nil {
		identity := normalizeIdentityName(review.Title)
		for _, item := range wiki.ParseReviewItems("local", string(current)) {
			if canonicalReviewType(item.Type) == review.Type && normalizeIdentityName(item.Title) == identity && item.Status == "open" {
				return mergeOpenReviewEntry(projectPath, path, string(current), item, sourceRel, files, review.Body)
			}
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

func mergeOpenReviewEntry(projectPath, path, content string, item core.ReviewItem, sourceRel string, files []string, detail string) error {
	needle := " | " + item.Title
	headingAt := strings.Index(content, needle)
	if headingAt < 0 {
		return nil
	}
	start := strings.LastIndex(content[:headingAt], "\n## [")
	if start < 0 {
		start = 0
	} else {
		start++
	}
	end := strings.Index(content[headingAt:], "\n## [")
	if end < 0 {
		end = len(content)
	} else {
		end += headingAt + 1
	}
	section := content[start:end]
	changed := false
	if sourceRel != "" && !strings.Contains(section, "`"+sourceRel+"`") {
		insertAt := strings.Index(section, "- Status:")
		if insertAt >= 0 {
			section = section[:insertAt] + "- Source: `" + sourceRel + "`\n" + section[insertAt:]
			changed = true
		}
	}
	var additions strings.Builder
	for _, file := range files {
		if !strings.Contains(section, "`"+file+"`") {
			fmt.Fprintf(&additions, "- `%s`\n", file)
		}
	}
	if additions.Len() > 0 {
		insertAt := strings.Index(section, "### Detail")
		if insertAt >= 0 {
			section = section[:insertAt] + additions.String() + section[insertAt:]
			changed = true
		}
	}
	detail = strings.TrimSpace(detail)
	if detail != "" && !strings.Contains(section, detail) {
		section += "\nAdditional evidence from `" + sourceRel + "`:\n\n" + detail + "\n"
		changed = true
	}
	if !changed {
		return nil
	}
	updated := content[:start] + section + content[end:]
	return wiki.WriteVersionedPage(projectPath, "wiki/reviews.md", []byte(updated), "merge duplicate open review evidence")
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
