package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadStrictYAMLAndResolvePaths(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	writeConfig(t, path, `
project:
  name: demo
  path: ./project
  bootstrap:
    source: ./sources
    agent: llm
    reuse_existing: true
llm:
  api_key: secret
  model: test-model
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Path != filepath.Join(root, "project") || cfg.Project.Bootstrap.Source != source {
		t.Fatalf("paths were not resolved relative to config: %+v", cfg.Project)
	}
	if cfg.LLM.Timeout.Duration != 180*time.Second || cfg.Server.Addr != "127.0.0.1:19829" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.LLM.Protocol != "openai" || cfg.LLM.AnthropicVersion != "2023-06-01" || cfg.LLM.UserAgent != "knowledge-core/0.1" {
		t.Fatalf("llm protocol defaults not applied: %+v", cfg.LLM)
	}
	if cfg.Project.Bootstrap.RetryInitialDelay.Duration != 15*time.Second || cfg.Project.Bootstrap.RetryMaxDelay.Duration != 5*time.Minute {
		t.Fatalf("bootstrap retry defaults not applied: %+v", cfg.Project.Bootstrap)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsMissingUnknownWrongTypeAndExtraDocument(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "unknown", content: "project:\n  mystery: true\n", want: "field mystery not found"},
		{name: "wrong type", content: "llm:\n  retries: many\n", want: "cannot unmarshal"},
		{name: "duration type", content: "llm:\n  timeout: 180\n", want: "duration must be a string"},
		{name: "bootstrap retry order", content: "project:\n  bootstrap:\n    retry_initial_delay: 5m\n    retry_max_delay: 1s\n", want: "retry_max_delay must be greater"},
		{name: "output token limit", content: "llm:\n  max_output_tokens: 200000\n", want: "max_output_tokens must not exceed 131072"},
		{name: "llm protocol", content: "llm:\n  protocol: cohere\n", want: "llm.protocol must be openai or anthropic"},
		{name: "extra doc", content: "project: {}\n---\nproject: {}\n", want: "multiple YAML documents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeConfig(t, path, tt.content)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want substring %q", err, tt.want)
			}
		})
	}
	_, err := Load(filepath.Join(root, "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "config file not found") {
		t.Fatalf("missing error=%v", err)
	}
}

func TestLoadAnthropicProtocol(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	writeConfig(t, path, `
project:
  name: demo
  path: ./project
  bootstrap:
    source: ./sources
llm:
  protocol: ANTHROPIC
  base_url: https://api.anthropic.com/v1/
  api_key: secret
  model: claude-test
  user_agent: "claude-cli/2.1.205 (external, cli)"
  anthropic_version: "2024-01-01"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Protocol != "anthropic" || cfg.LLM.BaseURL != "https://api.anthropic.com/v1" || cfg.LLM.AnthropicVersion != "2024-01-01" || cfg.LLM.UserAgent != "claude-cli/2.1.205 (external, cli)" {
		t.Fatalf("anthropic config not normalized: %+v", cfg.LLM)
	}
}

func TestLoadIgnoresLegacyEnvironmentAndEnvFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KB_CORE_LLM_MODEL", "env-model")
	t.Setenv("OPENAI_API_KEY", "env-secret")
	if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte("KB_CORE_LLM_MODEL=file-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	writeConfig(t, path, `
project:
  name: demo
  path: ./project
  bootstrap:
    source: ./sources
llm:
  api_key: yaml-secret
  model: yaml-model
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "yaml-model" || cfg.LLM.APIKey != "yaml-secret" {
		t.Fatalf("legacy configuration leaked into YAML config: %+v", cfg.LLM)
	}
}

func TestEmbeddingInheritsConfiguredLLMConnection(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	writeConfig(t, path, `
project:
  name: demo
  path: ./project
  bootstrap:
    source: ./sources
llm:
  base_url: https://models.example/v1
  api_key: shared-key
embedding:
  model: embed-model
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.BaseURL != "https://models.example/v1" || cfg.Embedding.APIKey != "shared-key" {
		t.Fatalf("embedding inheritance failed: %+v", cfg.Embedding)
	}
}

func TestLoadRejectsMissingRequiredProjectFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "project: {}")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "project.name is required") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadGraphRegistryAllowlistAndSecretsFromEnvironment(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_GIT_TOKEN", "token")
	t.Setenv("TEST_GIT_WEBHOOK", "webhook-secret")
	path := filepath.Join(root, "config.yaml")
	writeConfig(t, path, `
project:
  name: demo
  path: ./project
  bootstrap:
    source: ./sources
llm:
  api_key: secret
  model: model
graph:
  enabled: true
  worker: true
  semantic_enrichment: true
  registries:
    - id: corp
      provider: gitlab
      base_url: https://git.example.com
      api_token_env: TEST_GIT_TOKEN
      webhook_secret_env: TEST_GIT_WEBHOOK
      repositories:
        - id: demo
          full_name: platform/demo
          branch: main
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Graph.CheckoutRoot != filepath.Join(root, "project", ".kbcore", "repos") || cfg.Graph.MaxParallelJobs != 2 {
		t.Fatalf("graph defaults=%+v", cfg.Graph)
	}
	registry := cfg.Graph.Registries[0]
	if registry.APIToken != "token" || registry.WebhookSecret != "webhook-secret" || registry.Directory != "corp" {
		t.Fatalf("registry=%+v", registry)
	}
	if got := registry.Repositories[0].CloneURL; got != "https://git.example.com/platform/demo.git" {
		t.Fatalf("clone_url=%q", got)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
