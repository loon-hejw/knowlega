package compiler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type transientOnceSourceProvider struct {
	calls int
	mock  MockProvider
}

type transientConflictWaveProvider struct {
	mock          MockProvider
	firstErr      error
	mu            sync.Mutex
	aCalls        int
	aRetryStarted chan struct{}
	releaseA      chan struct{}
	cStarted      chan struct{}
	aRetryOnce    sync.Once
	cOnce         sync.Once
}

func (p *transientConflictWaveProvider) Analyze(input AnalysisInput) (string, error) {
	switch input.SourceTitle {
	case "A":
		p.mu.Lock()
		p.aCalls++
		call := p.aCalls
		p.mu.Unlock()
		if call == 1 {
			return "", p.firstErr
		}
		p.aRetryOnce.Do(func() { close(p.aRetryStarted) })
		<-p.releaseA
	case "B":
		<-p.aRetryStarted
	case "C":
		p.cOnce.Do(func() { close(p.cStarted) })
	}
	return p.mock.Analyze(input)
}

func (p *transientConflictWaveProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	return p.mock.Generate(analysis, input)
}

func (p *transientOnceSourceProvider) Analyze(input AnalysisInput) (string, error) {
	p.calls++
	if p.calls == 1 {
		return "", context.DeadlineExceeded
	}
	return p.mock.Analyze(input)
}

func (p *transientOnceSourceProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	return p.mock.Generate(analysis, input)
}

func TestTransientLLMTimeoutDoesNotConsumeSourceAttempt(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nevidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &transientOnceSourceProvider{}
	var attempts []int
	if _, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: source, Provider: provider, MaxTaskAttempts: 1,
		OnProgress: func(progress ValidateProgress) {
			if progress.Phase == "analysis" {
				attempts = append(attempts, progress.Attempt)
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 || len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 1 {
		t.Fatalf("calls=%d attempts=%v", provider.calls, attempts)
	}
}

func TestConflictFailureRetryStaysInConflictWave(t *testing.T) {
	for _, test := range []struct {
		name     string
		firstErr error
	}{
		{name: "transient", firstErr: context.DeadlineExceeded},
		{name: "ordinary", firstErr: fmt.Errorf("invalid generated contract")},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
				t.Fatal(err)
			}
			sources := filepath.Join(t.TempDir(), "sources")
			if err := os.MkdirAll(sources, 0o755); err != nil {
				t.Fatal(err)
			}
			state := bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{}}
			for _, name := range []string{"a", "b", "c"} {
				path := filepath.Join(sources, name+".md")
				if err := os.WriteFile(path, []byte("# "+strings.ToUpper(name)+"\n\nevidence"), 0o644); err != nil {
					t.Fatal(err)
				}
				state.Sources[path] = bootstrapSourceTask{Status: "conflicted", Attempts: 1, ConflictAttempts: 1, ConflictPaths: []string{"wiki/entities/" + name + ".md"}}
			}
			if err := saveBootstrapTaskState(project, state); err != nil {
				t.Fatal(err)
			}
			provider := &transientConflictWaveProvider{
				firstErr: test.firstErr, aRetryStarted: make(chan struct{}), releaseA: make(chan struct{}), cStarted: make(chan struct{}),
			}
			done := make(chan error, 1)
			go func() {
				_, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 2, LLMConcurrency: 2})
				done <- err
			}()
			select {
			case <-provider.cStarted:
				// C can fill B's slot while the retry for A is still active.
			case <-time.After(2 * time.Second):
				close(provider.releaseA)
				<-done
				t.Fatal("disjoint conflict C did not start while conflict A retry was active")
			}
			close(provider.releaseA)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistedTimeoutAtAttemptLimitRecoversSameAttempt(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nevidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveBootstrapTaskState(project, bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{
		source: {Status: "failed", Attempts: 1, Reason: "generation: context deadline exceeded"},
	}}); err != nil {
		t.Fatal(err)
	}
	attempt := 0
	if _, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: source, Provider: &analysisOrderProvider{}, MaxTaskAttempts: 1,
		OnProgress: func(progress ValidateProgress) {
			if progress.Phase == "analysis" {
				attempt = progress.Attempt
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if attempt != 1 {
		t.Fatalf("recovered attempt=%d want 1", attempt)
	}
}

func TestCodexAccountModelRoutingErrorIsTransient(t *testing.T) {
	err := fmt.Errorf("new-page durability gate: llm request failed: status=400 body=The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.")
	if !isTransientSourceTaskError(err) {
		t.Fatalf("account-routed model rejection must be transient: %v", err)
	}
}

func TestValidateLLMWikiPathReportsSourceStagesAndSkips(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var progress []ValidateProgress
	_, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: MockProvider{}, SkipUnchanged: true,
		OnProgress: func(item ValidateProgress) { progress = append(progress, item) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 2} {
		for _, phase := range []string{"analysis", "generation", "persisting", "completed"} {
			if !hasProgress(progress, index, 2, phase) {
				t.Fatalf("missing progress index=%d phase=%s: %+v", index, phase, progress)
			}
		}
	}
	progress = nil
	_, err = ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: MockProvider{}, SkipUnchanged: true,
		OnProgress: func(item ValidateProgress) { progress = append(progress, item) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || progress[0].Phase != "skipped" || progress[1].Phase != "skipped" {
		t.Fatalf("expected two skipped events, got %+v", progress)
	}
}

type phaseConcurrencyProvider struct {
	mu              sync.Mutex
	activeAnalyze   int
	maxAnalyze      int
	activeGenerate  int
	maxGenerate     int
	generatedTitles []string
}

func (p *phaseConcurrencyProvider) Analyze(input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.activeAnalyze++
	if p.activeAnalyze > p.maxAnalyze {
		p.maxAnalyze = p.activeAnalyze
	}
	p.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.activeAnalyze--
	p.mu.Unlock()
	return fmt.Sprintf("## Wiki Plan\n- wiki/sources/%s.md", safeSourceSlug(input)), nil
}

func (p *phaseConcurrencyProvider) Generate(_ string, input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.activeGenerate++
	if p.activeGenerate > p.maxGenerate {
		p.maxGenerate = p.activeGenerate
	}
	p.generatedTitles = append(p.generatedTitles, input.SourceTitle)
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p.mu.Lock()
	p.activeGenerate--
	p.mu.Unlock()
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestValidateLLMWikiPathRunsWholeSourceTasksConcurrently(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &phaseConcurrencyProvider{}
	result, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 4 || provider.maxAnalyze < 2 || provider.maxGenerate < 2 {
		t.Fatalf("result=%+v maxAnalyze=%d maxGenerate=%d", result, provider.maxAnalyze, provider.maxGenerate)
	}
}

type continuousWorkerProvider struct {
	release  chan struct{}
	eStarted chan struct{}
	once     sync.Once
}

func (p *continuousWorkerProvider) Analyze(input AnalysisInput) (string, error) {
	if input.SourceTitle == "e.md" {
		p.once.Do(func() { close(p.eStarted) })
	}
	return fmt.Sprintf("## Wiki Plan\n- wiki/sources/%s.md", safeSourceSlug(input)), nil
}

func (p *continuousWorkerProvider) Generate(_ string, input AnalysisInput) (string, error) {
	if input.SourceTitle != "a.md" && input.SourceTitle != "e.md" {
		<-p.release
	}
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestValidateLLMWikiPathStartsNextFileWhenAnyWorkerFinishes(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &continuousWorkerProvider{release: make(chan struct{}), eStarted: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 4})
		done <- err
	}()
	select {
	case <-provider.eStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("fifth source did not start while other first-wave tasks were still running")
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type checkpointWindowProvider struct {
	mu       sync.Mutex
	analyzed []string
}

type analysisOrderProvider struct {
	mu    sync.Mutex
	order []string
	mock  MockProvider
}

type restoredConflictBarrierProvider struct {
	mock            MockProvider
	normalStarted   chan struct{}
	releaseNormal   chan struct{}
	conflictStarted chan struct{}
	normalOnce      sync.Once
	conflictOnce    sync.Once
}

func (p *restoredConflictBarrierProvider) Analyze(input AnalysisInput) (string, error) {
	if strings.Contains(strings.ToLower(input.SourceTitle), "b") {
		p.normalOnce.Do(func() { close(p.normalStarted) })
		<-p.releaseNormal
	} else {
		p.conflictOnce.Do(func() { close(p.conflictStarted) })
	}
	return p.mock.Analyze(input)
}

func (p *restoredConflictBarrierProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	return p.mock.Generate(analysis, input)
}

func (p *analysisOrderProvider) Analyze(input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.order = append(p.order, input.SourceTitle)
	p.mu.Unlock()
	return p.mock.Analyze(input)
}

func (p *analysisOrderProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	return p.mock.Generate(analysis, input)
}

func TestRestartKeepsConflictedTasksBehindNormalQueue(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(sources, "a.md")
	b := filepath.Join(sources, "b.md")
	if err := os.WriteFile(a, []byte("# A\n\nfirst source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("# B\n\nsecond source"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{
		a: {Status: "processing", Attempts: 1, ConflictAttempts: 1, ConflictPaths: []string{"wiki/entities/shared.md"}},
	}}
	if err := saveBootstrapTaskState(project, state); err != nil {
		t.Fatal(err)
	}
	provider := &analysisOrderProvider{}
	if _, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	order := append([]string(nil), provider.order...)
	provider.mu.Unlock()
	if len(order) != 2 || order[0] != "B" || order[1] != "A" {
		t.Fatalf("analysis order=%v; restored conflict must run after the normal source queue", order)
	}
}

func TestRestoredConflictWaitsForRunningNormalTaskToFinish(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(sources, "a.md")
	b := filepath.Join(sources, "b.md")
	for path, body := range map[string]string{a: "# A\n\nconflict source", b: "# B\n\nnormal source"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveBootstrapTaskState(project, bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{
		a: {Status: "processing", Attempts: 1, ConflictAttempts: 1, ConflictPaths: []string{"wiki/entities/shared.md"}},
	}}); err != nil {
		t.Fatal(err)
	}
	provider := &restoredConflictBarrierProvider{
		normalStarted: make(chan struct{}), releaseNormal: make(chan struct{}), conflictStarted: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		_, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 2})
		done <- err
	}()
	select {
	case <-provider.normalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("normal task did not start")
	}
	restored, err := loadBootstrapTaskState(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Sources[a].Status; got != "conflicted" {
		t.Fatalf("restored queued conflict status=%q want conflicted", got)
	}
	select {
	case <-provider.conflictStarted:
		t.Fatal("conflict task started while a normal task was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(provider.releaseNormal)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not finish")
	}
}

func TestRestartResumesInterruptedProcessingAtSameAttempt(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nevidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveBootstrapTaskState(project, bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{
		source: {Status: "processing", Attempts: 3},
	}}); err != nil {
		t.Fatal(err)
	}
	attempt := 0
	if _, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: source, Provider: &analysisOrderProvider{}, Concurrency: 1,
		MaxTaskAttempts: 3,
		OnProgress: func(progress ValidateProgress) {
			if progress.Phase == "analysis" {
				attempt = progress.Attempt
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if attempt != 3 {
		t.Fatalf("analysis attempt=%d; interrupted processing must resume attempt 3", attempt)
	}
}

type sharedPageProvider struct {
	mu                  sync.Mutex
	analyzeCalls        map[string]int
	firstWaveGate       chan struct{}
	firstWaveCount      int
	impactOnce          bool
	impactTransientOnce bool
	impactCalls         int
	updateOnlySeen      bool
}

func newSharedPageProvider() *sharedPageProvider {
	return &sharedPageProvider{analyzeCalls: map[string]int{}, firstWaveGate: make(chan struct{}), impactOnce: true, impactTransientOnce: true}
}

func (p *sharedPageProvider) Analyze(input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.analyzeCalls[input.SourceTitle]++
	p.mu.Unlock()
	return "## Wiki Plan\n- wiki/sources/" + safeSourceSlug(input) + ".md\n- wiki/entities/shared.md", nil
}

func (p *sharedPageProvider) Generate(_ string, input AnalysisInput) (string, error) {
	p.mu.Lock()
	if input.GenerationPolicy.UpdateOnly {
		p.updateOnlySeen = true
	}
	p.firstWaveCount++
	call := p.firstWaveCount
	if call == 2 {
		close(p.firstWaveGate)
	}
	p.mu.Unlock()
	if call <= 2 {
		<-p.firstWaveGate
	}
	sources := map[string]bool{input.SourceRel: true}
	for _, line := range strings.Split(input.ExistingPages, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-") {
			value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "-")), `"'`)
			if strings.HasPrefix(value, "raw/") {
				sources[value] = true
			}
		}
	}
	ordered := make([]string, 0, len(sources))
	for source := range sources {
		ordered = append(ordered, source)
	}
	sort.Strings(ordered)
	var sourceYAML strings.Builder
	for _, source := range ordered {
		fmt.Fprintf(&sourceYAML, "  - %q\n", source)
	}
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n\n---FILE: wiki/entities/shared.md\n---\ntype: \"entity\"\ntitle: \"Shared\"\nsources:\n%sconfidence: \"EXTRACTED\"\n---\n\n# Shared\n\nEvidence sources: %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle, sourceYAML.String(), strings.Join(ordered, ", ")), nil
}

func (p *sharedPageProvider) AssessImpact(input ImpactInput) (ImpactDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.impactCalls++
	if p.impactTransientOnce && len(input.Candidates) > 0 {
		p.impactTransientOnce = false
		return ImpactDecision{}, fmt.Errorf("status=400 body=The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.")
	}
	if !p.impactOnce || len(input.Candidates) == 0 {
		return ImpactDecision{}, nil
	}
	p.impactOnce = false
	return ImpactDecision{Affected: []ImpactFinding{{SourcePath: input.Candidates[0].SourcePath, Reason: "shared evidence needs regeneration"}}}, nil
}

func TestValidateLLMWikiPathRequeuesStaleWholeFileTask(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := newSharedPageProvider()
	result, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 2 {
		t.Fatalf("result=%+v", result)
	}
	provider.mu.Lock()
	totalAnalyze := provider.analyzeCalls["a.md"] + provider.analyzeCalls["b.md"]
	updateOnlySeen := provider.updateOnlySeen
	impactCalls := provider.impactCalls
	provider.mu.Unlock()
	if totalAnalyze < 4 {
		t.Fatalf("analyze calls=%d; expected conflict and impact full-task retries", totalAnalyze)
	}
	if !updateOnlySeen {
		t.Fatal("impact requeue did not use update-only generation policy")
	}
	if impactCalls < 2 {
		t.Fatalf("impact calls=%d; transient impact error was not retried", impactCalls)
	}
	page, err := os.ReadFile(filepath.Join(project, "wiki", "entities", "shared.md"))
	if err != nil {
		t.Fatal(err)
	}
	frontmatter, err := parseGeneratedFrontmatter(string(page))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(frontmatterStrings(frontmatter["sources"])); got != 2 {
		t.Fatalf("shared sources=%v page=\n%s", frontmatter["sources"], page)
	}
	state, err := loadBootstrapTaskState(project)
	if err != nil {
		t.Fatal(err)
	}
	impactAttempts := 0
	for _, source := range state.Sources {
		impactAttempts += source.ImpactAttempts
	}
	if impactAttempts != 1 {
		t.Fatalf("impact attempts=%d state=%+v", impactAttempts, state.Sources)
	}
}

type alwaysFailSourceProvider struct{ calls int }

func (p *alwaysFailSourceProvider) Analyze(input AnalysisInput) (string, error) {
	p.calls++
	return "", fmt.Errorf("provider unavailable")
}

func (p *alwaysFailSourceProvider) Generate(string, AnalysisInput) (string, error) {
	p.calls++
	return "", fmt.Errorf("provider unavailable")
}

func TestValidateLLMWikiPathStopsAtPersistedAttemptLimit(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "a.md")
	if err := os.WriteFile(source, []byte("# A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &alwaysFailSourceProvider{}
	_, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sourceDir, Provider: provider, MaxTaskAttempts: 1})
	if !IsSourceTaskExhaustedError(err) || provider.calls != 1 {
		t.Fatalf("first err=%v calls=%d", err, provider.calls)
	}
	_, err = ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sourceDir, Provider: provider, MaxTaskAttempts: 1})
	if !IsSourceTaskExhaustedError(err) || provider.calls != 1 {
		t.Fatalf("persisted err=%v calls=%d", err, provider.calls)
	}
	if err := os.WriteFile(filepath.Join(project, "purpose.md"), []byte("# Changed purpose\n\nChanged generation contract.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sourceDir, Provider: provider, MaxTaskAttempts: 1})
	if provider.calls != 2 {
		t.Fatalf("contract change did not reset attempts: calls=%d", provider.calls)
	}
}

func (p *checkpointWindowProvider) Analyze(input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.analyzed = append(p.analyzed, input.SourceTitle)
	p.mu.Unlock()
	return fmt.Sprintf("## Wiki Plan\n- wiki/sources/%s.md", safeSourceSlug(input)), nil
}

func (p *checkpointWindowProvider) Generate(_ string, input AnalysisInput) (string, error) {
	if input.SourceTitle == "a.md" {
		return "", fmt.Errorf("generation unavailable")
	}
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestValidateLLMWikiPathKeepsSuccessfulPeerCommitWhenAnotherTaskFails(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &checkpointWindowProvider{}
	result, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: provider,
		SkipUnchanged: true, Concurrency: 2, MaxTaskAttempts: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "generation unavailable") {
		t.Fatalf("err=%v", err)
	}
	if result.SourceCount < 1 {
		t.Fatalf("result=%+v; successful concurrent source should be checkpointed", result)
	}
	manifest, manifestErr := loadSourceManifest(project)
	if manifestErr != nil {
		t.Fatal(manifestErr)
	}
	if len(manifest.Sources) < 1 {
		t.Fatalf("manifest sources=%d want at least 1", len(manifest.Sources))
	}
}

type retryFailedSourceProvider struct {
	mu       sync.Mutex
	attempts map[string]int
}

func (p *retryFailedSourceProvider) Analyze(input AnalysisInput) (string, error) {
	return fmt.Sprintf("## Wiki Plan\n- wiki/sources/%s.md", safeSourceSlug(input)), nil
}

func (p *retryFailedSourceProvider) Generate(_ string, input AnalysisInput) (string, error) {
	p.mu.Lock()
	p.attempts[input.SourceTitle]++
	attempt := p.attempts[input.SourceTitle]
	p.mu.Unlock()
	if input.SourceTitle == "a.md" && attempt == 1 {
		return "", fmt.Errorf("temporary generation failure")
	}
	return fmt.Sprintf("---FILE: wiki/sources/%s.md\n---\ntype: \"source-summary\"\ntitle: %q\nsources:\n  - %q\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n", safeSourceSlug(input), input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestValidateLLMWikiPathRequeuesOrdinarySourceFailure(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name+"\n\nsource"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &retryFailedSourceProvider{attempts: map[string]int{}}
	var progressMu sync.Mutex
	var progress []ValidateProgress
	result, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: sources, Provider: provider, Concurrency: 2, MaxTaskAttempts: 3,
		OnProgress: func(item ValidateProgress) {
			progressMu.Lock()
			progress = append(progress, item)
			progressMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 3 || provider.attempts["a.md"] != 2 {
		t.Fatalf("result=%+v attempts=%v", result, provider.attempts)
	}
	if !hasProgress(progress, 1, 3, "failed_requeued") {
		t.Fatalf("missing failed_requeued progress: %+v", progress)
	}
}

type recoveryCountingProvider struct{ calls int }

func (p *recoveryCountingProvider) Analyze(input AnalysisInput) (string, error) {
	p.calls++
	return (MockProvider{}).Analyze(input)
}

func (p *recoveryCountingProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	p.calls++
	return (MockProvider{}).Generate(analysis, input)
}

func TestValidateLLMWikiPathRecoversProcessingTaskInsteadOfTrustingManifest(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sources, "a.md")
	if err := os.WriteFile(source, []byte("# a.md\n\nsource"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: MockProvider{}, Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	state, err := loadBootstrapTaskState(project)
	if err != nil {
		t.Fatal(err)
	}
	state.Sources[source] = bootstrapSourceTask{Status: "processing", Attempts: 1}
	if err := saveBootstrapTaskState(project, state); err != nil {
		t.Fatal(err)
	}
	provider := &recoveryCountingProvider{}
	if _, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: project, SourcePath: sources, Provider: provider, SkipUnchanged: true, Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if provider.calls == 0 {
		t.Fatal("processing task was incorrectly skipped from manifest after recovery")
	}
}

func hasProgress(items []ValidateProgress, index, total int, phase string) bool {
	for _, item := range items {
		if item.Index == index && item.Total == total && item.Phase == phase {
			return true
		}
	}
	return false
}

type countedMockProvider struct {
	analyzeCalls  int
	generateCalls int
}

func (p *countedMockProvider) Analyze(input AnalysisInput) (string, error) {
	p.analyzeCalls++
	return (MockProvider{}).Analyze(input)
}

func (p *countedMockProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	p.generateCalls++
	return (MockProvider{}).Generate(analysis, input)
}

func TestPostCommitSyncFailureRetriesSyncWithoutAnotherLLMGeneration(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: project, Name: "pg-retry"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n\nEvidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &countedMockProvider{}
	syncCalls := 0
	result, err := ValidateLLMWikiPath(ValidateOptions{
		ProjectPath: project, SourcePath: source, Provider: provider, MaxTaskAttempts: 2,
		OnCommitted: func(ValidateResult) error {
			syncCalls++
			if syncCalls == 1 {
				return fmt.Errorf("postgres temporarily unavailable")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 1 || syncCalls != 2 || provider.analyzeCalls != 1 || provider.generateCalls != 1 {
		t.Fatalf("result=%+v sync=%d analyze=%d generate=%d", result, syncCalls, provider.analyzeCalls, provider.generateCalls)
	}
}
