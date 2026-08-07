package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type concurrentProbe struct {
	active atomic.Int32
	max    atomic.Int32
	block  <-chan struct{}
}

func (p *concurrentProbe) enter() func() {
	active := p.active.Add(1)
	for {
		max := p.max.Load()
		if active <= max || p.max.CompareAndSwap(max, active) {
			break
		}
	}
	return func() { p.active.Add(-1) }
}

type gateProbeProvider struct{ probe *concurrentProbe }

func (p gateProbeProvider) Analyze(AnalysisInput) (string, error) {
	leave := p.probe.enter()
	defer leave()
	<-p.probe.block
	return "ok", nil
}

func (p gateProbeProvider) Generate(string, AnalysisInput) (string, error) {
	leave := p.probe.enter()
	defer leave()
	<-p.probe.block
	return "ok", nil
}

type gateProbeImpact struct{ probe *concurrentProbe }

func (p gateProbeImpact) AssessImpact(ImpactInput) (ImpactDecision, error) {
	leave := p.probe.enter()
	defer leave()
	<-p.probe.block
	return ImpactDecision{}, nil
}

func TestLLMGateBoundsSourceAndImpactRequestsTogether(t *testing.T) {
	block := make(chan struct{})
	probe := &concurrentProbe{block: block}
	gate := newLLMGate(2)
	provider := gatedProvider{provider: gateProbeProvider{probe: probe}, gate: gate}
	impact := gatedImpactAssessor{assessor: gateProbeImpact{probe: probe}, gate: gate}
	var wg sync.WaitGroup
	for index := 0; index < 3; index++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = provider.Analyze(AnalysisInput{}) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = impact.AssessImpact(ImpactInput{}) }()
	time.Sleep(50 * time.Millisecond)
	if got := probe.max.Load(); got != 2 {
		t.Fatalf("max concurrent LLM requests=%d want 2", got)
	}
	close(block)
	wg.Wait()
}

func TestLLMConcurrencyCanBeLowerThanSourceTaskConcurrency(t *testing.T) {
	if got := normalizedLLMConcurrency(4, 2); got != 2 {
		t.Fatalf("llm concurrency=%d want 2", got)
	}
	if got := normalizedLLMConcurrency(4, 0); got != 4 {
		t.Fatalf("inherited llm concurrency=%d want 4", got)
	}
}

type lockedSharedPageProvider struct {
	active atomic.Int32
	max    atomic.Int32
}

func (p *lockedSharedPageProvider) Analyze(AnalysisInput) (string, error) {
	return "UPDATE EXISTING wiki/entities/shared.md", nil
}

func (p *lockedSharedPageProvider) Generate(_ string, input AnalysisInput) (string, error) {
	active := p.active.Add(1)
	for {
		max := p.max.Load()
		if active <= max || p.max.CompareAndSwap(max, active) {
			break
		}
	}
	defer p.active.Add(-1)
	time.Sleep(40 * time.Millisecond)
	slug := core.Slug(input.SourceTitle)
	return fmt.Sprintf(`---FILE: wiki/sources/%s.md
---
type: source-summary
title: %q
sources: [%q]
---

# %s

---FILE: wiki/entities/shared.md
---
type: entity
title: Shared
sources: [%q]
---

# Shared

Evidence from %s.
`, slug, input.SourceTitle, input.SourceRel, input.SourceTitle, input.SourceRel, input.SourceTitle), nil
}

func TestSharedEvidencePagesGenerateOptimisticallyInParallel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "locks"}); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "wiki", "entities", "shared.md")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("---\ntype: entity\ntitle: Shared\nsources: []\n---\n\n# Shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.md", "beta.md"} {
		if err := os.WriteFile(filepath.Join(sources, name), []byte("# "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := &lockedSharedPageProvider{}
	result, err := ValidateLLMWikiPath(ValidateOptions{ProjectPath: root, SourcePath: sources, Provider: provider, Concurrency: 2, MaxTaskAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceCount != 2 {
		t.Fatalf("result=%+v", result)
	}
	if got := provider.max.Load(); got < 2 {
		t.Fatalf("shared-page generations did not overlap: max=%d", got)
	}
}
