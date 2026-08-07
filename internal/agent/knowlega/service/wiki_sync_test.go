package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestSyncWikiPagesToStoreEmbedsPages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wiki", "concepts", "oauth.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`---
type: "concept"
title: "OAuth Token Validation"
aliases:
  - "token auth"
sources:
  - "raw/sources/oauth.md"
---

# OAuth Token Validation

Token validation calls the auth service.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiPageStore{}
	embedder := &fakeEmbeddingProvider{embedding: []float32{0.4, 0.5}}

	result, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages == 0 || result.Embeddings == 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(store.pages) == 0 {
		t.Fatal("expected pages to be upserted")
	}
	if len(store.embeddings) == 0 {
		t.Fatal("expected embeddings to be upserted")
	}
	if !containsEmbeddingText(embedder.texts, "OAuth Token Validation", "token auth", "raw/sources/oauth.md", "Token validation calls") {
		t.Fatalf("embedding texts=%+v", embedder.texts)
	}
	found := false
	for _, embedding := range store.embeddings {
		if embedding.path == "wiki/concepts/oauth.md" && len(embedding.values) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected oauth embedding, got %+v", store.embeddings)
	}
}

func TestSyncWikiPagesToStoreSkipsFreshEmbeddings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth

Token validation.
`)
	store := &recordingWikiPageStore{}
	embedder := &fakeEmbeddingProvider{embedding: []float32{0.7, 0.8}}

	first, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Embeddings == 0 {
		t.Fatalf("expected first sync to embed, got %+v", first)
	}
	if second.Embeddings != 0 {
		t.Fatalf("expected unchanged second sync to skip embeddings, got %+v", second)
	}
	if embedder.called != first.Embeddings {
		t.Fatalf("embedding provider should only be called for stale pages, called=%d first=%+v second=%+v", embedder.called, first, second)
	}

	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth

Token validation changed.
`)
	third, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Embeddings != 1 {
		t.Fatalf("expected changed page to refresh one embedding, got %+v", third)
	}
}

func TestWikiPageEmbeddingTextIncludesSemanticFrontmatter(t *testing.T) {
	text := wikiPageEmbeddingText(core.WikiPage{
		Title: "OAuth",
		Type:  "concept",
		Frontmatter: map[string]any{
			"aliases": []any{"token auth", "JWT check"},
		},
		Sources: []string{"raw/sources/oauth.md"},
		Body:    "Token validation calls AuthService.",
	})
	for _, want := range []string{"OAuth", "concept", "token auth", "JWT check", "raw/sources/oauth.md", "Token validation calls"} {
		if !strings.Contains(text, want) {
			t.Fatalf("embedding text missing %q:\n%s", want, text)
		}
	}
}

func TestSyncWikiPagePathsToStoreEmbedsOnlyRequestedPages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth

Token validation.
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "ignored.md"), `---
type: "concept"
title: "Ignored"
---

# Ignored
`)
	store := &recordingWikiPageStore{}
	embedder := &fakeEmbeddingProvider{embedding: []float32{0.7, 0.8}}

	result, err := SyncWikiPagePathsToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	}, []string{"wiki/concepts/oauth.md", "wiki/missing.md", "wiki/concepts/oauth.md"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 1 || result.Embeddings != 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(store.pages) != 1 || store.pages[0].Path != "wiki/concepts/oauth.md" {
		t.Fatalf("pages=%+v", store.pages)
	}
	if len(store.embeddings) != 1 || store.embeddings[0].path != "wiki/concepts/oauth.md" {
		t.Fatalf("embeddings=%+v", store.embeddings)
	}
}

func TestSyncWikiPagesToStorePrunesStalePages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth
`)
	store := &recordingWikiPageStore{existing: map[string]bool{
		"wiki/concepts/stale.md": true,
	}}

	result, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages == 0 {
		t.Fatalf("result=%+v", result)
	}
	if store.existing["wiki/concepts/stale.md"] {
		t.Fatalf("expected stale page to be pruned, existing=%+v", store.existing)
	}
	if !store.existing["wiki/concepts/oauth.md"] {
		t.Fatalf("expected current page to remain, existing=%+v", store.existing)
	}
	if len(store.deletedKeepPaths) != 1 || !containsString(store.deletedKeepPaths[0], "wiki/concepts/oauth.md") {
		t.Fatalf("expected pruning to keep current paths, got %+v", store.deletedKeepPaths)
	}
}

func TestSyncWikiPagePathsToStoreDoesNotPruneStalePages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth
`)
	store := &recordingWikiPageStore{existing: map[string]bool{
		"wiki/concepts/stale.md": true,
	}}

	result, err := SyncWikiPagePathsToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	}, []string{"wiki/concepts/oauth.md"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 1 {
		t.Fatalf("result=%+v", result)
	}
	if !store.existing["wiki/concepts/stale.md"] {
		t.Fatalf("incremental sync should not prune stale pages, existing=%+v", store.existing)
	}
	if len(store.deletedKeepPaths) != 0 {
		t.Fatalf("incremental sync should not call pruning, got %+v", store.deletedKeepPaths)
	}
}

func TestSyncWikiPagesToStoreSyncsArchivedPageVersions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	rel := "wiki/concepts/oauth.md"
	if err := wiki.WriteVersionedPage(root, rel, []byte(`---
type: "concept"
title: "OAuth"
---

# OAuth

Old version.
`), "first write"); err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(root, rel, []byte(`---
type: "concept"
title: "OAuth"
---

# OAuth

Current version.
`), "second write"); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiPageStore{}

	result, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Versions != 1 || len(store.versions) != 1 {
		t.Fatalf("expected one synced version, result=%+v versions=%+v", result, store.versions)
	}
	if store.versions[0].Path != rel || !strings.Contains(store.versions[0].Body, "Old version") {
		t.Fatalf("unexpected synced version: %+v", store.versions[0])
	}
	if store.existingVersions["stale-version"] {
		t.Fatalf("expected stale version to be pruned, existing=%+v", store.existingVersions)
	}
	if !store.existingVersions[store.versions[0].ID] {
		t.Fatalf("expected current version to remain, existing=%+v version=%+v", store.existingVersions, store.versions[0])
	}
	if len(store.deletedVersionKeepIDs) != 1 || !containsString(store.deletedVersionKeepIDs[0], store.versions[0].ID) {
		t.Fatalf("expected version pruning keep ids, got %+v", store.deletedVersionKeepIDs)
	}
}

func TestSyncWikiPagePathsToStoreDoesNotSyncArchivedPageVersions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	rel := "wiki/concepts/oauth.md"
	if err := wiki.WriteVersionedPage(root, rel, []byte("# Old\n"), "first write"); err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(root, rel, []byte("# Current\n"), "second write"); err != nil {
		t.Fatal(err)
	}
	store := &recordingWikiPageStore{}

	result, err := SyncWikiPagePathsToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	}, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	if result.Versions != 0 || len(store.versions) != 0 {
		t.Fatalf("incremental sync should not sync versions, result=%+v versions=%+v", result, store.versions)
	}
}

func TestSyncWikiPagesToStoreSyncsReviewItemsAndPrunesStaleReviews(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), "# OAuth\n")
	mustWrite(t, filepath.Join(root, "wiki", "reviews.md"), `# Reviews

## [2026-07-02] suggestion | Expand OAuth page

- Source: `+"`raw/sources/oauth.md`"+`
- Source title: OAuth Notes
- Status: open

### Affected Pages

- `+"`wiki/concepts/oauth.md`"+`
### Detail

Add missing AuthService details.
`)
	store := &recordingWikiPageStore{existingReviews: map[string]bool{
		"stale-review": true,
	}}

	result, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviews != 1 || len(store.reviews) != 1 {
		t.Fatalf("expected one synced review, result=%+v reviews=%+v", result, store.reviews)
	}
	if store.existingReviews["stale-review"] {
		t.Fatalf("expected stale review to be pruned, existing=%+v", store.existingReviews)
	}
	if !store.existingReviews[store.reviews[0].ID] {
		t.Fatalf("expected current review to remain, existing=%+v review=%+v", store.existingReviews, store.reviews[0])
	}
	if len(store.deletedReviewKeepIDs) != 1 || !containsString(store.deletedReviewKeepIDs[0], store.reviews[0].ID) {
		t.Fatalf("expected review pruning keep ids, got %+v", store.deletedReviewKeepIDs)
	}
}

func TestSyncWikiPagePathsToStoreSyncsReviewsWithoutPruning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "reviews.md"), `# Reviews

## [2026-07-02] missing-page | Add AuthService page

- Source: `+"`raw/sources/oauth.md`"+`
- Source title: OAuth Notes
- Status: open

### Affected Pages

- `+"`wiki/concepts/oauth.md`"+`
### Detail

Create a dedicated AuthService entity page.
`)
	store := &recordingWikiPageStore{existingReviews: map[string]bool{
		"stale-review": true,
	}}

	result, err := SyncWikiPagePathsToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	}, []string{"wiki/reviews.md"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviews != 1 || len(store.reviews) != 1 {
		t.Fatalf("expected one synced review, result=%+v reviews=%+v", result, store.reviews)
	}
	if !store.existingReviews["stale-review"] {
		t.Fatalf("incremental review sync should not prune stale reviews, existing=%+v", store.existingReviews)
	}
	if len(store.deletedReviewKeepIDs) != 0 {
		t.Fatalf("incremental review sync should not prune, got %+v", store.deletedReviewKeepIDs)
	}
}

func TestSyncWikiPagesToStoreSyncsSourceManifestAndPrunesStaleEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), "# OAuth\n")
	mustWrite(t, filepath.Join(root, ".kbcore", "source-manifest.json"), `{
  "version": 1,
  "sources": {
    "/tmp/oauth-notes.md": {
      "original_path": "/tmp/oauth-notes.md",
      "sha256": "abc123",
      "raw_path": "raw/sources/abc123-oauth-notes.md",
      "title": "OAuth Notes",
      "files": ["wiki/concepts/oauth.md"],
      "review_count": 2,
      "updated_at": "2026-07-02T10:00:00Z",
      "versions": [{
        "sha256": "older123",
        "raw_path": "raw/sources/older-oauth-notes.md",
        "updated_at": "2026-07-01T10:00:00Z"
      }]
    }
  }
}
`)
	store := &recordingWikiPageStore{
		existingSourceManifest: map[string]bool{"stale-source": true},
		existingSources:        map[string]bool{"stale-source-row": true},
	}

	result, err := SyncWikiPagesToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceManifestEntries != 1 || result.Sources != 2 || len(store.sourceManifestEntries) != 1 || len(store.sources) != 2 {
		t.Fatalf("expected one source manifest entry, result=%+v entries=%+v", result, store.sourceManifestEntries)
	}
	entry := store.sourceManifestEntries[0]
	if entry.ProjectID != "project-1" || entry.OriginalPath != "/tmp/oauth-notes.md" || entry.SHA256 != "abc123" {
		t.Fatalf("unexpected manifest entry: %+v", entry)
	}
	if entry.ReviewCount != 2 || len(entry.Files) != 1 || entry.Files[0] != "wiki/concepts/oauth.md" {
		t.Fatalf("unexpected manifest entry details: %+v", entry)
	}
	if store.existingSourceManifest["stale-source"] {
		t.Fatalf("expected stale source manifest entry to be pruned, existing=%+v", store.existingSourceManifest)
	}
	if !store.existingSourceManifest[entry.ID] {
		t.Fatalf("expected current source manifest entry to remain, existing=%+v entry=%+v", store.existingSourceManifest, entry)
	}
	if len(store.deletedSourceManifestKeepIDs) != 1 || !containsString(store.deletedSourceManifestKeepIDs[0], entry.ID) {
		t.Fatalf("expected source manifest pruning keep ids, got %+v", store.deletedSourceManifestKeepIDs)
	}
	source := store.sources[0]
	if source.ProjectID != "project-1" || !source.Immutable {
		t.Fatalf("unexpected source row: %+v", source)
	}
	paths := map[string]string{}
	for _, item := range store.sources {
		paths[item.Path] = item.SHA256
	}
	if paths[entry.RawPath] != entry.SHA256 || paths["raw/sources/older-oauth-notes.md"] != "older123" {
		t.Fatalf("source versions=%+v", store.sources)
	}
	if store.existingSources["stale-source-row"] {
		t.Fatalf("expected stale source row to be pruned, existing=%+v", store.existingSources)
	}
	if !store.existingSources[source.ID] {
		t.Fatalf("expected current source row to remain, existing=%+v source=%+v", store.existingSources, source)
	}
}

func TestSyncSourceManifestToStoreUpsertsWithoutPruning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, ".kbcore", "source-manifest.json"), `{
  "version": 1,
  "sources": {
    "/tmp/oauth-notes.md": {
      "original_path": "/tmp/oauth-notes.md",
      "sha256": "abc123",
      "raw_path": "raw/sources/abc123-oauth-notes.md",
      "title": "OAuth Notes",
      "files": ["wiki/concepts/oauth.md"],
      "review_count": 0,
      "updated_at": "2026-07-02T10:00:00Z"
    }
  }
}
`)
	store := &recordingWikiPageStore{
		existingSourceManifest: map[string]bool{"stale-source": true},
		existingSources:        map[string]bool{"stale-source-row": true},
	}

	count, err := SyncSourceManifestToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.sourceManifestEntries) != 1 || len(store.sources) != 1 {
		t.Fatalf("expected one source manifest entry, count=%d entries=%+v", count, store.sourceManifestEntries)
	}
	if !store.existingSourceManifest["stale-source"] {
		t.Fatalf("upsert-only source manifest sync should not prune stale entries, existing=%+v", store.existingSourceManifest)
	}
	if len(store.deletedSourceManifestKeepIDs) != 0 {
		t.Fatalf("upsert-only source manifest sync should not prune, got %+v", store.deletedSourceManifestKeepIDs)
	}
	if !store.existingSources["stale-source-row"] {
		t.Fatalf("upsert-only source sync should not prune stale source rows, existing=%+v", store.existingSources)
	}
	if len(store.deletedSourceKeepIDs) != 0 {
		t.Fatalf("upsert-only source sync should not prune, got %+v", store.deletedSourceKeepIDs)
	}
}

func TestSyncWikiPagePathsToStoreUsesTransactionalStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth"
---

# OAuth

Token validation.
`)
	store := &transactionalRecordingWikiPageStore{}
	embedder := failingEmbeddingProvider{}

	_, err := SyncWikiPagePathsToStore(context.Background(), WikiSyncOptions{
		ProjectPath:       root,
		ProjectID:         "project-1",
		Store:             store,
		EmbeddingProvider: embedder,
	}, []string{"wiki/concepts/oauth.md"})
	if err == nil || !strings.Contains(err.Error(), "embedding failed") {
		t.Fatalf("expected embedding failure, got %v", err)
	}
	if !store.txCalled {
		t.Fatal("expected transactional store to be used")
	}
	if len(store.pages) != 0 || len(store.embeddings) != 0 {
		t.Fatalf("transactional store should not commit partial sync on error: pages=%+v embeddings=%+v", store.pages, store.embeddings)
	}
}

func containsEmbeddingText(texts []string, parts ...string) bool {
	for _, text := range texts {
		all := true
		for _, part := range parts {
			if !strings.Contains(text, part) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

type recordingWikiPageStore struct {
	pages                        []core.WikiPage
	sources                      []core.Source
	versions                     []core.WikiPageVersion
	reviews                      []core.ReviewItem
	sourceManifestEntries        []core.SourceManifestEntry
	embeddings                   []recordedEmbedding
	statuses                     map[string]WikiPageEmbeddingStatus
	existing                     map[string]bool
	existingSources              map[string]bool
	existingVersions             map[string]bool
	existingReviews              map[string]bool
	existingSourceManifest       map[string]bool
	deletedKeepPaths             [][]string
	deletedSourceKeepIDs         [][]string
	deletedVersionKeepIDs        [][]string
	deletedReviewKeepIDs         [][]string
	deletedSourceManifestKeepIDs [][]string
}

type transactionalRecordingWikiPageStore struct {
	recordingWikiPageStore
	txCalled bool
}

func (s *transactionalRecordingWikiPageStore) WithWikiPageStoreTx(ctx context.Context, fn func(WikiPageStore) error) error {
	s.txCalled = true
	txStore := &recordingWikiPageStore{
		statuses:               cloneEmbeddingStatuses(s.statuses),
		existing:               cloneExistingPages(s.existing),
		existingSources:        cloneExistingPages(s.existingSources),
		existingVersions:       cloneExistingPages(s.existingVersions),
		existingReviews:        cloneExistingPages(s.existingReviews),
		existingSourceManifest: cloneExistingPages(s.existingSourceManifest),
	}
	if err := fn(txStore); err != nil {
		return err
	}
	s.pages = append(s.pages, txStore.pages...)
	s.sources = append(s.sources, txStore.sources...)
	s.versions = append(s.versions, txStore.versions...)
	s.reviews = append(s.reviews, txStore.reviews...)
	s.sourceManifestEntries = append(s.sourceManifestEntries, txStore.sourceManifestEntries...)
	s.embeddings = append(s.embeddings, txStore.embeddings...)
	s.statuses = txStore.statuses
	s.existing = txStore.existing
	s.existingSources = txStore.existingSources
	s.existingVersions = txStore.existingVersions
	s.existingReviews = txStore.existingReviews
	s.existingSourceManifest = txStore.existingSourceManifest
	s.deletedKeepPaths = append(s.deletedKeepPaths, txStore.deletedKeepPaths...)
	s.deletedSourceKeepIDs = append(s.deletedSourceKeepIDs, txStore.deletedSourceKeepIDs...)
	s.deletedVersionKeepIDs = append(s.deletedVersionKeepIDs, txStore.deletedVersionKeepIDs...)
	s.deletedReviewKeepIDs = append(s.deletedReviewKeepIDs, txStore.deletedReviewKeepIDs...)
	s.deletedSourceManifestKeepIDs = append(s.deletedSourceManifestKeepIDs, txStore.deletedSourceManifestKeepIDs...)
	return nil
}

func cloneEmbeddingStatuses(in map[string]WikiPageEmbeddingStatus) map[string]WikiPageEmbeddingStatus {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]WikiPageEmbeddingStatus, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneExistingPages(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type failingEmbeddingProvider struct{}

func (failingEmbeddingProvider) EmbedText(context.Context, string) ([]float32, error) {
	return nil, fmt.Errorf("embedding failed")
}

type recordedEmbedding struct {
	projectID string
	path      string
	values    []float32
}

func (s *recordingWikiPageStore) UpsertWikiPages(_ context.Context, pages []core.WikiPage) error {
	s.pages = append(s.pages, pages...)
	if s.existing == nil {
		s.existing = map[string]bool{}
	}
	for _, page := range pages {
		s.existing[page.Path] = true
	}
	return nil
}

func (s *recordingWikiPageStore) UpsertSource(_ context.Context, source core.Source) error {
	s.sources = append(s.sources, source)
	if s.existingSources == nil {
		s.existingSources = map[string]bool{}
	}
	s.existingSources[source.ID] = true
	return nil
}

func (s *recordingWikiPageStore) DeleteSourcesNotIn(_ context.Context, _ string, ids []string) error {
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	s.deletedSourceKeepIDs = append(s.deletedSourceKeepIDs, append([]string(nil), ids...))
	for id := range s.existingSources {
		if !keep[id] {
			delete(s.existingSources, id)
		}
	}
	return nil
}

func (s *recordingWikiPageStore) InsertWikiPageVersion(_ context.Context, version core.WikiPageVersion) error {
	s.versions = append(s.versions, version)
	if s.existingVersions == nil {
		s.existingVersions = map[string]bool{}
	}
	s.existingVersions[version.ID] = true
	return nil
}

func (s *recordingWikiPageStore) DeleteWikiPageVersionsNotIn(_ context.Context, _ string, ids []string) error {
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	s.deletedVersionKeepIDs = append(s.deletedVersionKeepIDs, append([]string(nil), ids...))
	for id := range s.existingVersions {
		if !keep[id] {
			delete(s.existingVersions, id)
		}
	}
	return nil
}

func (s *recordingWikiPageStore) UpsertReviewItem(_ context.Context, item core.ReviewItem) error {
	s.reviews = append(s.reviews, item)
	if s.existingReviews == nil {
		s.existingReviews = map[string]bool{}
	}
	s.existingReviews[item.ID] = true
	return nil
}

func (s *recordingWikiPageStore) DeleteReviewItemsNotIn(_ context.Context, _ string, ids []string) error {
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	s.deletedReviewKeepIDs = append(s.deletedReviewKeepIDs, append([]string(nil), ids...))
	for id := range s.existingReviews {
		if !keep[id] {
			delete(s.existingReviews, id)
		}
	}
	return nil
}

func (s *recordingWikiPageStore) UpsertSourceManifestEntry(_ context.Context, entry core.SourceManifestEntry) error {
	s.sourceManifestEntries = append(s.sourceManifestEntries, entry)
	if s.existingSourceManifest == nil {
		s.existingSourceManifest = map[string]bool{}
	}
	s.existingSourceManifest[entry.ID] = true
	return nil
}

func (s *recordingWikiPageStore) DeleteSourceManifestEntriesNotIn(_ context.Context, _ string, ids []string) error {
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	s.deletedSourceManifestKeepIDs = append(s.deletedSourceManifestKeepIDs, append([]string(nil), ids...))
	for id := range s.existingSourceManifest {
		if !keep[id] {
			delete(s.existingSourceManifest, id)
		}
	}
	return nil
}

func (s *recordingWikiPageStore) DeleteWikiPagesNotIn(_ context.Context, _ string, paths []string) error {
	keep := map[string]bool{}
	for _, path := range paths {
		keep[path] = true
	}
	s.deletedKeepPaths = append(s.deletedKeepPaths, append([]string(nil), paths...))
	for path := range s.existing {
		if !keep[path] {
			delete(s.existing, path)
		}
	}
	return nil
}

func (s *recordingWikiPageStore) WikiPageEmbeddingStatus(_ context.Context, projectID, path string) (WikiPageEmbeddingStatus, error) {
	if s.statuses == nil {
		return WikiPageEmbeddingStatus{}, nil
	}
	return s.statuses[projectID+"/"+path], nil
}

func (s *recordingWikiPageStore) UpsertWikiPageEmbedding(_ context.Context, projectID, path string, embedding []float32, meta WikiPageEmbeddingMetadata) error {
	s.embeddings = append(s.embeddings, recordedEmbedding{
		projectID: projectID,
		path:      path,
		values:    embedding,
	})
	if s.statuses == nil {
		s.statuses = map[string]WikiPageEmbeddingStatus{}
	}
	s.statuses[projectID+"/"+path] = WikiPageEmbeddingStatus{
		Model:        meta.Model,
		SourceSHA256: meta.SourceSHA256,
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
