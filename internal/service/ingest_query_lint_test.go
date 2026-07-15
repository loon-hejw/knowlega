package service

import (
	"context"
	"fmt"
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

func TestLintSkipsAmbiguousAliasMentions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"east-king", "west-king"} {
		mustWrite(t, filepath.Join(root, "wiki", "entities", name+".md"), `---
type: entity
title: `+name+`
aliases:
  - Dragon King
---

# `+name+`

Identity page.
`)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "court.md"), `---
type: concept
title: Court
---

# Court

The Dragon King appears here without enough context to identify which one.
`)
	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/court.md", "dragon king") {
		t.Fatalf("ambiguous alias should not select an arbitrary target: %+v", issues)
	}
}

func TestLintSkipsHighFrequencyAliasMentions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "named-demon.md"), `---
type: entity
title: Named Demon
aliases:
  - demon king
---

# Named Demon

Identity page.
`)
	for index := 0; index < 9; index++ {
		name := fmt.Sprintf("note-%02d", index)
		mustWrite(t, filepath.Join(root, "wiki", "concepts", name+".md"), `---
type: concept
title: `+name+`
---

# `+name+`

A demon king appears as a generic role.
`)
	}
	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/note-00.md", "demon king") {
		t.Fatalf("high-frequency alias should not become a mandatory link: %+v", issues)
	}
}

func TestRepairKnownMentionLinksIsSafeAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "north-garden.md"), `---
type: entity
title: North Garden
aliases:
  - Northern Garden
---

# North Garden

The garden is durable knowledge.
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "route.md"), `---
type: concept
title: Route Notes
---

# Northern Garden route

Travelers cross the Northern Garden before dawn.

The literal command is `+"`visit North Garden`"+` and must remain code.
`)

	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLintIssue(issues, "missing-link", "wiki/concepts/route.md", "northern garden") {
		t.Fatalf("expected repairable missing link: %+v", issues)
	}
	linked, err := RepairKnownMentionLinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d", linked)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "concepts", "route.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "[[north-garden|Northern Garden]]") || !strings.Contains(content, "`visit North Garden`") {
		t.Fatalf("unexpected repaired page:\n%s", content)
	}
	if strings.Contains(content, "# [[") {
		t.Fatalf("heading must not be linked:\n%s", content)
	}
	linked, err = RepairKnownMentionLinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Fatalf("second repair linked=%d", linked)
	}
	issues, err = LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/route.md", "northern garden") {
		t.Fatalf("missing link survived repair: %+v", issues)
	}
}

func TestRepairKnownMentionLinksIgnoresAggregatePagesWhenScoringAliasFrequency(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "drill-head-mountain.md"), `---
type: entity
title: 钻头号山
aliases:
  - 号山
---

# 钻头号山

一处山地。
`)
	for index := 0; index < 5; index++ {
		mustWrite(t, filepath.Join(root, "wiki", "concepts", fmt.Sprintf("route-%d.md", index)), fmt.Sprintf(`---
type: concept
title: 路线%d
---

# 路线%d

行者来到号山继续前行。
`, index, index))
	}
	// These four exempt aggregate pages would push the alias above the minimum
	// frequency threshold if their transient contents were counted.
	for _, path := range []string{"index.md", "overview.md", "log.md", "reviews.md"} {
		mustWrite(t, filepath.Join(root, "wiki", path), "# Aggregate\n\n号山 navigation.\n")
	}

	linked, err := RepairKnownMentionLinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 5 {
		t.Fatalf("linked=%d, want 5", linked)
	}
	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		path := fmt.Sprintf("wiki/concepts/route-%d.md", index)
		if hasLintIssue(issues, "missing-link", path, "号山") {
			t.Fatalf("aggregate pages made alias repair unstable: %+v", issues)
		}
	}
}

func TestLintIgnoresMentionsOnlyInHeadingsAndCode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "entities", "token-service.md"), `---
type: entity
title: Token Service
---

# Token Service
`)
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "commands.md"), `---
type: concept
title: Commands
---

# Token Service commands

Run `+"`Token Service`"+` in a shell.
`)
	issues, err := LintWiki(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasLintIssue(issues, "missing-link", "wiki/concepts/commands.md", "token service") {
		t.Fatalf("protected Markdown mention must not require a link: %+v", issues)
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
