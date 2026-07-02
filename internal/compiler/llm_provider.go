package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type OpenAICompatibleProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewEnvProvider() (Provider, bool, error) {
	apiKey := firstEnv("KB_CORE_LLM_API_KEY", "OPENAI_API_KEY")
	model := firstEnv("KB_CORE_LLM_MODEL", "OPENAI_MODEL")
	if apiKey == "" || model == "" {
		return nil, false, nil
	}
	baseURL := firstEnv("KB_CORE_LLM_BASE_URL", "OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return OpenAICompatibleProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 90 * time.Second},
	}, true, nil
}

func (p OpenAICompatibleProvider) Analyze(input AnalysisInput) (string, error) {
	system := `You are maintaining a persistent LLM Wiki.
Analyze the new immutable source in the context of purpose.md, schema.md, wiki/index.md, and wiki/overview.md.
Do not generate files yet.
Return a concise markdown analysis with:
- key facts and claims
- candidate entities with aliases
- candidate concepts/themes
- pages that should be created or updated
- contradictions, duplicates, missing pages, or review items`
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

Source text:
%s`, input.SourceTitle, input.SourceRel, input.Purpose, input.Schema, input.Index, input.Overview, input.SourceText)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) Generate(analysis string, input AnalysisInput) (string, error) {
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

Source text:
%s`, analysis, input.SourceTitle, input.SourceRel, input.Purpose, input.Schema, input.Index, input.Overview, input.SourceText)
	return p.chat(system, user)
}

func (p OpenAICompatibleProvider) chat(system, user string) (string, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	body, err := json.Marshal(chatCompletionRequest{
		Model: p.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm request failed: status=%d body=%s", resp.StatusCode, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
