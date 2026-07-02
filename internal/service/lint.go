package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hejw/knowledge-core/internal/core"
)

type LintIssue struct {
	Type   string
	Path   string
	Detail string
}

var wikiLinkPattern = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]+)?\]\]`)

func LintWiki(projectPath string) ([]LintIssue, error) {
	pages, err := loadWikiPages(projectPath)
	if err != nil {
		return nil, err
	}
	linkTargets := wikiLinkTargets(pages)
	inbound := map[string]int{}
	for _, page := range pages {
		inbound[page.RelPath] = 0
	}
	knownMentions := knownPageMentions(pages)
	var issues []LintIssue
	for _, page := range pages {
		links := wikiLinkPattern.FindAllStringSubmatch(page.Content, -1)
		linkedTargets := map[string]bool{}
		for _, link := range links {
			target, ok := linkTargets[normalizeLinkID(link[1])]
			if !ok {
				issues = append(issues, LintIssue{
					Type:   "broken-link",
					Path:   page.RelPath,
					Detail: "Missing target [[" + link[1] + "]].",
				})
				continue
			}
			inbound[target]++
			linkedTargets[target] = true
		}
		if len(links) == 0 && !isSpecialWikiID(page.ID) {
			issues = append(issues, LintIssue{
				Type:   "no-outlinks",
				Path:   page.RelPath,
				Detail: "Page has no wikilinks to related knowledge.",
			})
		}
		if !isSpecialWikiID(page.ID) {
			issues = append(issues, missingLinkIssues(page, linkedTargets, knownMentions)...)
		}
	}
	for _, page := range pages {
		if isSpecialWikiID(page.ID) || wikiPageType(page.Content) == "source-summary" {
			continue
		}
		if inbound[page.RelPath] == 0 {
			issues = append(issues, LintIssue{
				Type:   "orphan",
				Path:   page.RelPath,
				Detail: "Page has no inbound wikilinks.",
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Type == issues[j].Type {
			return issues[i].Path < issues[j].Path
		}
		return issues[i].Type < issues[j].Type
	})
	return issues, nil
}

func wikiLinkTargets(pages map[string]wikiPageContent) map[string]string {
	targets := map[string]linkTarget{}
	for _, page := range pages {
		addWikiLinkTarget(targets, page.RelPath, page.RelPath, 1000)
		addWikiLinkTarget(targets, strings.TrimSuffix(page.RelPath, ".md"), page.RelPath, 1000)
		title := titleFromMarkdown(page.Content, page.ID)
		priority := pageLinkPriority(page)
		addWikiLinkTarget(targets, page.ID, page.RelPath, priority)
		addWikiLinkTarget(targets, title, page.RelPath, priority)
		for _, alias := range aliasesFromMarkdown(page.Content) {
			addWikiLinkTarget(targets, alias, page.RelPath, priority)
		}
	}
	out := map[string]string{}
	for key, target := range targets {
		out[key] = target.ID
	}
	return out
}

type linkTarget struct {
	ID       string
	Priority int
}

func addWikiLinkTarget(targets map[string]linkTarget, value, id string, priority int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	setWikiLinkTarget(targets, normalizeLinkID(value), id, priority)
	slug := core.Slug(value)
	if slug != "untitled" {
		setWikiLinkTarget(targets, normalizeLinkID(slug), id, priority)
	}
}

func setWikiLinkTarget(targets map[string]linkTarget, key, id string, priority int) {
	if key == "" {
		return
	}
	existing, ok := targets[key]
	if !ok || priority > existing.Priority || (priority == existing.Priority && id < existing.ID) {
		targets[key] = linkTarget{ID: id, Priority: priority}
	}
}

func pageLinkPriority(page wikiPageContent) int {
	switch wikiPageType(page.Content) {
	case "entity":
		return 90
	case "concept":
		return 80
	case "synthesis":
		return 70
	case "code-symbol", "code-module", "code-flow", "code-impact", "code-overview":
		return 65
	case "source-summary":
		return 40
	default:
		return 50
	}
}

type knownMention struct {
	TargetKey string
	LinkID    string
	Title     string
	Term      string
}

func knownPageMentions(pages map[string]wikiPageContent) []knownMention {
	seen := map[string]bool{}
	linkTargets := wikiLinkTargets(pages)
	var mentions []knownMention
	for _, page := range pages {
		if isSpecialWikiID(page.ID) || wikiPageType(page.Content) == "source-summary" {
			continue
		}
		title := titleFromMarkdown(page.Content, page.ID)
		for _, term := range append([]string{title}, aliasesFromMarkdown(page.Content)...) {
			term = strings.TrimSpace(term)
			if !isUsefulMentionTerm(term) {
				continue
			}
			targetKey := page.RelPath
			if canonical, ok := linkTargets[normalizeLinkID(term)]; ok {
				targetKey = canonical
			}
			if targetKey != page.RelPath {
				continue
			}
			key := targetKey + "\x00" + strings.ToLower(term)
			if seen[key] {
				continue
			}
			seen[key] = true
			mentions = append(mentions, knownMention{
				TargetKey: targetKey,
				LinkID:    page.ID,
				Title:     title,
				Term:      strings.ToLower(term),
			})
		}
	}
	sort.Slice(mentions, func(i, j int) bool {
		if len([]rune(mentions[i].Term)) == len([]rune(mentions[j].Term)) {
			return mentions[i].Term < mentions[j].Term
		}
		return len([]rune(mentions[i].Term)) > len([]rune(mentions[j].Term))
	})
	return mentions
}

func missingLinkIssues(page wikiPageContent, linkedTargets map[string]bool, mentions []knownMention) []LintIssue {
	body := strings.ToLower(markdownBody(page.Content))
	reportedTargets := map[string]bool{}
	var issues []LintIssue
	for _, mention := range mentions {
		if mention.TargetKey == page.RelPath || linkedTargets[mention.TargetKey] || reportedTargets[mention.TargetKey] {
			continue
		}
		if containsMentionTerm(body, mention.Term) {
			issues = append(issues, LintIssue{
				Type: "missing-link",
				Path: page.RelPath,
				Detail: fmt.Sprintf("Known page [[%s|%s]] is mentioned as %q but not linked.",
					mention.LinkID, mention.Title, mention.Term),
			})
			reportedTargets[mention.TargetKey] = true
		}
	}
	return issues
}

func containsMentionTerm(body, term string) bool {
	if cjkRuneCount(term) > 0 {
		return strings.Contains(body, term)
	}
	searchFrom := 0
	for {
		idx := strings.Index(body[searchFrom:], term)
		if idx < 0 {
			return false
		}
		start := searchFrom + idx
		end := start + len(term)
		if isTermBoundary(body, start, -1) && isTermBoundary(body, end, 1) {
			return true
		}
		searchFrom = start + 1
	}
}

func isTermBoundary(text string, offset int, direction int) bool {
	if offset <= 0 || offset >= len(text) {
		return true
	}
	var r rune
	if direction < 0 {
		r, _ = utf8.DecodeLastRuneInString(text[:offset])
	} else {
		r, _ = utf8.DecodeRuneInString(text[offset:])
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func markdownBody(content string) string {
	content = strings.TrimLeft(content, "\ufeff\r\n\t ")
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	rest := strings.TrimPrefix(content, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return content
	}
	return rest[end+len("\n---"):]
}

func isUsefulMentionTerm(term string) bool {
	term = strings.TrimSpace(term)
	if term == "" {
		return false
	}
	runes := []rune(term)
	if len(runes) < 3 && cjkRuneCount(term) < 2 {
		return false
	}
	return true
}

func isSpecialWikiID(id string) bool {
	return id == "index" || id == "log" || id == "overview" || id == "reviews"
}

type wikiPageContent struct {
	ID      string
	RelPath string
	Content string
}

func loadWikiPages(projectPath string) (map[string]wikiPageContent, error) {
	out := map[string]wikiPageContent{}
	root := filepath.Join(projectPath, "wiki")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(projectPath, path)
		id := strings.TrimSuffix(filepath.Base(path), ".md")
		relPath := filepath.ToSlash(rel)
		out[relPath] = wikiPageContent{
			ID:      id,
			RelPath: relPath,
			Content: string(data),
		}
		return nil
	})
	return out, err
}

func normalizeLinkID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
