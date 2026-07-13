package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type WikiPageStore = core.WikiPageStore
type WikiPageEmbeddingStatus = core.WikiPageEmbeddingStatus
type WikiPageEmbeddingMetadata = core.WikiPageEmbeddingMetadata

type WikiPageVersionStore interface {
	InsertWikiPageVersion(context.Context, core.WikiPageVersion) error
}

type WikiPageVersionPruneStore interface {
	DeleteWikiPageVersionsNotIn(context.Context, string, []string) error
}

type ReviewItemStore interface {
	UpsertReviewItem(context.Context, core.ReviewItem) error
}

type ReviewItemPruneStore interface {
	DeleteReviewItemsNotIn(context.Context, string, []string) error
}

type SourceManifestStore interface {
	UpsertSourceManifestEntry(context.Context, core.SourceManifestEntry) error
}

type SourceManifestPruneStore interface {
	DeleteSourceManifestEntriesNotIn(context.Context, string, []string) error
}

type SourceStore interface {
	UpsertSource(context.Context, core.Source) error
}

type SourcePruneStore interface {
	DeleteSourcesNotIn(context.Context, string, []string) error
}

type TransactionalWikiPageStore interface {
	WikiPageStore
	WithWikiPageStoreTx(context.Context, func(WikiPageStore) error) error
}

type ModelEmbeddingProvider interface {
	EmbeddingProvider
	EmbeddingModel() string
}

type WikiSyncOptions struct {
	ProjectPath       string
	ProjectID         string
	Store             WikiPageStore
	EmbeddingProvider EmbeddingProvider
}

type WikiSyncResult struct {
	Pages                 int
	Versions              int
	Reviews               int
	Sources               int
	SourceManifestEntries int
	Embeddings            int
}

func SyncWikiPagesToStore(ctx context.Context, opts WikiSyncOptions) (WikiSyncResult, error) {
	if txStore, ok := opts.Store.(TransactionalWikiPageStore); ok {
		var result WikiSyncResult
		err := txStore.WithWikiPageStoreTx(ctx, func(store WikiPageStore) error {
			txOpts := opts
			txOpts.Store = store
			var syncErr error
			result, syncErr = syncWikiPagesToStore(ctx, txOpts)
			return syncErr
		})
		return result, err
	}
	return syncWikiPagesToStore(ctx, opts)
}

func syncWikiPagesToStore(ctx context.Context, opts WikiSyncOptions) (WikiSyncResult, error) {
	if opts.Store == nil {
		return WikiSyncResult{}, fmt.Errorf("wiki page store is required")
	}
	if strings.TrimSpace(opts.ProjectID) == "" {
		return WikiSyncResult{}, fmt.Errorf("project id is required")
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{
		ProjectPath: opts.ProjectPath,
		ProjectID:   opts.ProjectID,
	})
	if err != nil {
		return WikiSyncResult{}, err
	}
	if err := opts.Store.UpsertWikiPages(ctx, pages); err != nil {
		return WikiSyncResult{}, err
	}
	paths := make([]string, 0, len(pages))
	for _, page := range pages {
		paths = append(paths, page.Path)
	}
	if err := opts.Store.DeleteWikiPagesNotIn(ctx, opts.ProjectID, paths); err != nil {
		return WikiSyncResult{}, err
	}
	result := WikiSyncResult{Pages: len(pages)}
	if versionStore, ok := opts.Store.(WikiPageVersionStore); ok {
		versions, err := wiki.ScanWikiPageVersions(wiki.ScanOptions{
			ProjectPath: opts.ProjectPath,
			ProjectID:   opts.ProjectID,
		})
		if err != nil {
			return WikiSyncResult{}, err
		}
		ids := make([]string, 0, len(versions))
		for _, version := range versions {
			if err := versionStore.InsertWikiPageVersion(ctx, version); err != nil {
				return WikiSyncResult{}, err
			}
			ids = append(ids, version.ID)
		}
		if pruneStore, ok := opts.Store.(WikiPageVersionPruneStore); ok {
			if err := pruneStore.DeleteWikiPageVersionsNotIn(ctx, opts.ProjectID, ids); err != nil {
				return WikiSyncResult{}, err
			}
		}
		result.Versions = len(versions)
	}
	reviews, err := syncReviewItems(ctx, opts, true)
	if err != nil {
		return WikiSyncResult{}, err
	}
	result.Reviews = reviews
	sourceManifestEntries, err := syncSourceManifestEntries(ctx, opts, true)
	if err != nil {
		return WikiSyncResult{}, err
	}
	result.SourceManifestEntries = sourceManifestEntries
	sources, err := syncSourcesFromManifest(ctx, opts, true)
	if err != nil {
		return WikiSyncResult{}, err
	}
	result.Sources = sources
	if opts.EmbeddingProvider == nil {
		return result, nil
	}
	for _, page := range pages {
		updated, err := syncWikiPageEmbedding(ctx, opts, page)
		if err != nil {
			return result, err
		}
		if updated {
			result.Embeddings++
		}
	}
	return result, nil
}

func SyncWikiPagePathsToStore(ctx context.Context, opts WikiSyncOptions, paths []string) (WikiSyncResult, error) {
	if txStore, ok := opts.Store.(TransactionalWikiPageStore); ok {
		var result WikiSyncResult
		err := txStore.WithWikiPageStoreTx(ctx, func(store WikiPageStore) error {
			txOpts := opts
			txOpts.Store = store
			var syncErr error
			result, syncErr = syncWikiPagePathsToStore(ctx, txOpts, paths)
			return syncErr
		})
		return result, err
	}
	return syncWikiPagePathsToStore(ctx, opts, paths)
}

func SyncSourceManifestToStore(ctx context.Context, opts WikiSyncOptions) (int, error) {
	if txStore, ok := opts.Store.(TransactionalWikiPageStore); ok {
		var result int
		err := txStore.WithWikiPageStoreTx(ctx, func(store WikiPageStore) error {
			txOpts := opts
			txOpts.Store = store
			count, syncErr := syncSourceManifestEntries(ctx, txOpts, false)
			if syncErr != nil {
				return syncErr
			}
			if _, syncErr := syncSourcesFromManifest(ctx, txOpts, false); syncErr != nil {
				return syncErr
			}
			result = count
			return nil
		})
		return result, err
	}
	count, err := syncSourceManifestEntries(ctx, opts, false)
	if err != nil {
		return 0, err
	}
	if _, err := syncSourcesFromManifest(ctx, opts, false); err != nil {
		return 0, err
	}
	return count, nil
}

func syncWikiPagePathsToStore(ctx context.Context, opts WikiSyncOptions, paths []string) (WikiSyncResult, error) {
	if opts.Store == nil {
		return WikiSyncResult{}, fmt.Errorf("wiki page store is required")
	}
	if strings.TrimSpace(opts.ProjectID) == "" {
		return WikiSyncResult{}, fmt.Errorf("project id is required")
	}
	pages := make([]core.WikiPage, 0, len(paths))
	seen := map[string]bool{}
	for _, rel := range paths {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		data, err := os.ReadFile(filepath.Join(opts.ProjectPath, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return WikiSyncResult{}, err
		}
		page := wiki.ParseWikiPage(opts.ProjectID, rel, string(data))
		if info, statErr := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(rel))); statErr == nil {
			page.UpdatedAt = info.ModTime()
		}
		pages = append(pages, page)
	}
	if len(pages) == 0 {
		return WikiSyncResult{}, nil
	}
	if err := opts.Store.UpsertWikiPages(ctx, pages); err != nil {
		return WikiSyncResult{}, err
	}
	result := WikiSyncResult{Pages: len(pages)}
	if containsWikiReviewsPath(paths) {
		reviews, err := syncReviewItems(ctx, opts, false)
		if err != nil {
			return WikiSyncResult{}, err
		}
		result.Reviews = reviews
	}
	if opts.EmbeddingProvider == nil {
		return result, nil
	}
	for _, page := range pages {
		updated, err := syncWikiPageEmbedding(ctx, opts, page)
		if err != nil {
			return result, err
		}
		if updated {
			result.Embeddings++
		}
	}
	return result, nil
}

func syncWikiPageEmbedding(ctx context.Context, opts WikiSyncOptions, page core.WikiPage) (bool, error) {
	text := wikiPageEmbeddingText(page)
	if text == "" {
		return false, nil
	}
	model := embeddingModelName(opts.EmbeddingProvider)
	sourceSHA := sha256Hex(text)
	status, err := opts.Store.WikiPageEmbeddingStatus(ctx, opts.ProjectID, page.Path)
	if err != nil {
		return false, err
	}
	if status.Model == model && status.SourceSHA256 == sourceSHA {
		return false, nil
	}
	embedding, err := opts.EmbeddingProvider.EmbedText(ctx, text)
	if err != nil {
		return false, err
	}
	if len(embedding) == 0 {
		return false, nil
	}
	if err := opts.Store.UpsertWikiPageEmbedding(ctx, opts.ProjectID, page.Path, embedding, WikiPageEmbeddingMetadata{
		Model:        model,
		SourceSHA256: sourceSHA,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func syncSourceManifestEntries(ctx context.Context, opts WikiSyncOptions, prune bool) (int, error) {
	store, ok := opts.Store.(SourceManifestStore)
	if !ok {
		return 0, nil
	}
	entries, err := loadSourceManifestEntries(opts.ProjectPath, opts.ProjectID)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := store.UpsertSourceManifestEntry(ctx, entry); err != nil {
			return 0, err
		}
		ids = append(ids, entry.ID)
	}
	if prune {
		if pruneStore, ok := opts.Store.(SourceManifestPruneStore); ok {
			if err := pruneStore.DeleteSourceManifestEntriesNotIn(ctx, opts.ProjectID, ids); err != nil {
				return 0, err
			}
		}
	}
	return len(entries), nil
}

func syncSourcesFromManifest(ctx context.Context, opts WikiSyncOptions, prune bool) (int, error) {
	store, ok := opts.Store.(SourceStore)
	if !ok {
		return 0, nil
	}
	entries, err := loadSourceManifestEntries(opts.ProjectPath, opts.ProjectID)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		source := core.Source{
			ID:           core.StableID(opts.ProjectID, "source", entry.RawPath),
			ProjectID:    opts.ProjectID,
			Path:         entry.RawPath,
			Kind:         "source",
			Title:        entry.Title,
			SHA256:       entry.SHA256,
			Immutable:    true,
			ImportedAt:   entry.UpdatedAt,
			OriginalPath: entry.OriginalPath,
		}
		if source.ImportedAt.IsZero() {
			source.ImportedAt = time.Now()
		}
		if err := store.UpsertSource(ctx, source); err != nil {
			return 0, err
		}
		ids = append(ids, source.ID)
	}
	if prune {
		if pruneStore, ok := opts.Store.(SourcePruneStore); ok {
			if err := pruneStore.DeleteSourcesNotIn(ctx, opts.ProjectID, ids); err != nil {
				return 0, err
			}
		}
	}
	return len(entries), nil
}

type sourceManifestFile struct {
	Version int                                `json:"version"`
	Sources map[string]sourceManifestFileEntry `json:"sources"`
}

type sourceManifestFileEntry struct {
	OriginalPath    string          `json:"original_path"`
	PipelineVersion int             `json:"pipeline_version,omitempty"`
	SHA256          string          `json:"sha256"`
	RawPath         string          `json:"raw_path"`
	ArchivePath     string          `json:"archive_path,omitempty"`
	OriginalRawPath string          `json:"original_raw_path,omitempty"`
	ContentPath     string          `json:"content_path,omitempty"`
	OriginalSHA256  string          `json:"original_sha256,omitempty"`
	ContentSHA256   string          `json:"content_sha256,omitempty"`
	Title           string          `json:"title"`
	Files           []string        `json:"files"`
	ReviewCount     int             `json:"review_count"`
	UpdatedAt       string          `json:"updated_at"`
	Extraction      json.RawMessage `json:"extraction,omitempty"`
}

func loadSourceManifestEntries(projectPath, projectID string) ([]core.SourceManifestEntry, error) {
	path := filepath.Join(projectPath, ".kbcore", "source-manifest.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var manifest sourceManifestFile
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("read source manifest: %w", err)
	}
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sortStrings(keys)
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

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func syncReviewItems(ctx context.Context, opts WikiSyncOptions, prune bool) (int, error) {
	reviewStore, ok := opts.Store.(ReviewItemStore)
	if !ok {
		return 0, nil
	}
	reviews, err := wiki.ScanReviewItems(wiki.ScanOptions{
		ProjectPath: opts.ProjectPath,
		ProjectID:   opts.ProjectID,
	})
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(reviews))
	for _, review := range reviews {
		if err := reviewStore.UpsertReviewItem(ctx, review); err != nil {
			return 0, err
		}
		ids = append(ids, review.ID)
	}
	if prune {
		if pruneStore, ok := opts.Store.(ReviewItemPruneStore); ok {
			if err := pruneStore.DeleteReviewItemsNotIn(ctx, opts.ProjectID, ids); err != nil {
				return 0, err
			}
		}
	}
	return len(reviews), nil
}

func containsWikiReviewsPath(paths []string) bool {
	for _, path := range paths {
		if filepath.ToSlash(strings.TrimSpace(path)) == "wiki/reviews.md" {
			return true
		}
	}
	return false
}

func wikiPageEmbeddingText(page core.WikiPage) string {
	parts := []string{
		page.Title,
		page.Type,
		strings.Join(stringListFromFrontmatter(page.Frontmatter, "aliases"), "\n"),
		strings.Join(page.Sources, "\n"),
		page.Body,
	}
	return strings.TrimSpace(strings.Join(nonEmptyStrings(parts), "\n\n"))
}

func stringListFromFrontmatter(frontmatter map[string]any, key string) []string {
	if len(frontmatter) == 0 {
		return nil
	}
	value, ok := frontmatter[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func embeddingModelName(provider EmbeddingProvider) string {
	if provider == nil {
		return ""
	}
	if modelProvider, ok := provider.(ModelEmbeddingProvider); ok {
		return strings.TrimSpace(modelProvider.EmbeddingModel())
	}
	return fmt.Sprintf("%T", provider)
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
