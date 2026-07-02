package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ProjectOptions struct {
	Path string
	Name string
}

func InitProject(opts ProjectOptions) error {
	if strings.TrimSpace(opts.Path) == "" {
		return fmt.Errorf("project path is required")
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = filepath.Base(opts.Path)
	}

	dirs := []string{
		"raw/sources",
		"raw/code-graphs",
		"wiki/sources",
		"wiki/code",
		"wiki/syntheses",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(opts.Path, dir), 0o755); err != nil {
			return err
		}
	}

	files := map[string]string{
		"purpose.md": fmt.Sprintf("# %s Purpose\n\nDefine the knowledge base goal, scope, questions, and evolving thesis here.\n", name),
		"schema.md":  defaultSchema(),
		"wiki/index.md": fmt.Sprintf(`# %s Index

## Sources

## Code

`, name),
		"wiki/log.md":      fmt.Sprintf("# Log\n\n## [%s] init | %s\n\nProject initialized.\n", time.Now().Format("2006-01-02"), name),
		"wiki/overview.md": fmt.Sprintf("# %s Overview\n\nThis page is maintained from ingested sources and code graph snapshots.\n", name),
		"wiki/reviews.md":  "# Reviews\n\nOpen LLM review items, contradictions, missing pages, and human judgment calls.\n",
	}
	for rel, content := range files {
		path := filepath.Join(opts.Path, rel)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func defaultSchema() string {
	return `# Schema

This knowledge base follows the persistent LLM Wiki pattern.

## Layers

- raw sources are immutable.
- wiki pages are generated and maintained artifacts.
- this schema and purpose.md guide future maintenance.

## Page Types

- source-summary
- concept
- entity
- synthesis
- code-overview
- code-module
- code-flow
- code-symbol
- code-impact

## Conventions

- Use YAML frontmatter on generated pages.
- Use [[wikilink]] references for durable cross-links.
- Keep source provenance in frontmatter sources.
- Use aliases frontmatter for common names, alternate spellings, chapter names,
  symbol names, and user-facing wording that should recall the page.
- Append operational history to wiki/log.md.
- Prefer deterministic lint checks before LLM judgment.
`
}
