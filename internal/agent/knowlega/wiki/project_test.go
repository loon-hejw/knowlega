package wiki

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitProjectCreatesExpectedLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := InitProject(ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"purpose.md",
		"schema.md",
		"raw/sources",
		"raw/code-graphs",
		"wiki/index.md",
		"wiki/log.md",
		"wiki/overview.md",
		"wiki/reviews.md",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
	}
}
