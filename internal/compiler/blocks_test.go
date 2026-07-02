package compiler

import "testing"

func TestParseBlocks(t *testing.T) {
	parsed, err := ParseBlocks(`---FILE: wiki/concepts/token-validation.md
---
type: "concept"
title: "Token Validation"
---

# Token Validation

---REVIEW: suggestion | Follow up
SEARCH: token validation
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("files=%d", len(parsed.Files))
	}
	if parsed.Files[0].Path != "wiki/concepts/token-validation.md" {
		t.Fatalf("path=%s", parsed.Files[0].Path)
	}
	if len(parsed.Reviews) != 1 {
		t.Fatalf("reviews=%d", len(parsed.Reviews))
	}
}

func TestParseBlocksRejectsUnsafePath(t *testing.T) {
	_, err := ParseBlocks(`---FILE: ../outside.md
---
title: "x"
---
`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBlocksRequiresFrontmatter(t *testing.T) {
	_, err := ParseBlocks(`---FILE: wiki/concepts/x.md
# Missing Frontmatter
`)
	if err == nil {
		t.Fatal("expected error")
	}
}
