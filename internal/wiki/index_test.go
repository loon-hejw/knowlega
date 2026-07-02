package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendIndexEntryUsesSections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := InitProject(ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendIndexEntry(root, "Concepts", "Token Validation", "wiki/concepts/token-validation.md"); err != nil {
		t.Fatal(err)
	}
	if err := AppendIndexEntry(root, "Sources", "OAuth Notes", "wiki/sources/oauth-notes.md"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	concepts := strings.Index(content, "## Concepts")
	conceptEntry := strings.Index(content, "wiki/concepts/token-validation.md")
	sources := strings.Index(content, "## Sources")
	sourceEntry := strings.Index(content, "wiki/sources/oauth-notes.md")
	if concepts < 0 || conceptEntry < concepts {
		t.Fatalf("concept entry not under Concepts:\n%s", content)
	}
	if sources < 0 || sourceEntry < sources {
		t.Fatalf("source entry not under Sources:\n%s", content)
	}
	if !strings.Contains(content, "wiki/sources/oauth-notes.md`\n\n## Code") {
		t.Fatalf("source section is not separated from next heading:\n%s", content)
	}
}
