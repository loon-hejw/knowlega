package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/sourcearchive"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type DeleteSourceOptions struct {
	ProjectPath string `json:"project_path"`
	ProjectID   string `json:"project_id"`
	SourcePath  string `json:"source_path"`
	DeleteRaw   bool   `json:"delete_raw"`
	DryRun      bool   `json:"dry_run"`
}

type DeleteSourceResult struct {
	MatchedKey      string   `json:"matched_key"`
	OriginalPath    string   `json:"original_path"`
	RawPath         string   `json:"raw_path"`
	ArchivePath     string   `json:"archive_path,omitempty"`
	Title           string   `json:"title"`
	DeletedPages    []string `json:"deleted_pages"`
	UpdatedPages    []string `json:"updated_pages"`
	CleanedPages    []string `json:"cleaned_pages"`
	ResolvedReviews []string `json:"resolved_reviews"`
	DeletedRaw      bool     `json:"deleted_raw"`
	ManifestUpdated bool     `json:"manifest_updated"`
	DryRun          bool     `json:"dry_run"`
}

func DeleteSource(opts DeleteSourceOptions) (DeleteSourceResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return DeleteSourceResult{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.SourcePath) == "" {
		return DeleteSourceResult{}, fmt.Errorf("source path is required")
	}
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return DeleteSourceResult{}, err
	}
	defer release()
	manifest, err := loadSourceManifestFile(opts.ProjectPath)
	if err != nil {
		return DeleteSourceResult{}, err
	}
	key, entry, ok := findSourceManifestEntry(manifest, opts.SourcePath)
	if !ok {
		return DeleteSourceResult{}, fmt.Errorf("source not found in manifest: %s", opts.SourcePath)
	}
	result := DeleteSourceResult{
		MatchedKey:   key,
		OriginalPath: entry.OriginalPath,
		RawPath:      filepath.ToSlash(entry.RawPath),
		ArchivePath:  filepath.ToSlash(entry.ArchivePath),
		Title:        entry.Title,
		DryRun:       opts.DryRun,
	}
	affected, err := sourceDeleteAffectedPages(opts.ProjectPath, entry)
	if err != nil {
		return DeleteSourceResult{}, err
	}
	for _, page := range affected {
		if page.Delete {
			result.DeletedPages = append(result.DeletedPages, page.Path)
		} else if page.UpdateSources {
			result.UpdatedPages = append(result.UpdatedPages, page.Path)
		}
	}
	result.CleanedPages, err = pagesWithDeletedLinks(opts.ProjectPath, result.DeletedPages)
	if err != nil {
		return DeleteSourceResult{}, err
	}
	result.ResolvedReviews, err = sourceRelatedOpenReviewIDs(opts.ProjectPath, firstNonEmptyString(opts.ProjectID, "local"), entry, result.DeletedPages)
	if err != nil {
		return DeleteSourceResult{}, err
	}
	if opts.DryRun {
		return result, nil
	}
	for _, page := range affected {
		if page.Delete {
			if err := deleteWikiPageWithArchive(opts.ProjectPath, page.Path, "source-delete: "+entry.Title); err != nil {
				return DeleteSourceResult{}, err
			}
			continue
		}
		if page.UpdateSources {
			for _, rawPath := range page.RemoveSources {
				if err := removeSourceFromWikiPage(opts.ProjectPath, page.Path, rawPath); err != nil {
					return DeleteSourceResult{}, err
				}
			}
		}
	}
	if err := cleanupDeletedWikiLinks(opts.ProjectPath, result.DeletedPages); err != nil {
		return DeleteSourceResult{}, err
	}
	if err := removeLinesContainingPaths(filepath.Join(opts.ProjectPath, "wiki", "index.md"), result.DeletedPages); err != nil {
		return DeleteSourceResult{}, err
	}
	if err := removeLinesContainingPaths(filepath.Join(opts.ProjectPath, "wiki", "overview.md"), result.DeletedPages); err != nil {
		return DeleteSourceResult{}, err
	}
	for _, id := range result.ResolvedReviews {
		if _, err := wiki.UpdateReviewItemStatusWithAction(opts.ProjectPath, firstNonEmptyString(opts.ProjectID, "local"), id, "dismissed", "source-delete", time.Now().UTC()); err != nil {
			return DeleteSourceResult{}, err
		}
	}
	for _, page := range manifestfile.OwnedFiles(entry) {
		manifestfile.UnregisterSourceOwner(&manifest, page, key)
	}
	delete(manifest.Sources, key)
	if err := saveSourceManifestFile(opts.ProjectPath, manifest); err != nil {
		return DeleteSourceResult{}, err
	}
	result.ManifestUpdated = true
	if opts.DeleteRaw {
		deleted, err := deleteRawSourceArchive(opts.ProjectPath, entry)
		if err != nil {
			return DeleteSourceResult{}, err
		}
		result.DeletedRaw = deleted
	} else if err := ignoreRawSource(opts.ProjectPath, firstNonEmptyString(entry.OriginalRawPath, result.RawPath)); err != nil {
		return DeleteSourceResult{}, err
	}
	_ = appendOverview(opts.ProjectPath, "Source Deletions", entry.Title, "wiki/log.md", "source-delete")
	_ = appendLog(opts.ProjectPath, "source-delete", entry.Title, fmt.Sprintf("Deleted source `%s`; removed %d page(s), updated %d page(s), cleaned %d page(s).", result.RawPath, len(result.DeletedPages), len(result.UpdatedPages), len(result.CleanedPages)))
	if err := RefreshRelationsArtifact(opts.ProjectPath); err != nil {
		return result, fmt.Errorf("refresh generated relations: %w", err)
	}
	return result, nil
}

type sourceDeletePageAction struct {
	Path          string
	Delete        bool
	UpdateSources bool
	RemoveSources []string
}

func loadSourceManifestFile(projectPath string) (sourceManifestFile, error) {
	return manifestfile.Load(projectPath)
}

func saveSourceManifestFile(projectPath string, manifest sourceManifestFile) error {
	return manifestfile.Save(projectPath, manifest)
}

func findSourceManifestEntry(manifest sourceManifestFile, sourcePath string) (string, sourceManifestFileEntry, bool) {
	query := normalizeSourceLookup(sourcePath)
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := manifest.Sources[key]
		candidates := []string{key, entry.OriginalPath, entry.RawPath, entry.ArchivePath, entry.OriginalRawPath, entry.ContentPath}
		candidates = append(candidates, entry.Files...)
		for _, candidate := range candidates {
			if normalizeSourceLookup(candidate) == query {
				return key, entry, true
			}
		}
	}
	return "", sourceManifestFileEntry{}, false
}

func sourceDeleteAffectedPages(projectPath string, entry sourceManifestFileEntry) ([]sourceDeletePageAction, error) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, file := range manifestfile.OwnedFiles(entry) {
		files[filepath.ToSlash(file)] = true
	}
	rawPaths := sourceEntryRawPaths(entry)
	var actions []sourceDeletePageAction
	for _, page := range pages {
		var matched []string
		for _, raw := range rawPaths {
			if containsPathString(page.Sources, raw) {
				matched = append(matched, raw)
			}
		}
		if !files[page.Path] && len(matched) == 0 {
			continue
		}
		action := sourceDeletePageAction{Path: page.Path, RemoveSources: matched}
		if page.Type == "source-summary" || len(page.Sources) <= len(matched) || (files[page.Path] && len(page.Sources) == 0 && strings.HasPrefix(page.Path, "wiki/sources/")) {
			action.Delete = true
		} else if len(matched) > 0 {
			action.UpdateSources = true
		}
		if action.Delete || action.UpdateSources {
			actions = append(actions, action)
		}
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })
	return actions, nil
}

func sourceEntryRawPaths(entry sourceManifestFileEntry) []string {
	values := []string{entry.RawPath}
	for _, version := range entry.Versions {
		values = append(values, version.RawPath)
	}
	return mergePathLists(values)
}

func pagesWithDeletedLinks(projectPath string, deleted []string) ([]string, error) {
	if len(deleted) == 0 {
		return nil, nil
	}
	deletedIDs := deletedLinkIDs(deleted)
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, page := range pages {
		if containsPathString(deleted, page.Path) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(page.Path)))
		if err != nil {
			return nil, err
		}
		if contentHasDeletedLink(string(data), deletedIDs) {
			out = append(out, page.Path)
		}
	}
	return out, nil
}

func deleteWikiPageWithArchive(projectPath, rel, reason string) error {
	rel = filepath.ToSlash(rel)
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := archiveDeletedPage(projectPath, rel, data, reason); err != nil {
		return err
	}
	return os.Remove(abs)
}

func archiveDeletedPage(projectPath, rel string, data []byte, reason string) error {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])[:12]
	versionDir := filepath.Join(projectPath, ".kbcore", "page-versions", filepath.FromSlash(strings.TrimSuffix(rel, ".md")))
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString("<!-- kbcore-page-version\n")
	b.WriteString("original: ")
	b.WriteString(rel)
	b.WriteString("\nreason: ")
	b.WriteString(strings.ReplaceAll(reason, "\n", " "))
	b.WriteString("\narchived_at: ")
	b.WriteString(now.Format(time.RFC3339Nano))
	b.WriteString("\n-->\n\n")
	b.Write(data)
	name := now.Format("20060102T150405.000000000Z") + "-" + hash + ".md"
	return os.WriteFile(filepath.Join(versionDir, name), []byte(b.String()), 0o644)
}

func removeSourceFromWikiPage(projectPath, rel, raw string) error {
	path := filepath.Join(projectPath, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated := removeSourceFromFrontmatter(string(data), raw)
	if updated == string(data) {
		return nil
	}
	return wiki.WriteVersionedPage(projectPath, rel, []byte(updated), "source-delete: remove source "+raw)
}

func removeSourceFromFrontmatter(content, raw string) string {
	if !strings.HasPrefix(strings.TrimLeft(content, "\ufeff\r\n\t "), "---\n") {
		return content
	}
	lines := strings.Split(content, "\n")
	inSources := false
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "sources:") {
			inSources = true
			out = append(out, line)
			continue
		}
		if inSources {
			if strings.HasPrefix(trimmed, "- ") {
				value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), `"'`)
				if filepath.ToSlash(value) == filepath.ToSlash(raw) {
					continue
				}
				out = append(out, line)
				continue
			}
			inSources = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func cleanupDeletedWikiLinks(projectPath string, deleted []string) error {
	if len(deleted) == 0 {
		return nil
	}
	deletedIDs := deletedLinkIDs(deleted)
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return err
	}
	for _, page := range pages {
		if containsPathString(deleted, page.Path) {
			continue
		}
		path := filepath.Join(projectPath, filepath.FromSlash(page.Path))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated := removeDeletedWikiLinks(string(data), deletedIDs)
		if updated != string(data) {
			if err := wiki.WriteVersionedPage(projectPath, page.Path, []byte(updated), "source-delete: cleanup wikilinks"); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeDeletedWikiLinks(content string, deletedIDs map[string]bool) string {
	pattern := regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	return pattern.ReplaceAllStringFunc(content, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		if len(parts) == 0 {
			return match
		}
		target := normalizeLinkID(parts[1])
		if !deletedIDs[target] {
			return match
		}
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			return parts[2]
		}
		return strings.TrimSpace(parts[1])
	})
}

func contentHasDeletedLink(content string, deletedIDs map[string]bool) bool {
	pattern := regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]+)?\]\]`)
	for _, match := range pattern.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 && deletedIDs[normalizeLinkID(match[1])] {
			return true
		}
	}
	return false
}

func deletedLinkIDs(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, path := range paths {
		path = filepath.ToSlash(path)
		stem := strings.TrimSuffix(filepath.Base(path), ".md")
		out[normalizeLinkID(path)] = true
		out[normalizeLinkID(strings.TrimSuffix(path, ".md"))] = true
		out[normalizeLinkID(stem)] = true
	}
	return out
}

func removeLinesContainingPaths(path string, paths []string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		remove := false
		for _, rel := range paths {
			if strings.Contains(line, filepath.ToSlash(rel)) || strings.Contains(line, strings.TrimSuffix(filepath.Base(rel), ".md")) {
				remove = true
				break
			}
		}
		if !remove {
			out = append(out, line)
		}
	}
	updated := strings.Join(out, "\n")
	if updated == string(data) {
		return nil
	}
	return writeFileAtomic(path, []byte(updated))
}

func sourceRelatedOpenReviewIDs(projectPath, projectID string, entry sourceManifestFileEntry, deletedPages []string) ([]string, error) {
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: projectPath, ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	raw := filepath.ToSlash(entry.RawPath)
	var ids []string
	for _, item := range items {
		if item.Status != "open" {
			continue
		}
		if filepath.ToSlash(item.SourcePath) == raw || anyOverlap(item.AffectedPages, deletedPages) {
			ids = append(ids, item.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func deleteRawSourceFile(projectPath, raw string) (bool, error) {
	raw = filepath.ToSlash(raw)
	if !strings.HasPrefix(raw, "raw/sources/") {
		return false, fmt.Errorf("raw path must be under raw/sources: %s", raw)
	}
	abs := filepath.Join(projectPath, filepath.FromSlash(raw))
	if !pathInsideProject(projectPath, abs) {
		return false, fmt.Errorf("raw path escapes project: %s", raw)
	}
	if err := os.Remove(abs); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func deleteRawSourceArchive(projectPath string, entry sourceManifestFileEntry) (bool, error) {
	deleted, err := deleteOneRawSourceArchive(projectPath, entry.ArchivePath, entry.OriginalRawPath, entry.RawPath)
	if err != nil {
		return false, err
	}
	for _, version := range entry.Versions {
		versionDeleted, versionErr := deleteOneRawSourceArchive(projectPath, version.ArchivePath, version.OriginalRawPath, version.RawPath)
		if versionErr != nil {
			return deleted, versionErr
		}
		deleted = deleted || versionDeleted
	}
	return deleted, nil
}

func deleteOneRawSourceArchive(projectPath, archivePath, originalRawPath, rawPath string) (bool, error) {
	if strings.TrimSpace(archivePath) == "" {
		return deleteRawSourceFile(projectPath, rawPath)
	}
	archive, err := sourcearchive.Load(projectPath, archivePath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if originalRawPath != "" && filepath.ToSlash(originalRawPath) != archive.Metadata.OriginalRawPath {
		return false, fmt.Errorf("source archive original path mismatch: %s", archivePath)
	}
	if rawPath != "" && filepath.ToSlash(rawPath) != archive.Metadata.ContentPath {
		return false, fmt.Errorf("source archive content path mismatch: %s", archivePath)
	}
	rawRoot := filepath.Join(projectPath, "raw", "sources")
	if !isPathInside(rawRoot, archive.AbsDir) || filepath.Clean(rawRoot) == filepath.Clean(archive.AbsDir) {
		return false, fmt.Errorf("source archive escapes raw/sources: %s", archivePath)
	}
	if err := os.RemoveAll(archive.AbsDir); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeSourceLookup(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if abs, err := filepath.Abs(value); err == nil && !strings.HasPrefix(value, "wiki/") && !strings.HasPrefix(value, "raw/") {
		value = filepath.ToSlash(abs)
	}
	return strings.ToLower(value)
}

func containsPathString(values []string, want string) bool {
	want = filepath.ToSlash(want)
	for _, value := range values {
		if filepath.ToSlash(value) == want {
			return true
		}
	}
	return false
}

func anyOverlap(a, b []string) bool {
	set := map[string]bool{}
	for _, value := range a {
		set[filepath.ToSlash(value)] = true
	}
	for _, value := range b {
		if set[filepath.ToSlash(value)] {
			return true
		}
	}
	return false
}

func pathInsideProject(projectPath, abs string) bool {
	root, err := filepath.Abs(projectPath)
	if err != nil {
		return false
	}
	clean, err := filepath.Abs(abs)
	if err != nil {
		return false
	}
	return clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator))
}
