package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestConvergeWikiArtifactsCanonicalizesMergesAndCleansProvenance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	rawA := "raw/sources/a/original/a.txt"
	rawB := "raw/sources/b/original/b.txt"
	for _, rel := range []string{rawA, rawB} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	legacy := map[string]string{
		"wiki/entity/flower-mountain.md": `---
type: entity
title: Flower Mountain
aliases:
  - Old Mountain
sources:
  - raw/sources/a/original/a.txt
  - raw/sources/invented/original/no.txt
---

# Flower Mountain

Entity evidence.
`,
		"wiki/location/flower-mountain.md": `---
type: entity
title: Flower-Fruit Mountain
aliases:
  - Huaguo Mountain
sources:
  - raw/sources/b/original/b.txt
---

# Flower-Fruit Mountain

Location evidence.
`,
	}
	for rel, content := range legacy {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"a": {OriginalPath: "a", PipelineVersion: core.SourceManifestPipelineVersion, RawPath: rawA, Files: []string{"wiki/entity/flower-mountain.md"}},
		"b": {OriginalPath: "b", PipelineVersion: core.SourceManifestPipelineVersion, RawPath: rawB, Files: []string{"wiki/location/flower-mountain.md"}},
	}}
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "reviews.md"), []byte("# Reviews\n\n## [2026-07-14] type consistency | Normalize\n\n- Status: open\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.MovedPages != 2 || result.MergedPages != 1 || result.RemovedBadSources != 1 || result.NormalizedReviews != 1 {
		t.Fatalf("result=%+v", result)
	}
	canonical := filepath.Join(root, "wiki", "entities", "flower-mountain.md")
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{rawA, rawB, "Old Mountain", "Huaguo Mountain", "Entity evidence.", "Location evidence."} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "invented") {
		t.Fatalf("invented provenance survived:\n%s", content)
	}
	for rel := range legacy {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("legacy page still exists: %s err=%v", rel, err)
		}
	}
	for _, rel := range []string{"wiki/entity", "wiki/location"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("empty legacy directory still exists: %s err=%v", rel, err)
		}
	}
	updated, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for key, entry := range updated.Sources {
		if len(entry.Files) != 1 || entry.Files[0] != "wiki/entities/flower-mountain.md" {
			t.Fatalf("manifest %s=%+v", key, entry)
		}
	}
	reviews, err := os.ReadFile(filepath.Join(root, "wiki", "reviews.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviews), "review-needed | Normalize") {
		t.Fatalf("reviews=%s", reviews)
	}
}

func TestConvergePreservesHumanPagesAndQuarantinesOnlyTrackedSourceOrphans(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "owners"}); err != nil {
		t.Fatal(err)
	}
	pageContent := func(title string) string {
		return "---\ntype: entity\ntitle: " + title + "\nsources: []\n---\n\n# " + title + "\n"
	}
	for _, rel := range []string{"wiki/entities/manual.md", "wiki/entities/orphan.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(pageContent(filepath.Base(rel))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	value := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{}, PageOwners: map[string]manifestfile.PageOwnership{
		"wiki/entities/manual.md": {ManagedBy: "manual"},
		"wiki/entities/orphan.md": {ManagedBy: "source", SourceKeys: []string{"deleted-source"}},
	}}
	if err := saveSourceManifest(root, value); err != nil {
		t.Fatal(err)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.QuarantinedPages != 1 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki/entities/manual.md")); err != nil {
		t.Fatalf("manual page was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki/entities/orphan.md")); !os.IsNotExist(err) {
		t.Fatalf("tracked source orphan still active: %v", err)
	}
}

func TestConvergeKeepsPageOwnedByRetainedSourceVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "version-owner"}); err != nil {
		t.Fatal(err)
	}
	rel := "wiki/entities/historical.md"
	content := "---\ntype: entity\ntitle: Historical\nsources: [raw/sources/old.md]\n---\n\n# Historical\n"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	value := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"source": {
			SHA256: "current", RawPath: "raw/sources/current.md",
			Versions: []manifestfile.SourceVersion{{SHA256: "old", RawPath: "raw/sources/old.md", Files: []string{rel}}},
		},
	}, PageOwners: map[string]manifestfile.PageOwnership{
		rel: {ManagedBy: "source", SourceKeys: []string{"source"}},
	}}
	if err := saveSourceManifest(root, value); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSourceManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !pageOwnedByActiveSource(rel, loaded.PageOwners[rel], loaded) {
		t.Fatalf("historical owner was not active before convergence: manifest=%+v", loaded)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.QuarantinedPages != 0 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		t.Fatalf("historical source page was removed: %v", err)
	}
}

func TestConvergeQuarantinesSupersededHistoricalSourceSummary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "summary-version"}); err != nil {
		t.Fatal(err)
	}
	current := "wiki/sources/current.md"
	historical := "wiki/sources/historical.md"
	for rel, content := range map[string]string{
		current:                  "---\ntype: source-summary\ntitle: Current\nsources: [raw/sources/current.md]\n---\n\n# Current\n",
		historical:               "---\ntype: source-summary\ntitle: Historical\nsources: [raw/sources/old.md]\n---\n\n# Historical\n",
		"raw/sources/current.md": "current",
		"raw/sources/old.md":     "old",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	value := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"source": {
			SHA256: "current", RawPath: "raw/sources/current.md", Files: []string{current},
			Versions: []manifestfile.SourceVersion{{SHA256: "old", RawPath: "raw/sources/old.md", Files: []string{historical}}},
		},
	}, PageOwners: map[string]manifestfile.PageOwnership{
		current:    {ManagedBy: "source", SourceKeys: []string{"source"}},
		historical: {ManagedBy: "source", SourceKeys: []string{"source"}},
	}}
	if err := saveSourceManifest(root, value); err != nil {
		t.Fatal(err)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.QuarantinedPages != 1 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, current)); err != nil {
		t.Fatalf("current summary removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, historical)); !os.IsNotExist(err) {
		t.Fatalf("historical summary still live: %v", err)
	}
}

func TestConvergenceGroupsShareIdentityRequiresReciprocalAliases(t *testing.T) {
	page := func(title string, aliases ...string) []convergencePage {
		values := make([]any, 0, len(aliases))
		for _, alias := range aliases {
			values = append(values, alias)
		}
		return []convergencePage{{title: title, fm: map[string]any{"aliases": values}}}
	}

	if convergenceGroupsShareIdentity(
		page("Demon King of Confusion", "Bull Demon King"),
		page("Bull Demon King"),
	) {
		t.Fatal("one-way alias must not merge distinct pages")
	}
	if !convergenceGroupsShareIdentity(
		page("Sanzang", "Xuanzang"),
		page("Xuanzang", "Sanzang"),
	) {
		t.Fatal("reciprocal aliases should merge duplicate identity pages")
	}
	if convergenceGroupsShareIdentity(
		page("Tang Sanzang", "唐三藏", "玄奘"),
		page("Xuanzang", "唐三藏", "玄奘"),
	) {
		t.Fatal("shared aliases alone must not merge pages")
	}
	if !convergenceGroupsShareIdentity(
		page("Sanzang", "Tang Sanzang"),
		page("Tang Sanzang"),
	) {
		t.Fatal("one-way alias plus full title containment should merge")
	}
	if convergenceGroupsShareIdentity(
		page("Ao Run", "Western Sea Dragon King"),
		page("Ao Shun", "Western Sea Dragon King"),
	) {
		t.Fatal("one shared alias must not merge distinct pages")
	}
	if convergenceGroupsShareIdentity(
		page("Woodcutter"),
		page("Woodcutter of Hidden Mist Mountain", "Woodcutter"),
	) {
		t.Fatal("a specific page's generic alias must not absorb the generic page")
	}
	if !convergenceGroupsShareIdentity(page("Ruyi-Jingu Bang"), page("Ruyi Jingu Bang")) {
		t.Fatal("equal normalized titles should merge")
	}
}

func TestCanonicalReviewTypeUsesBoundedTaxonomy(t *testing.T) {
	tests := map[string]string{
		"type consistency":        "review-needed",
		"missing page":            "missing-page",
		"possible duplicate":      "duplicate",
		"claim contradiction":     "contradiction",
		"stale information":       "stale-claim",
		"source evidence gap":     "source-gap",
		"ambiguous identity":      "duplicate",
		"open question":           "review-needed",
		"suggested improvement":   "review-needed",
		"requires human judgment": "review-needed",
	}
	for input, want := range tests {
		if got := canonicalReviewType(input); got != want {
			t.Errorf("canonicalReviewType(%q)=%q want %q", input, got, want)
		}
	}
}

func TestSemanticCoalescingMergesExactTitleAcrossEntityAndConcept(t *testing.T) {
	groups := map[string][]convergencePage{
		"wiki/entities/bells.md": {{path: "wiki/entities/bells.md", title: "Purple-Gold Bells", body: "entity"}},
		"wiki/concepts/bells.md": {{path: "wiki/concepts/bells.md", title: "Purple-Gold Bells", body: "concept evidence"}},
	}
	pathMap := map[string]string{
		"wiki/entities/bells.md": "wiki/entities/bells.md",
		"wiki/concepts/bells.md": "wiki/concepts/bells.md",
	}
	merged, updatedPaths := coalesceSemanticDuplicateGroups(groups, pathMap)
	if len(merged) != 1 || len(merged["wiki/entities/bells.md"]) != 2 {
		t.Fatalf("merged=%+v", merged)
	}
	if updatedPaths["wiki/concepts/bells.md"] != "wiki/entities/bells.md" {
		t.Fatalf("pathMap=%+v", updatedPaths)
	}
}

func TestConvergenceAmbiguousAliasesTracksDifferentDestinations(t *testing.T) {
	groups := map[string][]convergencePage{
		"wiki/entities/east.md": {{path: "wiki/entities/east.md", title: "East King", fm: map[string]any{"aliases": []any{"Dragon King"}}}},
		"wiki/entities/west.md": {{path: "wiki/entities/west.md", title: "West King", fm: map[string]any{"aliases": []any{"Dragon King"}}}},
	}
	ambiguous, owners := convergenceAmbiguousAliases(groups, map[string]map[string]bool{
		"wiki/entities/east.md": {"raw/sources/a.txt": true},
		"wiki/entities/west.md": {"raw/sources/b.txt": true},
	})
	if !ambiguous["dragon king"] || len(owners["dragon king"]) != 2 {
		t.Fatalf("ambiguous=%+v owners=%+v", ambiguous, owners)
	}
	if ambiguous["east king"] || ambiguous["west king"] {
		t.Fatalf("unique titles must not be ambiguous: %+v", ambiguous)
	}
}

func TestConvergePreservesAmbiguousAliasesOutsideNavigation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	manifest := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{}}
	for index, name := range []string{"east-king", "west-king"} {
		raw := "raw/sources/" + name + ".txt"
		page := "wiki/entities/" + name + ".md"
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, raw)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, raw), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		content := "---\ntype: entity\ntitle: " + name + "\naliases:\n  - Dragon King\nsources:\n  - " + raw + "\n---\n\n# " + name + "\n"
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, page)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, page), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		manifest.Sources[fmt.Sprintf("source-%d", index)] = SourceManifestEntry{RawPath: raw, Files: []string{page}}
	}
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedAmbiguousAliases != 2 {
		t.Fatalf("result=%+v", result)
	}
	for _, name := range []string{"east-king", "west-king"} {
		data, err := os.ReadFile(filepath.Join(root, "wiki", "entities", name+".md"))
		if err != nil {
			t.Fatal(err)
		}
		fm, _, err := splitGeneratedFrontmatter(string(data))
		if err != nil {
			t.Fatal(err)
		}
		if containsString(frontmatterStrings(fm["aliases"]), "Dragon King") || !containsString(frontmatterStrings(fm["ambiguous_aliases"]), "Dragon King") {
			t.Fatalf("frontmatter=%+v", fm)
		}
	}
}

func TestConvergeAddsSourceNavigationToContentPageWithoutOutlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	raw := "raw/sources/garden/original/garden.md"
	summary := "wiki/sources/garden.md"
	entity := "wiki/entities/rose-bed.md"
	files := map[string]string{
		raw: "garden evidence",
		summary: `---
type: source-summary
title: Garden Notes
sources:
  - raw/sources/garden/original/garden.md
---

# Garden Notes

See [[Rose Bed]].
`,
		entity: `---
type: entity
title: Rose Bed
sources:
  - raw/sources/garden/original/garden.md
---

# Rose Bed

The bed uses compost in spring.
`,
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"garden": {RawPath: raw, Files: []string{summary, entity}},
	}}
	if err := saveSourceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.LinkedContentPages != 1 {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entity)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[[sources/garden|Source summary]]") {
		t.Fatalf("missing deterministic source navigation:\n%s", data)
	}
}

func TestConvergeLinksStandaloneSourceSummaryToWikiIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	raw := "raw/sources/note/original/note.md"
	summary := "wiki/sources/note.md"
	files := map[string]string{
		raw: "one-off evidence",
		summary: `---
type: source-summary
title: One-off Note
sources:
  - raw/sources/note/original/note.md
---

# One-off Note

The source does not justify any durable knowledge page.
`,
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveSourceManifest(root, SourceManifest{Version: 1, Sources: map[string]SourceManifestEntry{
		"note": {RawPath: raw, Files: []string{summary}},
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := ConvergeWikiArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.LinkedSummaries != 1 {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(summary)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[[index|Wiki index]]") {
		t.Fatalf("missing fallback navigation:\n%s", data)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
