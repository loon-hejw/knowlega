package compiler

import "testing"

func TestParseImpactDecisionFiltersUnknownAndDuplicateSources(t *testing.T) {
	decision, err := parseImpactDecision(`prose
{"affected":[
  {"source_path":"a.md","kind":"evidence_removed","reason":"evidence removed","evidence":"alpha owns beta","after_evidence":"alpha claim absent","paths":["wiki/entities/shared.md"]},
  {"source_path":"a.md","reason":"duplicate"},
  {"source_path":"unknown.md","reason":"not a candidate"}
]}
`, []ImpactCandidate{{SourcePath: "a.md"}, {SourcePath: "b.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Affected) != 1 || decision.Affected[0].SourcePath != "a.md" {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestParseImpactDecisionRejectsAdditiveOrUnsubstantiatedFinding(t *testing.T) {
	decision, err := parseImpactDecision(`{"affected":[{"source_path":"a.md","reason":"shared page gained new evidence","paths":["wiki/entities/shared.md"]}]}`, []ImpactCandidate{{SourcePath: "a.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Affected) != 0 {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestParseImpactDecisionRejectsStructuredAdditiveScopeFinding(t *testing.T) {
	decision, err := parseImpactDecision(`{"affected":[{
  "source_path":"a.md",
  "kind":"evidence_misrepresented",
  "reason":"The earlier chapter-only ending is no longer represented as the page's full scope.",
  "evidence":"The chapter ended at the cave.",
  "after_evidence":"The page now also incorporates later events into a broader synthesis.",
  "paths":["wiki/entities/shared.md"]
}]}`, []ImpactCandidate{{SourcePath: "a.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Affected) != 0 {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestParseImpactDecisionRejectsInvalidJSON(t *testing.T) {
	if _, err := parseImpactDecision("not json", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseImpactDecisionRequiresExactSourceAndAfterPageEvidence(t *testing.T) {
	input := ImpactInput{
		Candidates: []ImpactCandidate{{SourcePath: "a.md", SourceExcerpt: "The seal was broken by the pilgrim."}},
		Changes:    []PageChange{{Path: "wiki/entities/shared.md", AfterExcerpt: "The seal remains intact."}},
	}
	valid := `{"affected":[{"source_path":"a.md","kind":"evidence_contradicted","reason":"The claim is contradicted.","evidence":"The seal was broken by the pilgrim.","after_evidence":"The seal remains intact.","paths":["wiki/entities/shared.md"]}]}`
	decision, err := parseImpactDecisionInput(valid, input)
	if err != nil || len(decision.Affected) != 1 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	ungrounded := `{"affected":[{"source_path":"a.md","kind":"evidence_contradicted","reason":"The claim is contradicted.","evidence":"A paraphrase not present in the source","after_evidence":"Another paraphrase","paths":["wiki/entities/shared.md"]}]}`
	decision, err = parseImpactDecisionInput(ungrounded, input)
	if err != nil || len(decision.Affected) != 0 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}
