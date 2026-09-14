package compiler

import "testing"

func TestGeneratedContentQualityRejectsArtifacts(t *testing.T) {
	for _, body := range []string{"... body ...", "Wait, I should be careful.", "Make sure there are no extra blank lines outside the block? The instruction says test"} {
		if ValidateGeneratedPageContent(body) == nil {
			t.Errorf("accepted generation artifact %q", body)
		}
	}
	if err := ValidateGeneratedPageContent("# Original\n\nThe document quotes an example:\n```text\n... body ...\n```\n\nAn ordinary account of events."); err != nil {
		t.Fatal(err)
	}
}
