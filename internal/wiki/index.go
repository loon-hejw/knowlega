package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var indexSections = []string{"Sources", "Concepts", "Entities", "Syntheses", "Code"}

// RebuildIndex regenerates the navigation catalog from durable Markdown pages.
// It intentionally does not trust append history or model-authored aggregate
// prose, so restarts and page migrations converge to the same index.
func RebuildIndex(projectPath string) error {
	pages, err := ScanWikiPages(ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return err
	}
	bySection := make(map[string][]indexPage)
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/log.md" || page.Path == "wiki/reviews.md" {
			continue
		}
		section := indexSection(page.Path, page.Type)
		if section == "" {
			continue
		}
		title := strings.TrimSpace(page.Title)
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(page.Path), ".md")
		}
		bySection[section] = append(bySection[section], indexPage{Path: page.Path, Title: title})
	}
	var b strings.Builder
	b.WriteString("---\ntype: synthesis\ntitle: Wiki Index\naliases:\n  - Index\nsources: []\nconfidence: EXTRACTED\n---\n\n# Wiki Index\n")
	for _, section := range indexSections {
		items := bySection[section]
		sort.Slice(items, func(i, j int) bool {
			if section == "Sources" {
				return items[i].Path < items[j].Path
			}
			left := strings.ToLower(items[i].Title)
			right := strings.ToLower(items[j].Title)
			if left == right {
				return items[i].Path < items[j].Path
			}
			return left < right
		})
		b.WriteString("\n## " + section + "\n\n")
		for _, item := range items {
			target := strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(item.Path), "wiki/"), ".md")
			label := strings.ReplaceAll(item.Title, "|", "\\|")
			fmt.Fprintf(&b, "- [[%s|%s]] (`%s`)\n", target, label, filepath.ToSlash(item.Path))
		}
	}
	return WriteVersionedPage(projectPath, "wiki/index.md", []byte(b.String()), "rebuild deterministic wiki index")
}

type indexPage struct {
	Path  string
	Title string
}

func indexSection(path, pageType string) string {
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "wiki/code/") {
		return "Code"
	}
	switch strings.TrimSpace(pageType) {
	case "source-summary":
		return "Sources"
	case "concept":
		return "Concepts"
	case "entity":
		return "Entities"
	case "synthesis":
		return "Syntheses"
	}
	return ""
}

func AppendIndexEntry(projectPath, section, title, rel string) error {
	if strings.TrimSpace(section) == "" {
		section = "Other"
	}
	path := filepath.Join(projectPath, "wiki", "index.md")
	contentBytes, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		contentBytes = []byte("# Index\n\n")
	} else if err != nil {
		return err
	}
	content := string(contentBytes)
	entry := fmt.Sprintf("- [[%s|%s]] - `%s`\n", strings.TrimSuffix(filepath.Base(rel), ".md"), title, filepath.ToSlash(rel))
	if strings.Contains(content, entry) {
		return nil
	}
	updated := insertIndexEntry(content, section, entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func insertIndexEntry(content, section, entry string) string {
	lines := strings.SplitAfter(content, "\n")
	heading := "## " + section
	sectionStart := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			sectionStart = i
			break
		}
	}
	if sectionStart < 0 {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if !strings.HasSuffix(content, "\n\n") {
			content += "\n"
		}
		return content + heading + "\n\n" + entry
	}
	insertAt := len(lines)
	for i := sectionStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			insertAt = i
			break
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	if insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) != "" {
		out = append(out, "\n")
	}
	out = append(out, entry)
	if insertAt < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[insertAt]), "## ") {
		out = append(out, "\n")
	}
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "")
}
