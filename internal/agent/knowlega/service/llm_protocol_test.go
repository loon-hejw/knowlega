package service

import (
	"testing"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
)

func TestLLMAgentConstructorsPropagateAnthropicProtocol(t *testing.T) {
	cfg := config.Defaults().LLM
	cfg.Protocol = "anthropic"
	cfg.BaseURL = "https://anthropic.example/v1"
	cfg.APIKey = "test-key"
	cfg.Model = "claude-test"
	cfg.UserAgent = "claude-cli/2.1.205 (external, cli)"
	cfg.AnthropicVersion = "2024-01-01"

	query, err := NewQueryAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	queryAgent, ok := query.(OpenAICompatibleQueryAgent)
	if !ok {
		t.Fatalf("query agent type=%T", query)
	}
	if queryAgent.Protocol != "anthropic" || queryAgent.AnthropicVersion != "2024-01-01" || queryAgent.UserAgent != cfg.UserAgent || !queryAgent.DisableThinking {
		t.Fatalf("query protocol config=%+v", queryAgent)
	}

	review, err := NewWikiReviewAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reviewAgent, ok := review.(OpenAICompatibleWikiReviewAgent)
	if !ok {
		t.Fatalf("review agent type=%T", review)
	}
	if reviewAgent.Protocol != "anthropic" || reviewAgent.AnthropicVersion != "2024-01-01" || reviewAgent.UserAgent != cfg.UserAgent || !reviewAgent.DisableThinking {
		t.Fatalf("review protocol config=%+v", reviewAgent)
	}
}
