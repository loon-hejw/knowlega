package service

import "testing"

func TestExtractJSONObjectPrefersFinalValidObjectAfterReasoning(t *testing.T) {
	content := `I should return {"shape":{"issues":[]}} but first inspect the evidence.
The quoted brace "}" is not the answer.
Final answer:
{"issues":[{"type":"stale-claim","path":"wiki/example.md","detail":"3:1 is retired"}]}`
	want := `{"issues":[{"type":"stale-claim","path":"wiki/example.md","detail":"3:1 is retired"}]}`
	if got := extractJSONObject(content); got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestBalancedJSONObjectHandlesEscapedBraces(t *testing.T) {
	content := `prefix {"detail":"quoted \"}\" and { brace","nested":{"ok":true}} suffix`
	start := 7
	got, ok := balancedJSONObject(content, start)
	if !ok || got != `{"detail":"quoted \"}\" and { brace","nested":{"ok":true}}` {
		t.Fatalf("got=%q ok=%v", got, ok)
	}
}
