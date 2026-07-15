package compiler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hejw/knowledge-core/internal/promptbudget"
)

type NewPageCandidate struct{ Path, Content string }
type NewPageGateInput struct {
	Purpose, SourceTitle, SourceRel, SourceText, Analysis, OpenReviews string
	Pages                                                              []NewPageCandidate
}
type NewPageRejection struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type NewPageGateDecision struct {
	Accepted []string           `json:"accepted"`
	Rejected []NewPageRejection `json:"rejected"`
}
type NewPageGate interface {
	GateNewPages(NewPageGateInput) (NewPageGateDecision, error)
}

func (p OpenAICompatibleProvider) GateNewPages(input NewPageGateInput) (NewPageGateDecision, error) {
	payload, _ := json.Marshal(input.Pages)
	system := `You are the durability gate for a persistent LLM Wiki. Return only JSON: {"accepted":["wiki/..."],"rejected":[{"path":"wiki/...","reason":"..."}]}. Accept a new page only when it is a durable recurring identity, central entity/concept, or necessary canonical link target supported by the source. Reject one-off labels, document pointers, incidental objects, and pages contradicted by open review guidance. Every supplied path must appear exactly once in accepted or rejected.`
	gateProvider := p
	if gateProvider.MaxOutputTokens <= 0 || gateProvider.MaxOutputTokens > 2048 {
		gateProvider.MaxOutputTokens = 2048
	}
	content, err := gateProvider.chat(system, fmt.Sprintf("Purpose:\n%s\nSource: %s (%s)\nAnalysis:\n%s\nOpen reviews:\n%s\nCandidates:\n%s\nSource text:\n%s", promptbudget.TrimEnd(input.Purpose, 5000), input.SourceTitle, input.SourceRel, promptbudget.TrimMiddle(input.Analysis, 10000), promptbudget.TrimMiddle(input.OpenReviews, 12000), payload, promptbudget.TrimMiddle(input.SourceText, 24000)))
	if err != nil {
		return NewPageGateDecision{}, err
	}
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end >= start {
			content = content[start : end+1]
		}
	}
	var decision NewPageGateDecision
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		return decision, fmt.Errorf("decode new-page gate: %w", err)
	}
	return decision, nil
}

func applyNewPageGate(opts ValidateOptions, work *analyzedSource, blocks ParsedBlocks) (ParsedBlocks, error) {
	gate, ok := opts.Provider.(NewPageGate)
	if !ok {
		return blocks, nil
	}
	var candidates []NewPageCandidate
	for _, file := range blocks.Files {
		fm, err := parseGeneratedFrontmatter(file.Content)
		if err != nil || strings.TrimSpace(fmt.Sprint(fm["type"])) == "source-summary" {
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.ProjectPath, filepath.FromSlash(file.Path))); os.IsNotExist(err) {
			candidates = append(candidates, NewPageCandidate{Path: file.Path, Content: file.Content})
		}
	}
	if len(candidates) == 0 {
		return blocks, nil
	}
	decision, err := gate.GateNewPages(NewPageGateInput{Purpose: work.input.Purpose, SourceTitle: work.title, SourceRel: work.rawRel, SourceText: work.input.SourceText, Analysis: work.analysis, OpenReviews: readOptional(filepath.Join(opts.ProjectPath, "wiki", "reviews.md")), Pages: candidates})
	if err != nil {
		return blocks, fmt.Errorf("new-page durability gate: %w", err)
	}
	accepted := map[string]bool{}
	rejected := map[string]string{}
	candidateSet := map[string]bool{}
	for _, item := range candidates {
		candidateSet[item.Path] = true
	}
	for _, path := range decision.Accepted {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if candidateSet[path] {
			accepted[path] = true
		}
	}
	for _, item := range decision.Rejected {
		path := filepath.ToSlash(strings.TrimSpace(item.Path))
		if candidateSet[path] {
			rejected[path] = strings.TrimSpace(item.Reason)
		}
	}
	for _, item := range candidates {
		if accepted[item.Path] && rejected[item.Path] != "" {
			// A contradictory durability decision is uncertainty, not a reason to
			// regenerate the entire source task. Rejecting the optional new page is
			// the conservative outcome and keeps the durable source summary usable.
			delete(accepted, item.Path)
			rejected[item.Path] += "; gate returned both accepted and rejected, so rejection won conservatively"
		}
		if !accepted[item.Path] && rejected[item.Path] == "" {
			return blocks, fmt.Errorf("new-page durability gate omitted %s", item.Path)
		}
	}
	kept := blocks.Files[:0]
	for _, file := range blocks.Files {
		if reason, drop := rejected[file.Path]; drop {
			blocks.Reviews = append(blocks.Reviews, ReviewBlock{Type: "review-needed", Title: "Durability gate rejected " + file.Path, Body: reason})
			continue
		}
		kept = append(kept, file)
	}
	blocks.Files = kept
	downgradeMissingWikilinks(opts.ProjectPath, &blocks)
	downgradeMissingReviewWikilinks(opts.ProjectPath, &blocks)
	// Snapshot-sensitive validation belongs to the caller after the gate. A
	// candidate can be created by another source while this LLM call is in
	// flight; validating against os.Stat here would misclassify that optimistic
	// conflict as an ordinary generation failure before staleGeneratedPaths can
	// compare it with the task snapshot.
	return blocks, nil
}
