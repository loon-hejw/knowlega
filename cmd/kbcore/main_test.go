package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type flakyBootstrapProvider struct{ analyzeCalls int }

func (p *flakyBootstrapProvider) Analyze(input compiler.AnalysisInput) (string, error) {
	p.analyzeCalls++
	if p.analyzeCalls == 1 {
		return "", errors.New("temporary provider failure")
	}
	return (compiler.MockProvider{}).Analyze(input)
}

func (p *flakyBootstrapProvider) Generate(analysis string, input compiler.AnalysisInput) (string, error) {
	return (compiler.MockProvider{}).Generate(analysis, input)
}

func (p *flakyBootstrapProvider) SynthesizeOverview(input compiler.OverviewInput) (string, error) {
	return (compiler.MockProvider{}).SynthesizeOverview(input)
}

func TestUsageDocumentsLLMAgentOnly(t *testing.T) {
	output := captureStdout(t, usage)
	for _, want := range []string{
		"validate-llmwiki --project PATH --source FILE_OR_DIR",
		"[--agent llm|mock]",
		"query --project PATH --q QUERY",
		"lint --project PATH [--agent structural|llm]",
		"review-wiki --project PATH",
		"code-import-graphify",
		"source-layout migrate",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("usage missing %q:\n%s", want, output)
		}
	}
}

func TestRunLintLLMAgentRequiresYAMLConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := "project:\n  name: demo\n  path: " + root + "\n  bootstrap:\n    source: " + source + "\nllm:\n  api_key: \"\"\n  model: \"\"\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"--config", configPath, "lint", "--project", root, "--agent", "llm"})
	if err == nil || !strings.Contains(err.Error(), "llm.api_key is required") {
		t.Fatalf("expected missing llm YAML error, got %v", err)
	}
}

func TestExplicitFlagsTracksFalseBooleanOverride(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	worker := fs.Bool("worker", true, "")
	if err := fs.Parse([]string{"--worker=false"}); err != nil {
		t.Fatal(err)
	}
	if *worker {
		t.Fatal("explicit false did not override the configured true default")
	}
	if !explicitFlags(fs)["worker"] {
		t.Fatal("explicit false flag was not recorded")
	}
}

func TestExtractConfigFlagSupportsGlobalPositions(t *testing.T) {
	for _, args := range [][]string{
		{"--config", "/tmp/demo.yaml", "serve"},
		{"serve", "--config=/tmp/demo.yaml", "--worker=false"},
	} {
		path, remaining, err := extractConfigFlag(args)
		if err != nil {
			t.Fatal(err)
		}
		if path != "/tmp/demo.yaml" || len(remaining) == 0 || remaining[0] != "serve" {
			t.Fatalf("path=%q remaining=%v", path, remaining)
		}
	}
}

func TestConfiguredLLMConcurrencyCanDifferFromTaskConcurrency(t *testing.T) {
	cfg := config.Defaults()
	cfg.Project.Bootstrap.Concurrency = 4
	if got := configuredLLMConcurrency(cfg); got != 4 {
		t.Fatalf("inherited concurrency=%d", got)
	}
	cfg.LLM.Concurrency = 2
	if got := configuredLLMConcurrency(cfg); got != 2 {
		t.Fatalf("configured concurrency=%d", got)
	}
}

func TestRunServeBootstrapRetriesSourceInsideSingleAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\n西游记来源"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Project.Name = "demo"
	cfg.Project.Path = root
	cfg.Project.Bootstrap.Source = source
	cfg.Project.Bootstrap.RetryInitialDelay = config.Duration{Duration: time.Millisecond}
	cfg.Project.Bootstrap.RetryMaxDelay = config.Duration{Duration: 2 * time.Millisecond}
	tracker := service.NewBootstrapTracker(root, 1)
	provider := &flakyBootstrapProvider{}
	runServeBootstrap(context.Background(), serveBootstrapOptions{
		Config: cfg, ProjectPath: root, Provider: provider, Tracker: tracker,
	})
	status := tracker.Snapshot()
	if status.Status != "succeeded" || status.Attempt != 1 || provider.analyzeCalls != 2 {
		t.Fatalf("status=%+v analyze_calls=%d", status, provider.analyzeCalls)
	}
}

type exhaustedBootstrapProvider struct{ calls int }

func (p *exhaustedBootstrapProvider) Analyze(compiler.AnalysisInput) (string, error) {
	p.calls++
	return "", errors.New("provider unavailable")
}

func (p *exhaustedBootstrapProvider) Generate(string, compiler.AnalysisInput) (string, error) {
	p.calls++
	return "", errors.New("provider unavailable")
}

func TestRunServeBootstrapStopsWhenSourceAttemptsExhausted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Project.Name = "demo"
	cfg.Project.Path = root
	cfg.Project.Bootstrap.Source = source
	cfg.Project.Bootstrap.MaxTaskAttempts = 1
	tracker := service.NewBootstrapTracker(root, 1)
	provider := &exhaustedBootstrapProvider{}
	runServeBootstrap(context.Background(), serveBootstrapOptions{
		Config: cfg, ProjectPath: root, Provider: provider, Tracker: tracker,
	})
	status := tracker.Snapshot()
	if status.Status != "failed" || status.Attempt != 1 || provider.calls != 1 || !strings.Contains(status.Error, "failed after 1 attempts") {
		t.Fatalf("status=%+v calls=%d", status, provider.calls)
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

func TestAggregateWikiPathsAlwaysIncludesDurableReviews(t *testing.T) {
	paths := aggregateWikiPaths("wiki/entities/a.md")
	for _, path := range paths {
		if path == "wiki/reviews.md" {
			return
		}
	}
	t.Fatalf("paths=%v", paths)
}
