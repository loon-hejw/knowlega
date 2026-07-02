package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestIngestQueryAndLint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# OAuth Notes\n\nToken validation calls the auth service."), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := IngestSource(IngestOptions{ProjectPath: root, SourcePath: source, Title: "OAuth Notes"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RawPath == "" || result.WikiPath == "" || result.SHA256 == "" {
		t.Fatalf("incomplete ingest result: %+v", result)
	}
	results, err := QueryWiki(root, "token auth", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected query result")
	}
	overview, err := os.ReadFile(filepath.Join(root, "wiki", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overview), "Recent Source Updates") || !strings.Contains(string(overview), result.WikiPath) {
		t.Fatalf("overview missing source update:\n%s", overview)
	}
	manifest, err := os.ReadFile(filepath.Join(root, ".kbcore", "source-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), result.RawPath) || !strings.Contains(string(manifest), result.WikiPath) || !strings.Contains(string(manifest), result.SHA256) {
		t.Fatalf("source manifest missing ingest provenance:\n%s", manifest)
	}
	store := &recordingWikiPageStore{}
	count, err := SyncSourceManifestToStore(context.Background(), WikiSyncOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.sourceManifestEntries) != 1 || len(store.sources) != 1 {
		t.Fatalf("expected manifest and source sync from ingest, count=%d entries=%+v sources=%+v", count, store.sourceManifestEntries, store.sources)
	}
	if store.sources[0].Path != result.RawPath || store.sources[0].SHA256 != result.SHA256 {
		t.Fatalf("unexpected synced source: %+v", store.sources[0])
	}
	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("expected deterministic lint issues for unlinked generated source page")
	}
}

func TestLintReportsKnownAliasMentionsWithoutLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "chapter-054-concept.md"), `---
type: "concept"
title: "第五十四回 法性西来逢女国"
aliases:
  - "女儿国"
---

# 第五十四回 法性西来逢女国

西梁女国相关情节。
`)
	mustWrite(t, filepath.Join(root, "wiki", "syntheses", "journey.md"), `---
type: "synthesis"
title: "Journey Notes"
---

# Journey Notes

唐僧和八戒经过女儿国，但这里还没有链接。
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLintIssue(issues, "missing-link", "wiki/syntheses/journey.md", "女儿国") {
		t.Fatalf("expected missing-link issue for alias mention, got %+v", issues)
	}

	mustWrite(t, filepath.Join(root, "wiki", "syntheses", "journey.md"), `---
type: "synthesis"
title: "Journey Notes"
---

# Journey Notes

唐僧和八戒经过[[chapter-054-concept|女儿国]]。
`)
	issues, err = LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/syntheses/journey.md", "女儿国") {
		t.Fatalf("did not expect missing-link after wikilink, got %+v", issues)
	}
}

func TestLintResolvesWikiLinksByTitleAndAlias(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "north-garden-rose-bed.md"), `---
type: "entity"
title: "North Garden Rose Bed"
aliases:
  - "Rose Plan"
---

# North Garden Rose Bed

The [[Rose Plan]] points back to this bed.
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "spring-planting.md"), `---
type: "concept"
title: "Spring Planting"
---

# Spring Planting

Use [[North Garden Rose Bed]] during spring planting.
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "broken-link", "wiki/concepts/spring-planting.md", "North Garden Rose Bed") ||
		hasLintIssue(issues, "broken-link", "wiki/entities/north-garden-rose-bed.md", "Rose Plan") {
		t.Fatalf("title/alias wikilinks should resolve, got %+v", issues)
	}
}

func TestLintPrefersDurablePageOverSourceSummaryForDuplicateTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "garden-source.md"), `---
type: "source-summary"
title: "North Garden Rose Bed"
---

# North Garden Rose Bed

This source mentions the [[North Garden Rose Bed]].
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "north-garden-rose-bed.md"), `---
type: "entity"
title: "North Garden Rose Bed"
---

# North Garden Rose Bed

The [[North Garden Rose Bed]] uses [[Compost Blend A]].
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "compost-blend-a.md"), `---
type: "entity"
title: "Compost Blend A"
---

# Compost Blend A

[[Compost Blend A]] is used by [[North Garden Rose Bed]].
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "spring-planting.md"), `---
type: "concept"
title: "Spring Planting"
---

# Spring Planting

Use [[North Garden Rose Bed]] in spring.
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "orphan", "wiki/entities/north-garden-rose-bed.md", "") {
		t.Fatalf("entity title link should receive inbound links, got %+v", issues)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/spring-planting.md", "north garden rose bed") {
		t.Fatalf("canonical duplicate title link should satisfy missing-link, got %+v", issues)
	}
}

func TestLintDoesNotRequireMissingLinksToSourceSummaryAliases(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "watering.md"), `---
type: "source-summary"
title: "Rose Bed Watering Notes"
aliases:
  - "watering notes"
---

# Rose Bed Watering Notes

The [[Compost Blend A]] note is referenced here.
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "compost-blend-a.md"), `---
type: "entity"
title: "Compost Blend A"
---

# Compost Blend A

Current garden and watering notes both name [[Compost Blend A]].
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/entities/compost-blend-a.md", "watering notes") {
		t.Fatalf("source-summary alias should not force missing-link, got %+v", issues)
	}
}

func TestLintDoesNotReportSourceSummaryOrphans(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "sources", "garden-source.md"), `---
type: "source-summary"
title: "Garden Source"
---

# Garden Source

This source links to [[Compost Blend A]].
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "compost-blend-a.md"), `---
type: "entity"
title: "Compost Blend A"
---

# Compost Blend A

[[Compost Blend A]] is a garden amendment.
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "orphan", "wiki/sources/garden-source.md", "") {
		t.Fatalf("source-summary should not be treated as orphan, got %+v", issues)
	}
}

func TestLintMentionMatchingUsesWordBoundaries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "compost-blend-b.md"), `---
type: "entity"
title: "Compost Blend B"
aliases:
  - "compost b"
---

# Compost Blend B

[[Compost Blend B]] is historical.
`)
	mustWrite(t, filepath.Join(root, "wiki", "entities", "compost-blend-a.md"), `---
type: "entity"
title: "Compost Blend A"
---

# Compost Blend A

[[Compost Blend A]] is current.
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "spring.md"), `---
type: "concept"
title: "Spring Notes"
---

# Spring Notes

The bed uses [[Compost Blend A]] in spring.
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/spring.md", "compost b") {
		t.Fatalf("short alias should not match prefix of another term, got %+v", issues)
	}
}

func hasLintIssue(issues []LintIssue, issueType, path, detailPart string) bool {
	for _, issue := range issues {
		if issue.Type == issueType && issue.Path == path && strings.Contains(issue.Detail, detailPart) {
			return true
		}
	}
	return false
}
