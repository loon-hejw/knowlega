package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmclient"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/promptbudget"
)

type MaintenanceDocument struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type MaintenanceSynthesisInput struct {
	Task        string                `json:"task"`
	Instruction string                `json:"instruction"`
	Documents   []MaintenanceDocument `json:"documents"`
}

// MaintenanceSynthesisAgent is intentionally separate from the user-facing
// knowledge query path. It is used only by explicit wiki maintenance and
// research workflows.
type MaintenanceSynthesisAgent interface {
	SynthesizeMaintenance(context.Context, MaintenanceSynthesisInput) (string, error)
}

type OpenAICompatibleMaintenanceSynthesisAgent struct {
	options llmChatOptions
}

func NewMaintenanceSynthesisAgent(cfg config.LLMConfig) (MaintenanceSynthesisAgent, error) {
	options, err := llmChatOptionsFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return OpenAICompatibleMaintenanceSynthesisAgent{options: options}, nil
}

func (a OpenAICompatibleMaintenanceSynthesisAgent) SynthesizeMaintenance(ctx context.Context, input MaintenanceSynthesisInput) (string, error) {
	if strings.TrimSpace(input.Instruction) == "" {
		return "", fmt.Errorf("maintenance synthesis instruction is required")
	}
	encoded, _ := json.MarshalIndent(input.Documents, "", "  ")
	system := `You perform an explicit maintenance task for a persistent LLM Wiki.
Use only the supplied documents and follow the requested output format. Do not act as a user-facing query agent and do not invent sources.`
	user := fmt.Sprintf("Task: %s\n\nInstruction:\n%s\n\nDocuments:\n%s", input.Task, input.Instruction, promptbudget.TrimMiddle(string(encoded), 90000))
	return chatLLM(ctx, a.options, system, user)
}

type llmChatOptions struct {
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

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func llmChatOptionsFromConfig(cfg config.LLMConfig) (llmChatOptions, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return llmChatOptions{}, fmt.Errorf("llm.api_key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return llmChatOptions{}, fmt.Errorf("llm.model is required")
	}
	return llmChatOptions{
		Protocol: cfg.Protocol, BaseURL: strings.TrimRight(cfg.BaseURL, "/"), APIKey: cfg.APIKey, Model: cfg.Model,
		UserAgent: cfg.UserAgent, AnthropicVersion: cfg.AnthropicVersion, Client: &http.Client{Timeout: cfg.Timeout.Duration},
		MaxInputChars: cfg.MaxInputChars, MaxOutputTokens: cfg.MaxOutputTokens, DisableThinking: cfg.DisableThinking,
		RetryOptions: llmretry.Options{Retries: cfg.Retries, BaseDelay: cfg.RetryBaseDelay.Duration, MaxDelay: cfg.RetryMaxDelay.Duration, MaxElapsed: cfg.OperationTimeout.Duration},
	}, nil
}

func chatLLM(ctx context.Context, options llmChatOptions, system, user string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	maxInputChars := options.MaxInputChars
	if maxInputChars <= 0 {
		maxInputChars = 120000
	}
	maxOutputTokens := options.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = 4096
	}
	system, user = promptbudget.BudgetChatInput(system, user, maxInputChars)
	retryOptions := llmretry.Normalize(options.RetryOptions)
	return llmretry.Do(ctx, retryOptions, nil, func(int) (string, bool, error) {
		return (llmclient.Client{
			Protocol: options.Protocol, BaseURL: options.BaseURL, APIKey: options.APIKey, Model: options.Model,
			UserAgent: options.UserAgent, AnthropicVersion: options.AnthropicVersion, HTTPClient: options.Client,
		}).Chat(ctx, llmclient.ChatRequest{
			System: system, User: user, MaxTokens: maxOutputTokens, Temperature: 0.2, DisableThinking: options.DisableThinking,
			Stream: strings.EqualFold(strings.TrimSpace(options.Protocol), llmclient.ProtocolOpenAI) || strings.TrimSpace(options.Protocol) == "",
		})
	})
}

func extractJSONObject(content string) string {
	content = strings.TrimSpace(content)
	if json.Valid([]byte(content)) {
		return content
	}
	// Reasoning-capable OpenAI-compatible gateways may put internal analysis
	// before the requested strict JSON object. Prefer the last complete valid
	// object, which is conventionally the model's final answer, instead of
	// slicing from the first explanatory brace to the final brace.
	best := ""
	bestEnd := -1
	for start := strings.Index(content, "{"); start >= 0; {
		if candidate, ok := balancedJSONObject(content, start); ok && json.Valid([]byte(candidate)) {
			end := start + len(candidate)
			if end > bestEnd || end == bestEnd && len(candidate) > len(best) {
				best = candidate
				bestEnd = end
			}
		}
		next := strings.Index(content[start+1:], "{")
		if next < 0 {
			break
		}
		start += next + 1
	}
	if best != "" {
		return best
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end >= start {
		return content[start : end+1]
	}
	return content
}

func balancedJSONObject(content string, start int) (string, bool) {
	if start < 0 || start >= len(content) || content[start] != '{' {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(content); index++ {
		current := content[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				inString = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[start : index+1], true
			}
			if depth < 0 {
				return "", false
			}
		}
	}
	return "", false
}
