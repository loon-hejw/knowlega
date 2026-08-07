package service

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

// RepairKnownMentionLinks adds deterministic wikilinks only when lint has an
// unambiguous, low-frequency page identity for a plain-text mention. It skips
// headings, code, Markdown links, and existing wikilinks.
func RepairKnownMentionLinks(projectPath string) (int, error) {
	release, err := acquireServiceProjectLock(projectPath)
	if err != nil {
		return 0, err
	}
	defer release()
	pages, err := loadWikiPages(projectPath)
	if err != nil {
		return 0, err
	}
	linkTargets := wikiLinkTargets(pages)
	mentions := knownPageMentions(pages)
	paths := make([]string, 0, len(pages))
	for path := range pages {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	linked := 0
	for _, path := range paths {
		page := pages[path]
		if isSpecialWikiID(page.ID) {
			continue
		}
		linkedTargets := map[string]bool{}
		for _, match := range wikiLinkPattern.FindAllStringSubmatch(page.Content, -1) {
			if target, ok := linkTargets[normalizeLinkID(match[1])]; ok {
				linkedTargets[target] = true
			}
		}
		missing := missingLinkMentions(page, linkedTargets, mentions)
		if len(missing) == 0 {
			continue
		}
		bodyStart := markdownBodyStart(page.Content)
		body := page.Content[bodyStart:]
		pageLinks := 0
		for _, mention := range missing {
			start, end, ok := findRepairableMention(body, mention.Term)
			if !ok {
				continue
			}
			display := body[start:end]
			if strings.ContainsAny(display, "[]|\r\n") {
				continue
			}
			link := "[[" + mention.LinkID + "|" + display + "]]"
			body = body[:start] + link + body[end:]
			pageLinks++
		}
		if pageLinks == 0 {
			continue
		}
		updated := page.Content[:bodyStart] + body
		if err := wiki.WriteVersionedPage(projectPath, filepath.ToSlash(page.RelPath), []byte(updated), "link unambiguous known page mentions"); err != nil {
			return linked, err
		}
		linked += pageLinks
	}
	return linked, nil
}

func markdownBodyStart(content string) int {
	trimmed := strings.TrimLeft(content, "\ufeff\r\n\t ")
	leading := len(content) - len(trimmed)
	if !strings.HasPrefix(trimmed, "---\n") {
		return 0
	}
	restStart := leading + len("---\n")
	end := strings.Index(content[restStart:], "\n---")
	if end < 0 {
		return 0
	}
	return restStart + end + len("\n---")
}

func findRepairableMention(body, term string) (int, int, bool) {
	searchable := strings.ToLower(mentionSearchBody(body))
	term = strings.ToLower(term)
	searchFrom := 0
	for {
		index := strings.Index(searchable[searchFrom:], term)
		if index < 0 {
			return 0, 0, false
		}
		start := searchFrom + index
		end := start + len(term)
		if end <= len(body) && (cjkRuneCount(term) > 0 || isTermBoundary(searchable, start, -1) && isTermBoundary(searchable, end, 1)) {
			return start, end, true
		}
		searchFrom = start + 1
	}
}
