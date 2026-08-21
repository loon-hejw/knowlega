package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

var cjkPattern = regexp.MustCompile(`[\p{Han}]`)
var wikilinkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
var markdownLinkPattern = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)

func readProjectText(projectPath, rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func wikiPageType(content string) string {
	page := wiki.ParseWikiPage("service", "wiki/page.md", content)
	if strings.TrimSpace(page.Type) == "" {
		return "wiki-page"
	}
	return page.Type
}

func titleFromMarkdown(content, fallback string) string {
	page := wiki.ParseWikiPage("service", "wiki/"+fallback+".md", content)
	if strings.TrimSpace(page.Title) != "" {
		return page.Title
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "# "))
		}
	}
	return fallback
}

func titleForSearchResult(content, fallback, kind string) string {
	if kind == "raw-source" {
		var lines []string
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		for index, line := range lines {
			if index == 0 && len(lines) > 1 && isStandaloneRawWorkTitle(line) {
				continue
			}
			if strings.HasPrefix(line, "《》目录 ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "《》目录 "))
			}
			if strings.HasPrefix(line, "# ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "# "))
			}
			return line
		}
	}
	return titleFromMarkdown(content, fallback)
}

func isStandaloneRawWorkTitle(line string) bool {
	runes := []rune(strings.TrimSpace(line))
	return len(runes) > 2 && runes[0] == '《' && runes[len(runes)-1] == '》'
}

func aliasesFromMarkdown(content string) []string {
	return frontmatterStringList(wiki.ParseWikiPage("service", "wiki/page.md", content).Frontmatter, "aliases")
}

func extractWikiMarkdownLinks(content string) []string {
	seen := map[string]bool{}
	var links []string
	add := func(link string) {
		link = strings.TrimSpace(link)
		if link != "" && !seen[link] {
			seen[link] = true
			links = append(links, link)
		}
	}
	for _, match := range wikilinkPattern.FindAllStringSubmatch(content, -1) {
		add(strings.Split(match[1], "|")[0])
	}
	for _, match := range markdownLinkPattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	return links
}

func tailRunes(content string, maxRunes int) string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[len(runes)-maxRunes:])
}

func cjkRuneCount(value string) int {
	count := 0
	for _, r := range value {
		if cjkPattern.MatchString(string(r)) {
			count++
		}
	}
	return count
}

func cjkBigrams(value string) []string {
	runes := []rune(value)
	var result []string
	for i := 0; i+1 < len(runes); i++ {
		if cjkPattern.MatchString(string(runes[i])) && cjkPattern.MatchString(string(runes[i+1])) {
			result = append(result, string(runes[i:i+2]))
		}
	}
	return result
}

func isAggregateWikiPath(path string) bool { return IsAggregateKnowledgePath(path) }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
