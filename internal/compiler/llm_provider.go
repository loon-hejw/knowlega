package compiler

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/llmclient"
	"github.com/hejw/knowledge-core/internal/llmretry"
	"github.com/hejw/knowledge-core/internal/promptbudget"
)

type OpenAICompatibleProvider struct {
	Protocol         string
	BaseURL          string
	APIKey           string
	Model            string
	UserAgent        string
	AnthropicVersion string
	Client           *http.Client
	MaxInputChars    int
	MaxOutputTokens  int
	DisableThinking  bool
	RetryOptions     llmretry.Options
}

const (
	defaultLLMMaxInputChars   = 120000
	defaultLLMMaxOutputTokens = 4096
)

func NewProvider(cfg config.LLMConfig) (Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("llm.api_key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm.model is required")
	}
	return OpenAICompatibleProvider{
		Protocol:         cfg.Protocol,
		BaseURL:          strings.TrimRight(cfg.BaseURL, "/"),
		APIKey:           cfg.APIKey,
		Model:            cfg.Model,
		UserAgent:        cfg.UserAgent,
		AnthropicVersion: cfg.AnthropicVersion,
		Client:           &http.Client{Timeout: cfg.Timeout.Duration},
		MaxInputChars:    cfg.MaxInputChars,
		MaxOutputTokens:  cfg.MaxOutputTokens,
		DisableThinking:  cfg.DisableThinking,
		RetryOptions: llmretry.Options{
			Retries:   cfg.Retries,
			BaseDelay: cfg.RetryBaseDelay.Duration,
			MaxDelay:  cfg.RetryMaxDelay.Duration,
		},
	}, nil
}

func (p OpenAICompatibleProvider) Analyze(input AnalysisInput) (string, error) {
	input = budgetAnalysisInput(input)
	system := `You are maintaining a persistent LLM Wiki.
Analyze the new immutable source in the context of purpose.md, schema.md, wiki/index.md, and wiki/overview.md.
Do not generate files yet.
Return a concise markdown analysis with:
- key facts and claims
- candidate entities with aliases
- candidate concepts/themes
- pages that should be created or updated
- contradictions, duplicates, missing pages, or review items
- an explicit Wiki Plan listing every proposed target as a project-relative wiki/*.md path`
	user := fmt.Sprintf(`Source title: %s
Source path: %s

Purpose:
%s

Schema:
%s

Current index:
%s

Current overview:
%s

Existing pages selected for merge:
%s

Source text:
%s`, input.SourceTitle, input.SourceRel, input.Purpose, input.Schema, input.Index, input.Overview, input.ExistingPages, input.SourceText)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	input = budgetAnalysisInput(input)
	analysis = promptbudget.TrimMiddle(analysis, 16000)
	system := `You are generating updates for a persistent LLM Wiki.
Return only strict block output. Use one or more file blocks:
---FILE: wiki/path.md
---
type: "source-summary|entity|concept|synthesis"
title: "..."
sources:
  - "raw/sources/..."
confidence: "EXTRACTED|INFERRED|AMBIGUOUS"
---

# Title

Body with [[wikilinks]].

Optional review blocks use:
---REVIEW: type | title
body

Rules:
- All file paths must be under wiki/ and end in .md.
- Every file must have YAML frontmatter.
- Keep source provenance in sources.
- When an existing page is supplied, merge the new evidence into it. Preserve prior claims, sections, aliases, and sources unless the new source explicitly supersedes them.
- Existing-page sources must be unioned with the current source; never replace previous provenance.
- Always include exactly one source-summary page under wiki/sources/ for the current source.
- Prefer updating durable entity/concept/synthesis pages in addition to the source-summary when the source clearly belongs there.
- Use aliases in frontmatter when useful for entity names, book chapter titles, or common user wording.
- If you use a label, alias, or alternate wording as a [[wikilink]], add that wording to the target page aliases or create/update a target page for it.
- Do not include prose outside ---FILE or ---REVIEW blocks.`
	user := fmt.Sprintf(`Analysis:
%s

Source title: %s
Source path: %s

Purpose:
%s

Schema:
%s

Current index:
%s

Current overview:
%s

Existing pages that must be merged rather than replaced:
%s

Source text:
%s`, analysis, input.SourceTitle, input.SourceRel, input.Purpose, input.Schema, input.Index, input.Overview, input.ExistingPages, input.SourceText)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	system := `You maintain the global overview of a persistent LLM Wiki.
Return the complete wiki/overview.md as Markdown, without code fences.
This is a current high-level synthesis, not an append-only activity list.
Include the wiki's purpose, major entities and concepts, important relationships, evolving themes or claims, contradictions/gaps, and concise [[wikilink]] navigation.
Preserve still-valid insights from the previous overview and incorporate the current page evidence. Do not invent claims unsupported by the supplied pages.`
	user := fmt.Sprintf(`Purpose:
%s

Schema:
%s

Current index:
%s

Previous overview:
%s

Current wiki page excerpts:
%s`, promptbudget.TrimEnd(input.Purpose, 8000), promptbudget.TrimEnd(input.Schema, 8000), promptbudget.TrimMiddle(input.Index, 24000), promptbudget.TrimMiddle(input.CurrentOverview, 16000), promptbudget.TrimMiddle(input.PageExcerpts, 90000))
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) chat(system, user string) (string, error) {
	retryOpts := llmretry.Normalize(p.RetryOptions)
	return llmretry.Do(nil, retryOpts, nil, func(attempt int) (string, bool, error) {
		return p.chatOnce(system, user)
	})
}

func (p OpenAICompatibleProvider) chatOnce(system, user string) (string, bool, error) {
	maxInputChars := p.MaxInputChars
	if maxInputChars <= 0 {
		maxInputChars = defaultLLMMaxInputChars
	}
	maxOutputTokens := p.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultLLMMaxOutputTokens
	}
	system, user = promptbudget.BudgetChatInput(system, user, maxInputChars)
	return (llmclient.Client{
		Protocol:         p.Protocol,
		BaseURL:          p.BaseURL,
		APIKey:           p.APIKey,
		Model:            p.Model,
		UserAgent:        p.UserAgent,
		AnthropicVersion: p.AnthropicVersion,
		HTTPClient:       p.Client,
	}).Chat(context.Background(), llmclient.ChatRequest{
		System: system, User: user, MaxTokens: maxOutputTokens,
		Temperature: 0.2, DisableThinking: p.DisableThinking,
	})
}

type chatCompletionRequest struct {
	Model              string         `json:"model"`
	Messages           []chatMessage  `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func budgetAnalysisInput(input AnalysisInput) AnalysisInput {
	input.Purpose = promptbudget.TrimEnd(input.Purpose, 8000)
	input.Schema = promptbudget.TrimEnd(input.Schema, 8000)
	input.Index = promptbudget.TrimMiddle(input.Index, 24000)
	input.Overview = promptbudget.TrimMiddle(input.Overview, 16000)
	input.ExistingPages = promptbudget.TrimMiddle(input.ExistingPages, 32000)
	input.SourceText = promptbudget.TrimMiddle(input.SourceText, 64000)
	return input
}
