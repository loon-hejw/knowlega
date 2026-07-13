package compiler

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type AnalysisInput struct {
	SourceTitle   string
	SourceRel     string
	SourceText    string
	Purpose       string
	Schema        string
	Index         string
	Overview      string
	ExistingPages string
}

type Provider interface {
	Analyze(input AnalysisInput) (string, error)
	Generate(analysis string, input AnalysisInput) (string, error)
}

type MockProvider struct{}

func (MockProvider) Analyze(input AnalysisInput) (string, error) {
	sourceSlug := safeSourceSlug(input)
	conceptTitle := input.SourceTitle + " 情节主题"
	entityTitle := input.SourceTitle + " 人物线索"
	if isOAuthFixture(input.SourceText) {
		conceptTitle = "Token Validation"
		entityTitle = "AuthService"
	}
	return fmt.Sprintf(`# Analysis

Source: %s

## Entities
- %s: main entity page derived from the source.

## Concepts
- %s: concept page derived from the source.

## Connections
- Source page links to [[%s-concept]] and [[%s-entity]].

## Wiki Plan
- Create source summary.
- Create concept page.
- Create entity page.
`, input.SourceTitle, entityTitle, conceptTitle, sourceSlug, sourceSlug), nil
}

func (MockProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	titleSlug := safeSourceSlug(input)
	conceptSlug := titleSlug + "-concept"
	entitySlug := titleSlug + "-entity"
	conceptTitle := input.SourceTitle + " 情节主题"
	entityTitle := input.SourceTitle + " 人物线索"
	conceptSummary := "This page captures the central plot movement, themes, and durable concepts from the source."
	entitySummary := "This page captures the characters, places, and relationships that should remain available to later wiki synthesis."
	evidence := sourceExcerpt(input.SourceText)
	if isOAuthFixture(input.SourceText) {
		conceptSlug = "token-validation"
		entitySlug = "authservice"
		conceptTitle = "Token Validation"
		entityTitle = "AuthService"
		conceptSummary = "Token validation is the process of checking authentication tokens before accepting a session."
		entitySummary = "AuthService validates JWT claims and rejects expired sessions."
		evidence = "Token validation calls the auth service. AuthService validates JWT claims and rejects expired sessions."
	}
	today := time.Now().Format("2006-01-02")
	sourceTitle := input.SourceTitle
	if strings.TrimSpace(sourceTitle) == "" {
		sourceTitle = strings.TrimSuffix(filepath.Base(input.SourceRel), filepath.Ext(input.SourceRel))
	}
	return fmt.Sprintf(`---FILE: wiki/sources/%s.md
---
type: "source-summary"
title: "%s"
sources:
  - "%s"
created: "%s"
updated: "%s"
confidence: "EXTRACTED"
---

# %s

This source introduces [[%s]] and [[%s]].

## Evidence

%s

---FILE: wiki/concepts/%s.md
---
type: "concept"
title: "%s"
sources:
  - "%s"
created: "%s"
updated: "%s"
confidence: "EXTRACTED"
---

# %s

%s

## Source Excerpt

%s

It is connected to [[%s]] and grounded in [[%s|%s]].

## Source

See [[%s|%s]].

---FILE: wiki/entities/%s.md
---
type: "entity"
title: "%s"
sources:
  - "%s"
created: "%s"
updated: "%s"
confidence: "EXTRACTED"
---

# %s

%s

## Source Excerpt

%s

It supports [[%s]] and remains traceable to [[%s|%s]].

## Source

See [[%s|%s]].

---REVIEW: suggestion | Follow up on %s
SEARCH: %s
`, titleSlug, sourceTitle, input.SourceRel, today, today, sourceTitle, conceptSlug, entitySlug, evidence,
		conceptSlug, conceptTitle, input.SourceRel, today, today, conceptTitle, conceptSummary, evidence, entitySlug, titleSlug, sourceTitle, titleSlug, sourceTitle,
		entitySlug, entityTitle, input.SourceRel, today, today, entityTitle, entitySummary, evidence, conceptSlug, titleSlug, sourceTitle, titleSlug, sourceTitle,
		sourceTitle, reviewSearch(sourceTitle),
	), nil
}

func isOAuthFixture(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "token validation") || strings.Contains(text, "AuthService")
}

func sourceExcerpt(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	runes := []rune(text)
	if len(runes) > 2400 {
		text = string(runes[:2400]) + "..."
	}
	if text == "" {
		return "No source excerpt available."
	}
	return text
}

func reviewSearch(title string) string {
	if strings.Contains(title, "回") {
		return title + " 西游记 人物 情节 | " + title + " 西游记 主题 分析"
	}
	return title + " background research | " + title + " related concepts"
}

func safeSourceSlug(input AnalysisInput) string {
	slug := core.Slug(input.SourceTitle)
	if slug != "untitled" {
		return slug
	}
	base := strings.TrimSuffix(filepath.Base(input.SourceRel), filepath.Ext(input.SourceRel))
	parts := strings.SplitN(base, "-", 2)
	if len(parts) == 2 && len(parts[0]) == 12 {
		return core.Slug(parts[1])
	}
	return core.Slug(base)
}
