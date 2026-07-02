package compiler

import (
	"fmt"
	"path/filepath"
	"strings"
)

type FileBlock struct {
	Path    string
	Content string
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
	return parsed, nil
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
