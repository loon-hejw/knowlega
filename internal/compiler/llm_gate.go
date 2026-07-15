package compiler

import (
	"fmt"
	"sync/atomic"
	"time"
)

var llmCallSequence atomic.Uint64

// llmGate is shared by source analysis/generation/repair and impact review so
// the configured bootstrap concurrency is a real upper bound on LLM requests,
// not merely the number of source workers.
type llmGate struct {
	slots chan struct{}
}

func newLLMGate(limit int) *llmGate {
	if limit < 1 {
		limit = 1
	}
	return &llmGate{slots: make(chan struct{}, limit)}
}

func (g *llmGate) acquire() func() {
	g.slots <- struct{}{}
	return func() { <-g.slots }
}

type gatedProvider struct {
	provider Provider
	gate     *llmGate
	observe  func(LLMCallMetric)
}

func (p gatedProvider) Analyze(input AnalysisInput) (string, error) {
	release := p.gate.acquire()
	defer release()
	started := time.Now()
	id := p.start("analysis", started)
	result, err := p.provider.Analyze(input)
	p.finish("analysis", id, started, err)
	return result, err
}

func (p gatedProvider) Generate(analysis string, input AnalysisInput) (string, error) {
	release := p.gate.acquire()
	defer release()
	started := time.Now()
	id := p.start("generation", started)
	result, err := p.provider.Generate(analysis, input)
	p.finish("generation", id, started, err)
	return result, err
}

func (p gatedProvider) GateNewPages(input NewPageGateInput) (NewPageGateDecision, error) {
	gate, ok := p.provider.(NewPageGate)
	if !ok {
		return NewPageGateDecision{Accepted: candidatePaths(input.Pages)}, nil
	}
	release := p.gate.acquire()
	defer release()
	started := time.Now()
	id := p.start("page_gate", started)
	result, err := gate.GateNewPages(input)
	p.finish("page_gate", id, started, err)
	return result, err
}

func candidatePaths(pages []NewPageCandidate) []string {
	out := make([]string, 0, len(pages))
	for _, page := range pages {
		out = append(out, page.Path)
	}
	return out
}

func (p gatedProvider) start(kind string, started time.Time) string {
	id := fmt.Sprintf("%s-%d", kind, llmCallSequence.Add(1))
	if p.observe != nil {
		p.observe(LLMCallMetric{Kind: kind, ID: id, Started: true, StartedAt: started})
	}
	return id
}

func (p gatedProvider) finish(kind, id string, started time.Time, err error) {
	if p.observe != nil {
		p.observe(LLMCallMetric{Kind: kind, ID: id, StartedAt: started, Duration: time.Since(started), Failed: err != nil})
	}
}

type gatedImpactAssessor struct {
	assessor ImpactAssessor
	gate     *llmGate
	observe  func(LLMCallMetric)
}

func (a gatedImpactAssessor) AssessImpact(input ImpactInput) (ImpactDecision, error) {
	release := a.gate.acquire()
	defer release()
	started := time.Now()
	id := fmt.Sprintf("impact-%d", llmCallSequence.Add(1))
	if a.observe != nil {
		a.observe(LLMCallMetric{Kind: "impact", ID: id, Started: true, StartedAt: started})
	}
	result, err := a.assessor.AssessImpact(input)
	if a.observe != nil {
		a.observe(LLMCallMetric{Kind: "impact", ID: id, StartedAt: started, Duration: time.Since(started), Failed: err != nil})
	}
	return result, err
}
