package compiler

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmclient"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/promptbudget"
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
			Retries:    cfg.Retries,
			BaseDelay:  cfg.RetryBaseDelay.Duration,
			MaxDelay:   cfg.RetryMaxDelay.Duration,
			MaxElapsed: cfg.OperationTimeout.Duration,
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
- an explicit Wiki Plan using exactly: - LABEL | wiki/path.md | durability=recurring|central|link-target | evidence=...
- LABEL must be SOURCE SUMMARY, UPDATE EXISTING, CREATE NEW, or REVIEW ONLY; durability and evidence are required for CREATE NEW
- never plan more CREATE NEW targets than the hard remaining new-page budget`
	system += `
- Existing pages were selected deterministically from known titles and aliases mentioned by the source. For each supplied existing page, use UPDATE EXISTING when the source materially adds evidence, or REVIEW ONLY with a reason when it should remain unchanged. Never silently omit a materially changed canonical page.`
	user := fmt.Sprintf(`Source title: %s
Source path: %s

%s

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
%s`, input.SourceTitle, input.SourceRel, generationPolicyText(input.GenerationPolicy), input.Purpose, input.Schema, input.Index, input.Overview, input.ExistingPages, input.SourceText)
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
- Use only these canonical directories: source-summary -> wiki/sources/, entity (including places and objects) -> wiki/entities/, concept -> wiki/concepts/, synthesis -> wiki/syntheses/. All paths end in .md.
- Never write wiki/index.md, wiki/overview.md, wiki/log.md, wiki/reviews.md, or duplicate index/overview pages under syntheses; the compiler maintains those aggregates separately.
- Obey the numeric generation budget supplied with the source. Prefer a small number of durable canonical pages over one-off pages for every minor name, object, event, or location.
- Review type must be exactly one of: contradiction, duplicate, missing-page, stale-claim, source-gap, review-needed.
- Every file must have YAML frontmatter.
- Emit every YAML frontmatter key exactly once. Never repeat type, title, sources, aliases, or confidence.
- Keep source provenance in sources.
- The sources list must contain the current supplied raw source. Never invent, guess, copy from prose, or synthesize raw/sources paths; the compiler owns provenance unioning.
- The first ---FILE block MUST be the current source's single source-summary under wiki/sources/. Emit it before every entity, concept, synthesis, or review block so it cannot be lost if output is truncated.
- When an existing page is supplied, merge the new evidence into it. Preserve prior claims, sections, aliases, and sources unless the new source explicitly supersedes them.
- Treat each supplied ---EXISTING PAGE: wiki/path.md path as the canonical identity owner. If a proposed title or alias refers to that identity, update that exact path and do not create a competing page.
- Never return two pages whose titles or aliases identify the same entity or concept; fold their evidence into one canonical page.
- Existing-page sources must be unioned with the current source; never replace previous provenance.
- Always include exactly one source-summary page under wiki/sources/ for the current source.
- Prefer updating durable entity/concept/synthesis pages in addition to the source-summary when the source clearly belongs there.
- Use aliases in frontmatter when useful for entity names, book chapter titles, or common user wording.
- Aliases must be genuine alternate names or spellings for that exact page. Never use topical keywords, generic words, related entity names, or broad search terms as aliases.
- If you use a label, alias, or alternate wording as a [[wikilink]], add that wording to the target page aliases or create/update a target page for it.
- Do not include prose outside ---FILE or ---REVIEW blocks.`
	user := fmt.Sprintf(`Analysis:
%s

Source title: %s
Source path: %s

%s

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
%s`, analysis, input.SourceTitle, input.SourceRel, generationPolicyText(input.GenerationPolicy), input.Purpose, input.Schema, input.Index, input.Overview, input.ExistingPages, input.SourceText)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) SynthesizeOverview(input OverviewInput) (string, error) {
	system := `You maintain the global overview of a persistent LLM Wiki.
Return the complete wiki/overview.md as Markdown, without code fences.
The page may begin with valid YAML frontmatter, but its body must begin with one H1 heading. Never return commentary outside the page.
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
%s

Open review evidence (authoritative; never claim there are no contradictions while contradiction or stale-claim items are open):
%s

Validation repair instruction, if any:
%s`, promptbudget.TrimEnd(input.Purpose, 4000), promptbudget.TrimEnd(input.Schema, 4000), promptbudget.TrimMiddle(input.Index, 8000), promptbudget.TrimMiddle(input.CurrentOverview, 6000), promptbudget.TrimMiddle(input.PageExcerpts, 40000), promptbudget.TrimMiddle(input.OpenReviews, 12000), input.ValidationError)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) chat(system, user string) (string, error) {
	retryOpts := llmretry.Normalize(p.RetryOptions)
	ctx := context.Background()
	if retryOpts.MaxElapsed > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, retryOpts.MaxElapsed)
		defer cancel()
		retryOpts.MaxElapsed = 0
	}
	return llmretry.Do(ctx, retryOpts, nil, func(attempt int) (string, bool, error) {
		return p.chatOnce(ctx, system, user)
	})
}

func (p OpenAICompatibleProvider) chatOnce(ctx context.Context, system, user string) (string, bool, error) {
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
	}).Chat(ctx, llmclient.ChatRequest{
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
