package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const baseConfig = "database:\n  url: postgres://example/qm\nauth:\n  grpc_internal_token: test-internal-token\nqm:\n  org_id: acme\n"

func TestDefaultUsesNonLegacyHTTPPort(t *testing.T) {
	if got := Default().Server.HTTPAddr; got != ":18083" {
		t.Fatalf("default HTTP address = %q, want :18083", got)
	}
}

func TestProjectFileKnowledgeProcessingDefaultsOnAndCanBeDisabled(t *testing.T) {
	if !Default().Knowledge.AutoProcessProjectFiles {
		t.Fatal("project file knowledge processing must default on")
	}
	loaded, err := Load(writeConfig(t, baseConfig+"knowledge:\n  auto_process_project_files: false\n  worker: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Knowledge.AutoProcessProjectFiles || loaded.Knowledge.Worker {
		t.Fatalf("knowledge config=%+v", loaded.Knowledge)
	}
}

func TestSharedYAMLModelConfig(t *testing.T) {
	loaded, err := Load(writeConfig(t, baseConfig+`  models:
    default_harness: pi
    request:
      timeout_seconds: 90
      operation_timeout_seconds: 380
      retries: 3
    providers:
      - id: modelgate
        protocol: openai
        base_url: https://models.example/v1/
        api_key: secret
        models:
          - id: qwen-test
            name: Qwen Test
            context_window: 128000
            max_tokens: 8192
    harnesses:
      - id: pi
        provider: modelgate
        model_ids: [qwen-test]
        default_model: qwen-test
  slack:
    bot_token: xoxb-test
    app_token: xapp-test
  oauth:
    clients:
      - provider: github
        client_id: client
        client_secret: secret
`))
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := loaded.QM.Models.ProviderForModel("qwen-test")
	if !ok || provider.ID != "modelgate" || provider.Protocol != "openai" {
		t.Fatalf("provider=%+v ok=%v", provider, ok)
	}
	if loaded.QM.Models.Request.TimeoutSeconds != 90 || loaded.QM.Models.Request.OperationTimeoutSeconds != 380 || len(loaded.QM.OAuth.Clients) != 1 {
		t.Fatalf("loaded config=%+v", loaded.QM)
	}
}

func TestKnowledgeQueryConfigIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, baseConfig+"knowledge:\n  query:\n    total_timeout_seconds: 180\n"))
	if err == nil || !strings.Contains(err.Error(), "knowledge.query is obsolete") {
		t.Fatalf("err=%v", err)
	}
}

func TestLegacyKnowledgeLLMMigratesToSharedModels(t *testing.T) {
	loaded, err := Load(writeConfig(t, baseConfig+`knowledge:
  llm:
    base_url: https://legacy.example/v1/
    api_key: legacy-secret
    model: legacy-model
    timeout_seconds: 90
`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.QM.Models.DefaultHarness != "pi" || loaded.QM.Models.DefaultModel() != "legacy-model" {
		t.Fatalf("models=%+v", loaded.QM.Models)
	}
	provider, ok := loaded.QM.Models.ProviderForModel("legacy-model")
	if !ok || provider.ID != "legacy-knowledge" || provider.Protocol != "openai" || provider.BaseURL != "https://legacy.example/v1" || provider.APIKey != "legacy-secret" {
		t.Fatalf("provider=%+v ok=%v", provider, ok)
	}
	if loaded.QM.Models.Request.TimeoutSeconds != 90 || loaded.QM.Models.Request.OperationTimeoutSeconds != 280 {
		t.Fatalf("request=%+v", loaded.QM.Models.Request)
	}
	if loaded.Knowledge.LLM != (legacyKnowledgeLLMConfig{}) {
		t.Fatalf("legacy config remains visible: %+v", loaded.Knowledge.LLM)
	}
}

func TestLegacyKnowledgeLLMCompatibility(t *testing.T) {
	t.Run("empty provider block is ignored", func(t *testing.T) {
		loaded, err := Load(writeConfig(t, baseConfig+`knowledge:
  llm:
    base_url: ""
    api_key: ""
    model: ""
    timeout_seconds: 60
`))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.QM.Models.DefaultHarness != "" || len(loaded.QM.Models.Providers) != 0 {
			t.Fatalf("models=%+v", loaded.QM.Models)
		}
	})

	t.Run("new model config wins", func(t *testing.T) {
		loaded, err := Load(writeConfig(t, baseConfig+`  models:
    default_harness: mock
    request:
      timeout_seconds: 30
      operation_timeout_seconds: 30
      retries: 0
    providers:
      - id: mock
        protocol: mock
        models: [{id: current-model}]
    harnesses:
      - id: mock
        provider: mock
        model_ids: [current-model]
        default_model: current-model
knowledge:
  llm:
    base_url: https://legacy.example/v1
    api_key: legacy-secret
    model: legacy-model
    timeout_seconds: 90
`))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.QM.Models.DefaultHarness != "mock" || loaded.QM.Models.DefaultModel() != "current-model" || loaded.QM.Models.Request.TimeoutSeconds != 30 {
			t.Fatalf("models=%+v", loaded.QM.Models)
		}
		if _, ok := loaded.QM.Models.ProviderForModel("legacy-model"); ok {
			t.Fatal("legacy model unexpectedly replaced qm.models")
		}
	})

	t.Run("shared request settings win", func(t *testing.T) {
		loaded, err := Load(writeConfig(t, baseConfig+`  models:
    request:
      timeout_seconds: 45
      retries: 0
knowledge:
  llm:
    base_url: https://legacy.example/v1
    api_key: legacy-secret
    model: legacy-model
    timeout_seconds: 90
`))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.QM.Models.Request.TimeoutSeconds != 45 || loaded.QM.Models.Request.Retries != 0 || loaded.QM.Models.Request.OperationTimeoutSeconds != 0 {
			t.Fatalf("request=%+v", loaded.QM.Models.Request)
		}
	})

	t.Run("partial legacy settings produce normal model validation", func(t *testing.T) {
		_, err := Load(writeConfig(t, baseConfig+"knowledge:\n  llm:\n    model: old\n"))
		if err == nil || strings.Contains(err.Error(), "deprecated") || !strings.Contains(err.Error(), "base_url and api_key") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestSharedYAMLConfigRejectsLegacyAndInvalidSettings(t *testing.T) {
	tests := []struct {
		name, extra, want string
	}{
		{"legacy oauth", baseConfig + "  oauth_catalog_enabled: true\n", "deprecated"},
		{"legacy base model", baseConfig + "  models:\n    base_model: missing\n", "no longer supported"},
		{"unknown default harness", baseConfig + "  models:\n    default_harness: missing\n    providers: []\n", "not declared"},
		{"partial slack", baseConfig + "  slack:\n    bot_token: xoxb-only\n", "configured together"},
		{"unknown oauth", baseConfig + "  oauth:\n    clients:\n      - provider: unknown\n        client_id: id\n        client_secret: secret\n", "unknown provider"},
		{"retry budget too short", baseConfig + "  models:\n    default_harness: mock\n    request:\n      timeout_seconds: 60\n      operation_timeout_seconds: 60\n      retries: 2\n    providers:\n      - id: mock\n        protocol: mock\n        models: [{id: mock}]\n    harnesses:\n      - id: mock\n        provider: mock\n        model_ids: [mock]\n        default_model: mock\n", "must be at least 190"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contents := test.extra
			if !strings.Contains(contents, "database:") {
				contents = baseConfig + contents
			}
			_, err := Load(writeConfig(t, contents))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestModelSlackAndOAuthIgnoreEnvironmentVariables(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-load")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-must-not-load")
	t.Setenv("GITHUB_CLIENT_ID", "must-not-load")
	loaded, err := Load(writeConfig(t, baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.QM.Models.Providers) != 0 || loaded.QM.Slack.BotToken != "" || len(loaded.QM.OAuth.Clients) != 0 {
		t.Fatalf("onboarding configuration unexpectedly loaded from environment: %+v", loaded.QM)
	}
}

func TestSharedYAMLConfigRequiresProtectedRuntimeGRPC(t *testing.T) {
	contents := strings.Replace(baseConfig, "auth:\n  grpc_internal_token: test-internal-token\n", "", 1) + `  models:
    default_harness: mock
    providers:
      - id: mock
        protocol: mock
        models:
          - id: mock
    harnesses:
      - id: mock
        provider: mock
        model_ids: [mock]
        default_model: mock
`
	_, err := Load(writeConfig(t, contents))
	if err == nil || !strings.Contains(err.Error(), "auth.grpc_internal_token") {
		t.Fatalf("err=%v", err)
	}
}

func TestSharedYAMLHarnessValidation(t *testing.T) {
	modelConfig := `  models:
    default_harness: %s
    providers:
      - id: openai-gateway
        protocol: openai
        base_url: https://models.example/v1
        api_key: secret
        models: [{id: code-model}]
      - id: anthropic-gateway
        protocol: anthropic
        base_url: https://models.example/anthropic
        api_key: secret
        models: [{id: reasoning-model}]
    harnesses:
%s`
	tests := []struct{ name, defaultHarness, harnesses, want string }{
		{"valid mappings", "pi", "      - {id: pi, provider: openai-gateway, model_ids: [code-model], default_model: code-model}\n      - {id: claude, provider: anthropic-gateway, model_ids: [reasoning-model], default_model: reasoning-model}\n", ""},
		{"protocol mismatch", "codex", "      - {id: codex, provider: anthropic-gateway, model_ids: [reasoning-model], default_model: reasoning-model}\n", "incompatible"},
		{"cross provider model", "pi", "      - {id: pi, provider: openai-gateway, model_ids: [reasoning-model], default_model: reasoning-model}\n", "belongs to provider"},
		{"default missing from list", "pi", "      - {id: pi, provider: openai-gateway, model_ids: [code-model], default_model: reasoning-model}\n", "must be listed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, baseConfig+fmt.Sprintf(modelConfig, test.defaultHarness, test.harnesses)))
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestHarnessRuntimeOptionsValidation(t *testing.T) {
	modelConfig := `  models:
    default_harness: pi
    providers:
      - id: openai-gateway
        protocol: openai
        base_url: https://models.example/v1
        api_key: secret
        models: [{id: code-model}, {id: judge-model}]
    harnesses:
      - id: pi
        provider: openai-gateway
        model_ids: [code-model, judge-model]
        default_model: code-model
        runtime:
%s`
	tests := []struct{ name, runtime, want string }{
		{"valid pi", "          detect_model: judge-model\n          title_model: judge-model\n          judge_model: judge-model\n          capture_requests: true\n          system_cache_split: true\n          turn_wall_clock_seconds: 900\n", ""},
		{"unknown auxiliary model", "          judge_model: missing\n", "must be listed"},
		{"pi binary", "          binary_path: /usr/local/bin/pi\n", "not supported by pi"},
		{"negative timeout", "          exec_timeout_seconds: -1\n", "must be non-negative"},
		{"short ceiling", "          exec_timeout_seconds: 20\n          exec_timeout_ceiling_seconds: 10\n", "must not be shorter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, baseConfig+fmt.Sprintf(modelConfig, test.runtime)))
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("err=%v want=%q", err, test.want)
			}
		})
	}
}

func TestWorkerPoolDefaultsAndOverrides(t *testing.T) {
	loaded, err := Load(writeConfig(t, baseConfig+`workers:
  reap_interval_seconds: 15
  pools:
    turn:
      concurrency: 12
    deploy:
      concurrency: 3
      lease_ttl_seconds: 120
      heartbeat_interval_seconds: 20
      poll_interval_millis: 500
      max_claim_backoff_millis: 10000
`))
	if err != nil {
		t.Fatal(err)
	}
	turn, ok := loaded.Workers.Pool("turn")
	if !ok || turn.Concurrency != 12 || turn.LeaseTTLSeconds != 30 || turn.HeartbeatIntervalSeconds != 10 {
		t.Fatalf("turn worker pool=%+v ok=%v", turn, ok)
	}
	deploy, ok := loaded.Workers.Pool("deploy")
	if !ok || deploy.Concurrency != 3 || deploy.LeaseTTLSeconds != 120 || deploy.HeartbeatIntervalSeconds != 20 {
		t.Fatalf("deploy worker pool=%+v ok=%v", deploy, ok)
	}
	if loaded.Workers.ReapIntervalSeconds != 15 || len(loaded.Workers.Pools) != 8 {
		t.Fatalf("workers=%+v", loaded.Workers)
	}
}

func TestWorkerPoolValidation(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown pool", "    unknown:\n      concurrency: 1\n", "unknown pool"},
		{"invalid heartbeat", "    deploy:\n      lease_ttl_seconds: 5\n      heartbeat_interval_seconds: 5\n", "shorter than its lease ttl"},
		{"invalid polling", "    oauth:\n      poll_interval_millis: 5\n", "must be at least 10"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, baseConfig+"workers:\n  pools:\n"+test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestConnectorSecretKeyValidation(t *testing.T) {
	withAuth := func(extra string) string {
		return strings.Replace(baseConfig, "  grpc_internal_token: test-internal-token\n", "  grpc_internal_token: test-internal-token\n"+extra, 1)
	}
	_, err := Load(writeConfig(t, withAuth("  connector_secret_key: short\n")))
	if err == nil || !strings.Contains(err.Error(), "at least 32 characters") {
		t.Fatalf("err=%v", err)
	}
	loaded, err := Load(writeConfig(t, withAuth("  connector_secret_key: 0123456789abcdef0123456789abcdef\n  previous_connector_secret_keys: [abcdef0123456789abcdef0123456789]\n")))
	if err != nil || len(loaded.Auth.PreviousConnectorSecrets) != 1 {
		t.Fatalf("config=%+v err=%v", loaded.Auth, err)
	}
}
