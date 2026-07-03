package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValueLoadsLocalEnvFile(t *testing.T) {
	t.Cleanup(resetForTest)
	t.Setenv("KB_CORE_CONFIG", "")
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".env.local", []byte(`
# local LLM config
KB_CORE_LLM_BASE_URL=https://modelgate.example/v1
KB_CORE_LLM_API_KEY='local-key'
KB_CORE_LLM_MODEL="Qwen3.6-27B-FP8"
KB_CORE_EMBEDDING_MODEL= # intentionally disabled
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := Value("KB_CORE_LLM_BASE_URL"); got != "https://modelgate.example/v1" {
		t.Fatalf("base url = %q", got)
	}
	if got := Value("KB_CORE_LLM_API_KEY"); got != "local-key" {
		t.Fatalf("api key = %q", got)
	}
	if got := Value("KB_CORE_LLM_MODEL"); got != "Qwen3.6-27B-FP8" {
		t.Fatalf("model = %q", got)
	}
	if got := Value("KB_CORE_EMBEDDING_MODEL"); got != "" {
		t.Fatalf("empty embedding model should be ignored, got %q", got)
	}
}

func TestValuePrefersProcessEnvironment(t *testing.T) {
	t.Cleanup(resetForTest)
	t.Setenv("KB_CORE_CONFIG", "")
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("KB_CORE_LLM_MODEL", "env-model")
	if err := os.WriteFile(".env.local", []byte("KB_CORE_LLM_MODEL=file-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := Value("KB_CORE_LLM_MODEL"); got != "env-model" {
		t.Fatalf("model = %q", got)
	}
}

func TestValueLoadsExplicitConfigFile(t *testing.T) {
	t.Cleanup(resetForTest)
	root := t.TempDir()
	path := filepath.Join(root, "kbcore.local.env")
	t.Setenv("KB_CORE_CONFIG", path)
	if err := os.WriteFile(path, []byte("KB_CORE_PROJECT_ID=demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := Value("KB_CORE_PROJECT_ID"); got != "demo" {
		t.Fatalf("project id = %q", got)
	}
}
