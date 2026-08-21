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

	maintenance, err := NewMaintenanceSynthesisAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	maintenanceAgent, ok := maintenance.(OpenAICompatibleMaintenanceSynthesisAgent)
	if !ok {
		t.Fatalf("maintenance agent type=%T", maintenance)
	}
	if maintenanceAgent.options.Protocol != "anthropic" || maintenanceAgent.options.AnthropicVersion != "2024-01-01" || maintenanceAgent.options.UserAgent != cfg.UserAgent || !maintenanceAgent.options.DisableThinking {
		t.Fatalf("maintenance protocol config=%+v", maintenanceAgent)
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
