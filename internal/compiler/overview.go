package compiler

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/promptbudget"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type OverviewInput struct {
	Purpose         string
	Schema          string
	Index           string
	CurrentOverview string
	PageExcerpts    string
}

type OverviewProvider interface {
	SynthesizeOverview(OverviewInput) (string, error)
}

func RefreshOverview(provider Provider, projectPath string) (bool, error) {
	selected, ok := provider.(OverviewProvider)
	if !ok {
		return false, nil
	}
	input := OverviewInput{
		Purpose:         readOptional(filepath.Join(projectPath, "purpose.md")),
		Schema:          readOptional(filepath.Join(projectPath, "schema.md")),
		Index:           readOptional(filepath.Join(projectPath, "wiki", "index.md")),
		CurrentOverview: readOptional(filepath.Join(projectPath, "wiki", "overview.md")),
		PageExcerpts:    overviewPageExcerpts(projectPath),
	}
	content, err := selected.SynthesizeOverview(input)
	if err != nil {
		return false, err
	}
	content = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(content), "```markdown"), "```"))
	if content == "" || !strings.HasPrefix(content, "#") {
		return false, fmt.Errorf("overview synthesis returned invalid markdown")
	}
	if err := wiki.WriteVersionedPage(projectPath, "wiki/overview.md", []byte(content+"\n"), "batch ingest: refresh global overview"); err != nil {
		return false, err
	}
	return true, nil
}

func overviewPageExcerpts(projectPath string) string {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return ""
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Path < pages[j].Path })
	var b strings.Builder
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/log.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/reviews.md" {
			continue
		}
		fmt.Fprintf(&b, "\n## %s | %s | %s\nSources: %s\n%s\n", page.Path, page.Type, page.Title, strings.Join(page.Sources, ", "), promptbudget.TrimEnd(page.Body, 700))
		if b.Len() >= 90000 {
			break
		}
	}
	return b.String()
}
