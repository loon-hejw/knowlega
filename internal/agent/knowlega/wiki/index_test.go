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

func TestRebuildIndexCatalogsPagesByTypeAndDropsAggregateProse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := InitProject(ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"wiki/sources/a.md":  "---\ntype: source-summary\ntitle: Z Source\nsources:\n  - raw/sources/a.txt\n---\n\n# Z Source\n",
		"wiki/sources/b.md":  "---\ntype: source-summary\ntitle: A Source\nsources:\n  - raw/sources/b.txt\n---\n\n# A Source\n",
		"wiki/concept/c.md":  "---\ntype: concept\ntitle: Concept C\nsources:\n  - raw/sources/a.txt\n---\n\n# Concept C\n",
		"wiki/location/e.md": "---\ntype: entity\ntitle: Entity E\nsources:\n  - raw/sources/a.txt\n---\n\n# Entity E\n",
	}
	for rel, content := range pages {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "index.md"), []byte("# polluted\n\nmodel prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RebuildIndex(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"## Sources", "## Concepts", "## Entities", "## Syntheses", "## Code", "[[sources/a|Z Source]]", "[[sources/b|A Source]]", "[[concept/c|Concept C]]", "[[location/e|Entity E]]"} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "model prose") {
		t.Fatalf("polluted aggregate prose survived rebuild:\n%s", content)
	}
	if strings.Index(content, "[[a|Z Source]]") > strings.Index(content, "[[b|A Source]]") {
		t.Fatalf("source summaries must be ordered by stable path, not model title:\n%s", content)
	}
}
