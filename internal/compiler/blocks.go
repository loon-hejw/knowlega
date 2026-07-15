package compiler

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
	"gopkg.in/yaml.v3"
)

var generatedWikiLinkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

type FileBlock struct {
	Path    string
	Content string
}

func ValidateGeneratedBlocks(projectPath string, parsed ParsedBlocks, sourceRel string) error {
	if len(parsed.Files) == 0 {
		return fmt.Errorf("generation returned no file blocks")
	}
	sourceRel = filepath.ToSlash(strings.TrimSpace(sourceRel))
	summaries := 0
	seen := map[string]bool{}
	for _, file := range parsed.Files {
		if isProtectedWikiPath(file.Path) {
			return fmt.Errorf("generated file block may not overwrite protected aggregate %s", file.Path)
		}
		if seen[file.Path] {
			return fmt.Errorf("duplicate file block %s", file.Path)
		}
		seen[file.Path] = true
		frontmatter, err := parseGeneratedFrontmatter(file.Content)
		if err != nil {
			return fmt.Errorf("file block %s: %w", file.Path, err)
		}
		pageType, _ := frontmatter["type"].(string)
		title, _ := frontmatter["title"].(string)
		if strings.TrimSpace(pageType) == "" || strings.TrimSpace(title) == "" {
			return fmt.Errorf("file block %s requires non-empty type and title", file.Path)
		}
		if err := validateGeneratedTypePath(file.Path, pageType); err != nil {
			return err
		}
		if pageType == "source-summary" {
			if !strings.HasPrefix(file.Path, "wiki/sources/") {
				return fmt.Errorf("source-summary %s must be under wiki/sources/", file.Path)
			}
			summaries++
		}
		if !frontmatterContainsSource(frontmatter["sources"], sourceRel) {
			return fmt.Errorf("file block %s sources must include current source %s", file.Path, sourceRel)
		}
	}
	if summaries != 1 {
		return fmt.Errorf("generation must contain exactly one source-summary; got %d", summaries)
	}
	if err := validateGeneratedWikilinks(projectPath, parsed); err != nil {
		return err
	}
	return nil
}

func validateGeneratedWikilinks(projectPath string, parsed ParsedBlocks) error {
	missing := missingGeneratedWikilinks(projectPath, parsed)
	if len(missing) > 0 {
		if len(missing) > 8 {
			missing = missing[:8]
		}
		return fmt.Errorf("generated wikilinks have no existing or generated target: %s", strings.Join(missing, ", "))
	}
	return nil
}

func missingGeneratedWikilinks(projectPath string, parsed ParsedBlocks) []string {
	known := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		known[normalizeGeneratedLink(value)] = true
		slug := core.Slug(value)
		if slug != "untitled" {
			known[normalizeGeneratedLink(slug)] = true
		}
	}
	if pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath}); err == nil {
		for _, page := range pages {
			add(page.Path)
			add(strings.TrimSuffix(page.Path, ".md"))
			wikiRelative := strings.TrimPrefix(page.Path, "wiki/")
			add(wikiRelative)
			add(strings.TrimSuffix(wikiRelative, ".md"))
			add(strings.TrimSuffix(filepath.Base(page.Path), ".md"))
			add(page.Title)
			for _, alias := range frontmatterStrings(page.Frontmatter["aliases"]) {
				add(alias)
			}
		}
	}
	for _, file := range parsed.Files {
		frontmatter, _ := parseGeneratedFrontmatter(file.Content)
		add(file.Path)
		add(strings.TrimSuffix(file.Path, ".md"))
		add(strings.TrimSuffix(filepath.Base(file.Path), ".md"))
		add(fmt.Sprint(frontmatter["title"]))
		for _, alias := range frontmatterStrings(frontmatter["aliases"]) {
			add(alias)
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, file := range parsed.Files {
		for _, match := range generatedWikiLinkPattern.FindAllStringSubmatch(file.Content, -1) {
			target := strings.TrimSpace(strings.SplitN(strings.SplitN(match[1], "|", 2)[0], "#", 2)[0])
			if target == "" || known[normalizeGeneratedLink(target)] || seen[target] {
				continue
			}
			seen[target] = true
			missing = append(missing, target)
		}
	}
	return missing
}

func downgradeMissingWikilinks(projectPath string, parsed *ParsedBlocks) []string {
	missing := missingGeneratedWikilinks(projectPath, *parsed)
	if len(missing) == 0 {
		return nil
	}
	missingSet := make(map[string]bool, len(missing))
	for _, target := range missing {
		missingSet[normalizeGeneratedLink(target)] = true
	}
	for index := range parsed.Files {
		parsed.Files[index].Content = downgradeWikiLinksInText(parsed.Files[index].Content, missingSet)
	}
	return missing
}

func downgradeMissingReviewWikilinks(projectPath string, parsed *ParsedBlocks) []string {
	if parsed == nil || len(parsed.Reviews) == 0 {
		return nil
	}
	missingSet := map[string]bool{}
	var missing []string
	for _, review := range parsed.Reviews {
		probe := ParsedBlocks{Files: append(append([]FileBlock(nil), parsed.Files...), FileBlock{Path: "wiki/reviews.md", Content: review.Body})}
		for _, target := range missingGeneratedWikilinks(projectPath, probe) {
			key := normalizeGeneratedLink(target)
			if !missingSet[key] {
				missingSet[key] = true
				missing = append(missing, target)
			}
		}
	}
	if len(missingSet) == 0 {
		return nil
	}
	for index := range parsed.Reviews {
		parsed.Reviews[index].Body = downgradeWikiLinksInText(parsed.Reviews[index].Body, missingSet)
	}
	return missing
}

func downgradeWikiLinksInText(content string, missingSet map[string]bool) string {
	return generatedWikiLinkPattern.ReplaceAllStringFunc(content, func(link string) string {
		match := generatedWikiLinkPattern.FindStringSubmatch(link)
		if len(match) != 2 {
			return link
		}
		parts := strings.SplitN(match[1], "|", 2)
		target := strings.TrimSpace(strings.SplitN(parts[0], "#", 2)[0])
		if !missingSet[normalizeGeneratedLink(target)] {
			return link
		}
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1])
		}
		return target
	})
}

func normalizeGeneratedLink(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".md")
	return strings.ToLower(value)
}

func frontmatterStrings(value any) []string {
	var out []string
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			out = append(out, strings.TrimSpace(fmt.Sprint(item)))
		}
	case []string:
		out = append(out, values...)
	case string:
		out = append(out, values)
	}
	return out
}

func parseGeneratedFrontmatter(content string) (map[string]any, error) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "---\n") {
		return nil, fmt.Errorf("missing YAML frontmatter")
	}
	rest := strings.TrimPrefix(trimmed, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("unterminated YAML frontmatter")
	}
	frontmatter := map[string]any{}
	if err := yaml.Unmarshal([]byte(rest[:end]), &frontmatter); err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	return frontmatter, nil
}

func frontmatterContainsSource(value any, sourceRel string) bool {
	switch sources := value.(type) {
	case []any:
		for _, source := range sources {
			if filepath.ToSlash(strings.TrimSpace(fmt.Sprint(source))) == sourceRel {
				return true
			}
		}
	case []string:
		for _, source := range sources {
			if filepath.ToSlash(strings.TrimSpace(source)) == sourceRel {
				return true
			}
		}
	case string:
		return filepath.ToSlash(strings.TrimSpace(sources)) == sourceRel
	}
	return false
}

type ReviewBlock struct {
	Type  string
	Title string
	Body  string
}

type ParsedBlocks struct {
	Files   []FileBlock
	Reviews []ReviewBlock
}

func canonicalizeGeneratedPaths(parsed ParsedBlocks) ParsedBlocks {
	for index := range parsed.Files {
		frontmatter, err := parseGeneratedFrontmatter(parsed.Files[index].Content)
		if err != nil {
			continue
		}
		pageType := strings.TrimSpace(fmt.Sprint(frontmatter["type"]))
		parsed.Files[index].Path = canonicalWikiPathForType(parsed.Files[index].Path, pageType)
	}
	return parsed
}

func canonicalWikiPathForType(path, pageType string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if isProtectedWikiPath(path) {
		return path
	}
	dir := ""
	switch strings.TrimSpace(pageType) {
	case "source-summary":
		dir = "sources"
	case "entity":
		dir = "entities"
	case "location":
		dir = "entities"
	case "concept":
		dir = "concepts"
	case "synthesis":
		dir = "syntheses"
	}
	if dir != "" {
		return "wiki/" + dir + "/" + filepath.Base(path)
	}
	return path
}

func isProtectedWikiPath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "wiki/index.md" || path == "wiki/overview.md" || path == "wiki/log.md" || path == "wiki/reviews.md" {
		return true
	}
	base := strings.ToLower(filepath.Base(path))
	return strings.HasPrefix(path, "wiki/syntheses/") && (base == "index.md" || base == "overview.md")
}

func validateGeneratedTypePath(path, pageType string) error {
	prefix := ""
	switch strings.TrimSpace(pageType) {
	case "source-summary":
		prefix = "wiki/sources/"
	case "entity":
		prefix = "wiki/entities/"
	case "concept":
		prefix = "wiki/concepts/"
	case "synthesis":
		prefix = "wiki/syntheses/"
	default:
		return fmt.Errorf("file block %s has unsupported type %q", path, pageType)
	}
	if !strings.HasPrefix(path, prefix) {
		return fmt.Errorf("file block %s type %s must be under %s", path, pageType, prefix)
	}
	return nil
}

func ParseBlocks(text string) (ParsedBlocks, error) {
	lines := strings.Split(text, "\n")
	var parsed ParsedBlocks
	var currentFile *FileBlock
	var currentReview *ReviewBlock
	var body []string

	flush := func() {
		if currentFile != nil {
			currentFile.Content = strings.TrimSpace(strings.Join(body, "\n")) + "\n"
			parsed.Files = append(parsed.Files, *currentFile)
		}
		if currentReview != nil {
			currentReview.Body = strings.TrimSpace(strings.Join(body, "\n"))
			parsed.Reviews = append(parsed.Reviews, *currentReview)
		}
		currentFile = nil
		currentReview = nil
		body = nil
	}

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "---FILE:"):
			flush()
			path := strings.TrimSpace(strings.TrimPrefix(line, "---FILE:"))
			if err := validateWikiFilePath(path); err != nil {
				return ParsedBlocks{}, err
			}
			currentFile = &FileBlock{Path: filepath.ToSlash(path)}
		case strings.HasPrefix(line, "---REVIEW:"):
			flush()
			header := strings.TrimSpace(strings.TrimPrefix(line, "---REVIEW:"))
			parts := strings.SplitN(header, "|", 2)
			reviewType := strings.TrimSpace(parts[0])
			title := ""
			if len(parts) == 2 {
				title = strings.TrimSpace(parts[1])
			}
			if reviewType == "" || title == "" {
				return ParsedBlocks{}, fmt.Errorf("invalid review block header %q", line)
			}
			currentReview = &ReviewBlock{Type: reviewType, Title: title}
		default:
			if currentFile != nil || currentReview != nil {
				body = append(body, line)
			}
		}
	}
	flush()

	for _, file := range parsed.Files {
		if !hasFrontmatter(file.Content) {
			return ParsedBlocks{}, fmt.Errorf("file block %s is missing YAML frontmatter", file.Path)
		}
	}
	for index := range parsed.Files {
		normalized, err := normalizeGeneratedFrontmatter(parsed.Files[index].Content)
		if err != nil {
			return ParsedBlocks{}, fmt.Errorf("file block %s: %w", parsed.Files[index].Path, err)
		}
		parsed.Files[index].Content = normalized
	}
	return parsed, nil
}

// normalizeGeneratedFrontmatter repairs harmless duplicate YAML keys before
// semantic validation. Conflicting scalar values remain errors and are sent
// through the focused LLM repair path.
func normalizeGeneratedFrontmatter(content string) (string, error) {
	trimmed := strings.TrimSpace(content)
	rest := strings.TrimPrefix(trimmed, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return "", fmt.Errorf("unterminated YAML frontmatter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rest[:end]), &doc); err != nil {
		return "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if len(doc.Content) == 0 || len(doc.Content[0].Content)%2 != 0 {
		return "", fmt.Errorf("frontmatter must be a YAML mapping")
	}
	mapping := doc.Content[0]
	values := map[string][]*yaml.Node{}
	order := []string{}
	for i := 0; i < len(mapping.Content); i += 2 {
		key := strings.TrimSpace(mapping.Content[i].Value)
		if _, ok := values[key]; !ok {
			order = append(order, key)
		}
		values[key] = append(values[key], mapping.Content[i+1])
	}
	out := map[string]any{}
	for _, key := range order {
		nodes := values[key]
		if key == "sources" || key == "aliases" {
			var merged []string
			for _, node := range nodes {
				var value any
				if err := node.Decode(&value); err != nil {
					return "", err
				}
				merged = append(merged, frontmatterStrings(value)...)
			}
			out[key] = sortedUniqueStrings(merged)
			continue
		}
		var first any
		if err := nodes[0].Decode(&first); err != nil {
			return "", err
		}
		firstYAML, _ := yaml.Marshal(first)
		for _, node := range nodes[1:] {
			var next any
			if err := node.Decode(&next); err != nil {
				return "", err
			}
			nextYAML, _ := yaml.Marshal(next)
			if string(firstYAML) != string(nextYAML) {
				return "", fmt.Errorf("conflicting duplicate YAML key %q", key)
			}
		}
		out[key] = first
	}
	encoded, err := yaml.Marshal(out)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(rest[end+len("\n---\n"):])
	return "---\n" + strings.TrimSpace(string(encoded)) + "\n---\n\n" + body + "\n", nil
}

func validateWikiFilePath(path string) error {
	clean := filepath.ToSlash(filepath.Clean(path))
	if strings.HasPrefix(clean, "../") || clean == ".." || filepath.IsAbs(clean) {
		return fmt.Errorf("file block path escapes project: %s", path)
	}
	if !strings.HasPrefix(clean, "wiki/") {
		return fmt.Errorf("file block path must be under wiki/: %s", path)
	}
	if filepath.Ext(clean) != ".md" {
		return fmt.Errorf("file block path must be markdown: %s", path)
	}
	return nil
}

func hasFrontmatter(content string) bool {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---\n") {
		return false
	}
	rest := strings.TrimPrefix(content, "---\n")
	return strings.Contains(rest, "\n---\n")
}
