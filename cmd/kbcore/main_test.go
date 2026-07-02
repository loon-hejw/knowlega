package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestUsageDocumentsQueryMockAgent(t *testing.T) {
	output := captureStdout(t, usage)
	for _, want := range []string{
		"validate-llmwiki --project PATH --source FILE_OR_DIR",
		"[--agent auto|mock|llm]",
		"query --project PATH --q QUERY",
		"[--agent auto|fallback|mock|llm]",
		"lint --project PATH [--agent structural|llm]",
		"review-wiki --project PATH",
		"[--agent auto|llm]",
		"code-import-graphify",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("usage missing %q:\n%s", want, output)
		}
	}
}

func TestRunLintLLMAgentRequiresEnv(t *testing.T) {
	t.Setenv("KB_CORE_LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("KB_CORE_LLM_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"lint", "--project", root, "--agent", "llm"})
	if err == nil || !strings.Contains(err.Error(), "lint --agent llm requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL") {
		t.Fatalf("expected missing llm env error, got %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
	}()

	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
