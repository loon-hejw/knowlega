package compiler

import (
	"strings"
	"testing"
)

func TestParseBlocksNormalizesDuplicateFrontmatterKeys(t *testing.T) {
	parsed, err := ParseBlocks("---FILE: wiki/entities/a.md\n---\ntype: entity\ntitle: A\nconfidence: EXTRACTED\nconfidence: EXTRACTED\nsources: [raw/sources/a.md]\nsources: [raw/sources/b.md]\n---\n\n# A\n")
	if err != nil {
		t.Fatal(err)
	}
	fm, err := parseGeneratedFrontmatter(parsed.Files[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(frontmatterStrings(fm["sources"])); got != 2 {
		t.Fatalf("sources=%v", fm["sources"])
	}
	if strings.Count(parsed.Files[0].Content, "confidence:") != 1 {
		t.Fatalf("frontmatter not normalized:\n%s", parsed.Files[0].Content)
	}
	_, err = ParseBlocks("---FILE: wiki/entities/a.md\n---\ntype: entity\ntitle: A\nconfidence: EXTRACTED\nconfidence: INFERRED\nsources: [raw/sources/a.md]\n---\n\n# A\n")
	if err == nil {
		t.Fatal("expected conflicting duplicate key error")
	}
}

func TestParseBlocks(t *testing.T) {
	parsed, err := ParseBlocks(`---FILE: wiki/concepts/token-validation.md
---
type: "concept"
title: "Token Validation"
---

# Token Validation

---REVIEW: suggestion | Follow up
SEARCH: token validation
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("files=%d", len(parsed.Files))
	}
	if parsed.Files[0].Path != "wiki/concepts/token-validation.md" {
		t.Fatalf("path=%s", parsed.Files[0].Path)
	}
	if len(parsed.Reviews) != 1 {
		t.Fatalf("reviews=%d", len(parsed.Reviews))
	}
}

func TestParseBlocksRejectsUnsafePath(t *testing.T) {
	_, err := ParseBlocks(`---FILE: ../outside.md
---
title: "x"
---
`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBlocksRequiresFrontmatter(t *testing.T) {
	_, err := ParseBlocks(`---FILE: wiki/concepts/x.md
# Missing Frontmatter
`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCanonicalizeGeneratedPathsMovesSourceSummaryToSources(t *testing.T) {
	parsed, err := ParseBlocks(`---FILE: wiki/source-summary/chapter-001.md
---
type: "source-summary"
title: "Chapter 001"
sources:
  - "raw/sources/chapter-001.txt"
---

# Chapter 001
`)
	if err != nil {
		t.Fatal(err)
	}
	parsed = canonicalizeGeneratedPaths(parsed)
	if parsed.Files[0].Path != "wiki/sources/chapter-001.md" {
		t.Fatalf("path=%s", parsed.Files[0].Path)
	}
}

func TestDowngradeMissingWikilinksKeepsKnownLinksAndCreatesPlainText(t *testing.T) {
	parsed := ParsedBlocks{Files: []FileBlock{
		{Path: "wiki/entities/known.md", Content: "---\ntype: entity\ntitle: Known\nsources:\n  - raw/a.txt\n---\n\n# Known\n"},
		{Path: "wiki/sources/a.md", Content: "---\ntype: source-summary\ntitle: A\nsources:\n  - raw/a.txt\n---\n\nSee [[Known]], [[missing-page]], and [[other-missing|display label]].\n"},
	}}
	missing := downgradeMissingWikilinks(t.TempDir(), &parsed)
	if len(missing) != 2 {
		t.Fatalf("missing=%v", missing)
	}
	body := parsed.Files[1].Content
	if !strings.Contains(body, "[[Known]]") || !strings.Contains(body, "missing-page") || !strings.Contains(body, "display label") || strings.Contains(body, "[[missing-page]]") || strings.Contains(body, "[[other-missing") {
		t.Fatalf("body=%s", body)
	}
}

func TestDowngradeMissingReviewWikilinksKeepsGeneratedTargets(t *testing.T) {
	parsed := ParsedBlocks{
		Files:   []FileBlock{{Path: "wiki/entities/known.md", Content: "---\ntype: entity\ntitle: Known\nsources: [raw/a.txt]\n---\n\n# Known\n"}},
		Reviews: []ReviewBlock{{Type: "missing-page", Title: "Review", Body: "Compare [[Known]] with [[Missing|missing evidence]]."}},
	}
	missing := downgradeMissingReviewWikilinks(t.TempDir(), &parsed)
	if len(missing) != 1 || missing[0] != "Missing" {
		t.Fatalf("missing=%v", missing)
	}
	body := parsed.Reviews[0].Body
	if !strings.Contains(body, "[[Known]]") || !strings.Contains(body, "missing evidence") || strings.Contains(body, "[[Missing") {
		t.Fatalf("review body=%s", body)
	}
}
