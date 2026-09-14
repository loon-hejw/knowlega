package compiler

import (
	"fmt"
	"strings"
)

// ValidateGeneratedPageContent rejects unambiguous generation artifacts, not
// semantic claims. Contradictions and coverage belong to the LLM review pass.
func ValidateGeneratedPageContent(content string) error {
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lower := strings.ToLower(line)
		for _, marker := range []string{"... body ...", "[insert content here]", "wait, i should", "wait, the rules say:", "make sure there are no extra blank lines outside the block", "return exactly one ---file block", "double-check the path", "actually, looking at the existing wiki", "let me reconsider what the"} {
			if strings.HasPrefix(lower, marker) {
				return fmt.Errorf("generated page contains unfinished or instructional content: %s", marker)
			}
		}
	}
	return nil
}
