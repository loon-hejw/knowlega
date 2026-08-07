package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/sourcearchive"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

const sourceLayoutMigrationVersion = 1

var legacyHashPrefix = regexp.MustCompile(`^[0-9a-fA-F]{12}-`)

type SourceLayoutMigrationOptions struct {
	ProjectPath string `json:"project_path"`
	Apply       bool   `json:"apply"`
}

type SourceLayoutMigrationJournal struct {
	Version     int                         `json:"version"`
	ProjectPath string                      `json:"project_path"`
	Status      string                      `json:"status"`
	CreatedAt   string                      `json:"created_at"`
	UpdatedAt   string                      `json:"updated_at"`
	Error       string                      `json:"error,omitempty"`
	Items       []SourceLayoutMigrationItem `json:"items"`
}

type SourceLayoutMigrationItem struct {
	ManifestKey       string   `json:"manifest_key,omitempty"`
	OriginalSource    string   `json:"original_source"`
	OriginalName      string   `json:"original_name"`
	Collection        string   `json:"collection,omitempty"`
	OldPaths          []string `json:"old_paths"`
	OldRawPath        string   `json:"old_raw_path,omitempty"`
	TargetArchivePath string   `json:"target_archive_path"`
	TargetOriginal    string   `json:"target_original_path"`
	TargetContent     string   `json:"target_content_path"`
	SHA256            string   `json:"sha256"`
	Stage             string   `json:"stage"`
	Error             string   `json:"error,omitempty"`
}

type SourceLayoutMigrationResult struct {
	Status      string                      `json:"status"`
	DryRun      bool                        `json:"dry_run"`
	Total       int                         `json:"total"`
	Completed   int                         `json:"completed"`
	Blocked     int                         `json:"blocked"`
	JournalPath string                      `json:"journal_path"`
	Items       []SourceLayoutMigrationItem `json:"items"`
}

func PlanSourceLayoutMigration(projectPath string) (SourceLayoutMigrationResult, error) {
	journal, err := buildSourceLayoutMigrationPlan(projectPath)
	if err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	return sourceLayoutMigrationResult(journal, true), nil
}

func SourceLayoutMigrationStatus(projectPath string) (SourceLayoutMigrationResult, error) {
	journal, err := loadSourceLayoutMigrationJournal(projectPath)
	if os.IsNotExist(err) {
		return SourceLayoutMigrationResult{Status: "not_started", JournalPath: sourceLayoutMigrationJournalRel()}, nil
	}
	if err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	return sourceLayoutMigrationResult(journal, false), nil
}

// MigrateSourceLayout executes or resumes the journaled, non-LLM migration.
// The journal advances after every durable stage so a process restart can
// safely continue without regenerating wiki content.
func MigrateSourceLayout(opts SourceLayoutMigrationOptions) (SourceLayoutMigrationResult, error) {
	if !opts.Apply {
		return PlanSourceLayoutMigration(opts.ProjectPath)
	}
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	defer release()
	journal, err := loadSourceLayoutMigrationJournal(opts.ProjectPath)
	if os.IsNotExist(err) || (err == nil && journal.Status == "complete") {
		journal, err = buildSourceLayoutMigrationPlan(opts.ProjectPath)
	}
	if err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	journal.Status = "running"
	journal.Error = ""
	if journal.CreatedAt == "" {
		journal.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := saveSourceLayoutMigrationJournal(opts.ProjectPath, &journal); err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	for index := range journal.Items {
		item := &journal.Items[index]
		item.Error = ""
		if err := migrateSourceLayoutItem(opts.ProjectPath, &journal, item); err != nil {
			item.Error = err.Error()
			journal.Status = "failed"
			journal.Error = fmt.Sprintf("%s: %v", item.OriginalSource, err)
			_ = saveSourceLayoutMigrationJournal(opts.ProjectPath, &journal)
			return sourceLayoutMigrationResult(journal, false), err
		}
	}
	journal.Status = "complete"
	journal.Error = ""
	if err := saveSourceLayoutMigrationJournal(opts.ProjectPath, &journal); err != nil {
		return SourceLayoutMigrationResult{}, err
	}
	if err := RefreshRelationsArtifact(opts.ProjectPath); err != nil {
		return sourceLayoutMigrationResult(journal, false), fmt.Errorf("refresh generated relations: %w", err)
	}
	_ = appendLog(opts.ProjectPath, "source-layout-migration", "source archives", fmt.Sprintf("Migrated %d source(s) into per-source archive directories without LLM reprocessing.", len(journal.Items)))
	return sourceLayoutMigrationResult(journal, false), nil
}

func migrateSourceLayoutItem(projectPath string, journal *SourceLayoutMigrationJournal, item *SourceLayoutMigrationItem) error {
	if sourceLayoutStageBefore(item.Stage, "copied") {
		data, err := os.ReadFile(resolveMigrationPath(projectPath, item.OriginalSource))
		if err != nil {
			return fmt.Errorf("read immutable original: %w", err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != item.SHA256 {
			return fmt.Errorf("original SHA256 changed: planned=%s actual=%s", item.SHA256, got)
		}
		archive, err := sourcearchive.ImportBytes(sourcearchive.ImportOptions{
			ProjectPath:      projectPath,
			Collection:       item.Collection,
			RelativePath:     item.OriginalName,
			OriginalLocation: item.OriginalSource,
		}, data)
		if err != nil {
			return err
		}
		if filepath.ToSlash(archive.Metadata.ArchivePath) != filepath.ToSlash(item.TargetArchivePath) {
			return fmt.Errorf("archive target changed: planned=%s actual=%s", item.TargetArchivePath, archive.Metadata.ArchivePath)
		}
		if item.TargetContent != item.TargetOriginal {
			extracted, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(item.OldRawPath)))
			if err != nil {
				return fmt.Errorf("read legacy extracted content: %w", err)
			}
			archive, err = sourcearchive.SetExtracted(projectPath, archive.Metadata.OriginalRawPath, extracted, "legacy-migration")
			if err != nil {
				return err
			}
			item.TargetContent = archive.Metadata.ContentPath
		}
		item.Stage = "copied"
		if err := saveSourceLayoutMigrationJournal(projectPath, journal); err != nil {
			return err
		}
	}
	if sourceLayoutStageBefore(item.Stage, "references_updated") {
		archive, err := sourcearchive.Load(projectPath, item.TargetArchivePath)
		if err != nil {
			return err
		}
		if err := updateMigratedSourceReferences(projectPath, *item, archive.Metadata); err != nil {
			return err
		}
		item.Stage = "references_updated"
		if err := saveSourceLayoutMigrationJournal(projectPath, journal); err != nil {
			return err
		}
	}
	if sourceLayoutStageBefore(item.Stage, "old_removed") {
		for _, oldPath := range item.OldPaths {
			if err := removeLegacySourcePath(projectPath, oldPath, item.TargetArchivePath); err != nil {
				return err
			}
		}
		item.Stage = "old_removed"
		if err := saveSourceLayoutMigrationJournal(projectPath, journal); err != nil {
			return err
		}
	}
	item.Stage = "done"
	return saveSourceLayoutMigrationJournal(projectPath, journal)
}

func buildSourceLayoutMigrationPlan(projectPath string) (SourceLayoutMigrationJournal, error) {
	projectAbs, err := filepath.Abs(projectPath)
	if err != nil {
		return SourceLayoutMigrationJournal{}, err
	}
	rawRoot := filepath.Join(projectAbs, "raw", "sources")
	manifest, err := loadSourceManifestFile(projectAbs)
	if err != nil {
		return SourceLayoutMigrationJournal{}, err
	}
	claimed := map[string]bool{}
	var items []SourceLayoutMigrationItem
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := manifest.Sources[key]
		if strings.TrimSpace(entry.ArchivePath) != "" {
			continue
		}
		item, err := planManifestSourceMigration(projectAbs, rawRoot, key, entry)
		if err != nil {
			return SourceLayoutMigrationJournal{}, err
		}
		for _, old := range item.OldPaths {
			claimed[filepath.Clean(resolveMigrationPath(projectAbs, old))] = true
		}
		items = append(items, item)
	}
	archives, err := sourcearchive.Discover(projectAbs)
	if err != nil {
		return SourceLayoutMigrationJournal{}, err
	}
	archiveDirs := map[string]bool{}
	for _, archive := range archives {
		archiveDirs[filepath.Clean(archive.AbsDir)] = true
	}
	if _, statErr := os.Stat(rawRoot); statErr == nil {
		err = filepath.WalkDir(rawRoot, func(candidate string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if archiveDirs[filepath.Clean(candidate)] {
					return filepath.SkipDir
				}
				return nil
			}
			if claimed[filepath.Clean(candidate)] || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			item, itemErr := planUntrackedSourceMigration(projectAbs, rawRoot, candidate)
			if itemErr != nil {
				return itemErr
			}
			items = append(items, item)
			return nil
		})
		if err != nil {
			return SourceLayoutMigrationJournal{}, err
		}
	} else if !os.IsNotExist(statErr) {
		return SourceLayoutMigrationJournal{}, statErr
	}
	sort.Slice(items, func(i, j int) bool { return items[i].TargetArchivePath < items[j].TargetArchivePath })
	now := time.Now().UTC().Format(time.RFC3339)
	return SourceLayoutMigrationJournal{
		Version: sourceLayoutMigrationVersion, ProjectPath: projectAbs,
		Status: "planned", CreatedAt: now, UpdatedAt: now, Items: items,
	}, nil
}

func planManifestSourceMigration(projectAbs, rawRoot, key string, entry sourceManifestFileEntry) (SourceLayoutMigrationItem, error) {
	oldRawAbs := resolveMigrationPath(projectAbs, entry.RawPath)
	if _, err := os.Stat(oldRawAbs); err != nil {
		return SourceLayoutMigrationItem{}, fmt.Errorf("manifest %s raw source: %w", key, err)
	}
	originalAbs := ""
	if strings.TrimSpace(entry.OriginalPath) != "" {
		candidate := resolveMigrationPath(projectAbs, entry.OriginalPath)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			originalAbs = candidate
		}
	}
	if originalAbs == "" {
		originalAbs = oldRawAbs
	}
	originalExt := strings.ToLower(filepath.Ext(originalAbs))
	rawExt := strings.ToLower(filepath.Ext(oldRawAbs))
	if (originalExt == ".pdf" || originalExt == ".docx") && filepath.Clean(originalAbs) == filepath.Clean(oldRawAbs) && rawExt == ".md" {
		return SourceLayoutMigrationItem{}, fmt.Errorf("manifest %s requires the immutable rich-source original before migration", key)
	}
	data, err := os.ReadFile(originalAbs)
	if err != nil {
		return SourceLayoutMigrationItem{}, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if entry.SHA256 != "" && entry.SHA256 != hash {
		return SourceLayoutMigrationItem{}, fmt.Errorf("manifest %s original SHA256 mismatch", key)
	}
	originalName := filepath.Base(originalAbs)
	if filepath.Clean(originalAbs) == filepath.Clean(oldRawAbs) {
		originalName = legacyHashPrefix.ReplaceAllString(filepath.Base(oldRawAbs), "")
	}
	collection := legacyCollection(rawRoot, oldRawAbs)
	item := plannedMigrationItem(projectAbs, collection, originalName, originalAbs, hash)
	item.ManifestKey = key
	item.OldRawPath = projectRelativeOrAbsolute(projectAbs, oldRawAbs)
	item.OldPaths = []string{item.OldRawPath}
	if insidePath(rawRoot, originalAbs) && filepath.Clean(originalAbs) != filepath.Clean(oldRawAbs) {
		item.OldPaths = append(item.OldPaths, projectRelativeOrAbsolute(projectAbs, originalAbs))
	}
	if filepath.Clean(originalAbs) != filepath.Clean(oldRawAbs) && rawExt == ".md" {
		item.TargetContent = path.Join(item.TargetArchivePath, "extracted.md")
	}
	return item, nil
}

func planUntrackedSourceMigration(projectAbs, rawRoot, sourceAbs string) (SourceLayoutMigrationItem, error) {
	data, err := os.ReadFile(sourceAbs)
	if err != nil {
		return SourceLayoutMigrationItem{}, err
	}
	sum := sha256.Sum256(data)
	name := legacyHashPrefix.ReplaceAllString(filepath.Base(sourceAbs), "")
	item := plannedMigrationItem(projectAbs, legacyCollection(rawRoot, sourceAbs), name, sourceAbs, hex.EncodeToString(sum[:]))
	item.OldRawPath = projectRelativeOrAbsolute(projectAbs, sourceAbs)
	item.OldPaths = []string{item.OldRawPath}
	return item, nil
}

func plannedMigrationItem(projectAbs, collection, originalName, originalAbs, hash string) SourceLayoutMigrationItem {
	archiveRel := path.Join("raw/sources", collection, core.Slug(originalName)+"-"+hash[:12])
	originalRel := path.Join(archiveRel, "original", filepath.Base(originalName))
	return SourceLayoutMigrationItem{
		OriginalSource: projectRelativeOrAbsolute(projectAbs, originalAbs),
		OriginalName:   originalName, Collection: collection,
		TargetArchivePath: archiveRel, TargetOriginal: originalRel, TargetContent: originalRel,
		SHA256: hash, Stage: "planned",
	}
}

func updateMigratedSourceReferences(projectPath string, item SourceLayoutMigrationItem, metadata sourcearchive.Metadata) error {
	manifest, err := loadSourceManifestFile(projectPath)
	if err != nil {
		return err
	}
	if item.ManifestKey != "" {
		entry, ok := manifest.Sources[item.ManifestKey]
		if !ok {
			return fmt.Errorf("manifest entry disappeared: %s", item.ManifestKey)
		}
		oldRaw := filepath.ToSlash(entry.RawPath)
		entry.RawPath = metadata.ContentPath
		entry.ArchivePath = metadata.ArchivePath
		entry.OriginalRawPath = metadata.OriginalRawPath
		entry.ContentPath = metadata.ContentPath
		entry.OriginalSHA256 = metadata.OriginalSHA256
		entry.ContentSHA256 = metadata.ContentSHA256
		manifest.Sources[item.ManifestKey] = entry
		if err := saveSourceManifestFile(projectPath, manifest); err != nil {
			return err
		}
		if err := replaceSourcePathInWiki(projectPath, oldRaw, metadata.ContentPath); err != nil {
			return err
		}
	}
	return replaceSourcePathsInOperationalState(projectPath, item.OldPaths, metadata.OriginalRawPath)
}

func replaceSourcePathInWiki(projectPath, oldPath, newPath string) error {
	if oldPath == "" || oldPath == newPath {
		return nil
	}
	root := filepath.Join(projectPath, "wiki")
	return filepath.WalkDir(root, func(filename string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") || filepath.ToSlash(filename) == filepath.ToSlash(filepath.Join(root, "log.md")) {
			return nil
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		updated := strings.ReplaceAll(string(data), filepath.ToSlash(oldPath), filepath.ToSlash(newPath))
		if updated == string(data) {
			return nil
		}
		rel, err := filepath.Rel(projectPath, filename)
		if err != nil {
			return err
		}
		return wiki.WriteVersionedPage(projectPath, filepath.ToSlash(rel), []byte(updated), "source-layout-migration")
	})
}

func replaceSourcePathsInOperationalState(projectPath string, oldPaths []string, newOriginal string) error {
	replacements := map[string]string{}
	newAbs := filepath.ToSlash(filepath.Join(projectPath, filepath.FromSlash(newOriginal)))
	for _, old := range oldPaths {
		oldSlash := filepath.ToSlash(old)
		replacements[oldSlash] = newOriginal
		replacements[filepath.ToSlash(resolveMigrationPath(projectPath, old))] = newAbs
	}
	queue, err := LoadIngestQueue(projectPath)
	if err != nil {
		return err
	}
	queueChanged := false
	for index := range queue.Tasks {
		if replacement, ok := replacements[filepath.ToSlash(queue.Tasks[index].SourcePath)]; ok {
			queue.Tasks[index].SourcePath = replacement
			queue.Tasks[index].ID = core.StableID("ingest-task", replacement)
			queue.Tasks[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			queueChanged = true
		}
	}
	if queueChanged {
		if err := SaveIngestQueue(projectPath, queue); err != nil {
			return err
		}
	}
	watch, err := loadSourceWatch(projectPath)
	if err != nil {
		return err
	}
	watchChanged := false
	for old, replacement := range replacements {
		if entry, ok := watch.Sources[old]; ok {
			delete(watch.Sources, old)
			watch.Sources[replacement] = entry
			watchChanged = true
		}
	}
	if watchChanged {
		return saveSourceWatch(projectPath, watch)
	}
	return nil
}

func removeLegacySourcePath(projectPath, oldPath, targetArchive string) error {
	abs := resolveMigrationPath(projectPath, oldPath)
	rawRoot := filepath.Join(projectPath, "raw", "sources")
	targetAbs := filepath.Join(projectPath, filepath.FromSlash(targetArchive))
	if !insidePath(rawRoot, abs) || insidePath(targetAbs, abs) {
		return nil
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	for dir := filepath.Dir(abs); insidePath(rawRoot, dir) && filepath.Clean(dir) != filepath.Clean(rawRoot); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break
		}
	}
	return nil
}

func sourceLayoutStageBefore(got, want string) bool {
	order := map[string]int{"planned": 0, "copied": 1, "references_updated": 2, "old_removed": 3, "done": 4}
	return order[got] < order[want]
}

func sourceLayoutMigrationResult(journal SourceLayoutMigrationJournal, dryRun bool) SourceLayoutMigrationResult {
	result := SourceLayoutMigrationResult{
		Status: journal.Status, DryRun: dryRun, Total: len(journal.Items),
		JournalPath: sourceLayoutMigrationJournalRel(), Items: append([]SourceLayoutMigrationItem(nil), journal.Items...),
	}
	for _, item := range journal.Items {
		if item.Stage == "done" {
			result.Completed++
		}
		if item.Error != "" {
			result.Blocked++
		}
	}
	return result
}

func loadSourceLayoutMigrationJournal(projectPath string) (SourceLayoutMigrationJournal, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(sourceLayoutMigrationJournalRel())))
	if err != nil {
		return SourceLayoutMigrationJournal{}, err
	}
	var journal SourceLayoutMigrationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return SourceLayoutMigrationJournal{}, fmt.Errorf("read source layout migration journal: %w", err)
	}
	if journal.Version != sourceLayoutMigrationVersion {
		return SourceLayoutMigrationJournal{}, fmt.Errorf("unsupported source layout migration journal version %d", journal.Version)
	}
	return journal, nil
}

func saveSourceLayoutMigrationJournal(projectPath string, journal *SourceLayoutMigrationJournal) error {
	journal.Version = sourceLayoutMigrationVersion
	journal.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	filename := filepath.Join(projectPath, filepath.FromSlash(sourceLayoutMigrationJournalRel()))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filename, data)
}

func sourceLayoutMigrationJournalRel() string {
	return ".kbcore/source-layout-migration.json"
}

func legacyCollection(rawRoot, sourceAbs string) string {
	rel, err := filepath.Rel(rawRoot, filepath.Dir(sourceAbs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func projectRelativeOrAbsolute(projectPath, filename string) string {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return filepath.ToSlash(filename)
	}
	projectAbs, err := filepath.Abs(projectPath)
	if err == nil {
		if rel, relErr := filepath.Rel(projectAbs, abs); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(abs)
}

func resolveMigrationPath(projectPath, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(projectPath, filepath.FromSlash(value))
}

func insidePath(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
