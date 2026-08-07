package compiler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/promptbudget"
)

type PageChange struct {
	Path          string `json:"path"`
	BeforeSHA256  string `json:"before_sha256,omitempty"`
	AfterSHA256   string `json:"after_sha256"`
	BeforeExcerpt string `json:"before_excerpt,omitempty"`
	AfterExcerpt  string `json:"after_excerpt"`
}

type ImpactCandidate struct {
	SourcePath    string   `json:"source_path"`
	SourceTitle   string   `json:"source_title"`
	RawPath       string   `json:"raw_path"`
	Files         []string `json:"files"`
	SourceExcerpt string   `json:"source_excerpt"`
}

type ImpactInput struct {
	ProjectPath   string            `json:"-"`
	ChangedSource string            `json:"changed_source"`
	Purpose       string            `json:"purpose"`
	Schema        string            `json:"schema"`
	Changes       []PageChange      `json:"changes"`
	Candidates    []ImpactCandidate `json:"candidates"`
}

type ImpactFinding struct {
	SourcePath    string   `json:"source_path"`
	Kind          string   `json:"kind"`
	Reason        string   `json:"reason"`
	Evidence      string   `json:"evidence"`
	AfterEvidence string   `json:"after_evidence"`
	Paths         []string `json:"paths,omitempty"`
}

type ImpactDecision struct {
	Affected []ImpactFinding `json:"affected"`
}

type ImpactAssessor interface {
	AssessImpact(ImpactInput) (ImpactDecision, error)
}

func (p OpenAICompatibleProvider) AssessImpact(input ImpactInput) (ImpactDecision, error) {
	input.Purpose = promptbudget.TrimEnd(input.Purpose, 6000)
	input.Schema = promptbudget.TrimEnd(input.Schema, 6000)
	for i := range input.Changes {
		input.Changes[i].BeforeExcerpt = promptbudget.TrimMiddle(input.Changes[i].BeforeExcerpt, 5000)
		input.Changes[i].AfterExcerpt = promptbudget.TrimMiddle(input.Changes[i].AfterExcerpt, 5000)
	}
	for i := range input.Candidates {
		input.Candidates[i].SourceExcerpt = promptbudget.TrimMiddle(input.Candidates[i].SourceExcerpt, 4000)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return ImpactDecision{}, err
	}
	system := `You maintain a cumulative persistent LLM Wiki and decide whether a completed source task became semantically stale after another source changed shared wiki pages.
Return only JSON: {"affected":[{"source_path":"...","kind":"evidence_removed|evidence_contradicted|evidence_misrepresented|required_link_broken","reason":"...","evidence":"exact candidate-source claim that is no longer represented correctly","after_evidence":"exact changed-page text or state proving the loss/conflict","paths":["wiki/..."]}]}.

The wiki is a cumulative synthesis, not a source-scoped page set. Return an empty affected list when changes are additive and compatible.
NEVER mark a candidate affected merely because a shared page gained another chapter's events, roles, aliases, sources, richer synthesis, changed framing, or a more specific page type. Those are normal cumulative updates.
An earlier source's narrower scope, endpoint, framing, or incomplete view does not need to remain the shared page's complete scope. Its source-summary preserves that source-scoped view. If the shared page now also covers later evidence while the earlier claim remains compatible, return an empty affected list.
Mark a candidate only when you can name a concrete claim from that candidate that the after-page removed, contradicted, materially distorted, or left with a broken required link. Generic statements such as "should be regenerated", "shared page changed", "keep integration clean", or "now includes substantial new evidence" are not valid impact evidence.
Every finding must contain a valid kind plus concrete evidence and after_evidence. The source_path must exactly match one candidate.`
	content, err := p.chat(system, string(payload))
	if err != nil {
		return ImpactDecision{}, err
	}
	return parseImpactDecisionInput(content, input)
}

func (MockProvider) AssessImpact(ImpactInput) (ImpactDecision, error) {
	return ImpactDecision{}, nil
}

func parseImpactDecision(content string, candidates []ImpactCandidate) (ImpactDecision, error) {
	return parseImpactDecisionInput(content, ImpactInput{Candidates: candidates})
}

func parseImpactDecisionInput(content string, input ImpactInput) (ImpactDecision, error) {
	content = strings.TrimSpace(content)
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end >= start {
			content = content[start : end+1]
		}
	}
	var decision ImpactDecision
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		return ImpactDecision{}, fmt.Errorf("decode impact decision: %w", err)
	}
	allowed := make(map[string]ImpactCandidate, len(input.Candidates))
	for _, candidate := range input.Candidates {
		allowed[candidate.SourcePath] = candidate
	}
	seen := map[string]bool{}
	validKinds := map[string]bool{
		"evidence_removed": true, "evidence_contradicted": true,
		"evidence_misrepresented": true, "required_link_broken": true,
	}
	filtered := decision.Affected[:0]
	for _, finding := range decision.Affected {
		finding.SourcePath = strings.TrimSpace(finding.SourcePath)
		finding.Kind = strings.TrimSpace(finding.Kind)
		finding.Reason = strings.TrimSpace(finding.Reason)
		finding.Evidence = strings.TrimSpace(finding.Evidence)
		finding.AfterEvidence = strings.TrimSpace(finding.AfterEvidence)
		candidate, candidateOK := allowed[finding.SourcePath]
		if !candidateOK || seen[finding.SourcePath] || !validKinds[finding.Kind] || finding.Reason == "" || finding.Evidence == "" || finding.AfterEvidence == "" || additiveOnlyImpactFinding(finding) || !groundedImpactFinding(finding, candidate, input.Changes) {
			continue
		}
		seen[finding.SourcePath] = true
		filtered = append(filtered, finding)
	}
	decision.Affected = filtered
	return decision, nil
}

func groundedImpactFinding(finding ImpactFinding, candidate ImpactCandidate, changes []PageChange) bool {
	// Legacy/offline callers may not supply excerpts. Real impact review always
	// does, and then every claimed loss must be traceable to exact raw evidence
	// and exact post-change page evidence.
	if strings.TrimSpace(candidate.SourceExcerpt) != "" && !containsNormalized(candidate.SourceExcerpt, finding.Evidence) {
		return false
	}
	if len(changes) == 0 {
		return true
	}
	pathSet := map[string]bool{}
	for _, path := range finding.Paths {
		pathSet[strings.TrimSpace(path)] = true
	}
	afterGrounded := false
	pathGrounded := len(pathSet) == 0
	for _, change := range changes {
		if pathSet[change.Path] {
			pathGrounded = true
		}
		if containsNormalized(change.AfterExcerpt, finding.AfterEvidence) {
			afterGrounded = true
		}
	}
	return pathGrounded && afterGrounded
}

func containsNormalized(haystack, needle string) bool {
	normalize := func(value string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
	}
	needle = normalize(needle)
	return needle != "" && strings.Contains(normalize(haystack), needle)
}

func additiveOnlyImpactFinding(finding ImpactFinding) bool {
	text := strings.ToLower(strings.Join([]string{finding.Reason, finding.AfterEvidence}, " "))
	patterns := []string{
		"now also", "now includes", "now incorporates", "gained new", "added new",
		"broader synthesis", "broader framing", "broader scope", "expanded scope",
		"no longer represented as the page's full scope", "absorbed into a broader",
		"shared page changed", "substantial new evidence", "richer synthesis",
		"narrower framing", "narrower evidentiary framing", "source-scoped claim",
		"no longer contains the earlier", "rewritten around a different later-formed account",
		"现在还", "现又", "新增了", "加入了后续", "更广泛的综合", "范围扩大",
		"较窄表述", "来源范围", "后续章节",
	}
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}
