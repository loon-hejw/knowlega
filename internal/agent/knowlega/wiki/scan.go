package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

type ScanOptions struct {
	ProjectPath string
	ProjectID   string
}

func ScanWikiPages(opts ScanOptions) ([]core.WikiPage, error) {
	root := filepath.Join(opts.ProjectPath, "wiki")
	var pages []core.WikiPage
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(opts.ProjectPath, path)
		rel = filepath.ToSlash(rel)
		page := ParseWikiPage(opts.ProjectID, rel, string(data))
		info, statErr := d.Info()
		if statErr == nil {
			page.UpdatedAt = info.ModTime()
		}
		pages = append(pages, page)
		return nil
	})
	return pages, err
}

func ScanWikiPageVersions(opts ScanOptions) ([]core.WikiPageVersion, error) {
	root := filepath.Join(opts.ProjectPath, ".kbcore", "page-versions")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var versions []core.WikiPageVersion
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		version, ok, parseErr := ParseWikiPageVersion(opts.ProjectID, string(data), path)
		if parseErr != nil {
			return parseErr
		}
		if !ok {
			return nil
		}
		if version.CreatedAt.IsZero() {
			if info, statErr := d.Info(); statErr == nil {
				version.CreatedAt = info.ModTime()
			}
		}
		versions = append(versions, version)
		return nil
	})
	return versions, err
}

func ParseWikiPageVersion(projectID, content, archivePath string) (core.WikiPageVersion, bool, error) {
	content = strings.TrimLeft(content, "\ufeff\r\n\t ")
	if !strings.HasPrefix(content, "<!-- kbcore-page-version\n") {
		return core.WikiPageVersion{}, false, nil
	}
	end := strings.Index(content, "\n-->")
	if end < 0 {
		return core.WikiPageVersion{}, false, nil
	}
	meta := parseArchiveMetadata(content[:end])
	rel := filepath.ToSlash(strings.TrimSpace(meta["original"]))
	if rel == "" {
		return core.WikiPageVersion{}, false, nil
	}
	body := strings.TrimPrefix(content[end+len("\n-->"):], "\n")
	body = strings.TrimPrefix(body, "\n")
	page := ParseWikiPage(projectID, rel, body)
	createdAt := time.Time{}
	if archived := strings.TrimSpace(meta["archived_at"]); archived != "" {
		parsed, err := time.Parse(time.RFC3339Nano, archived)
		if err != nil {
			return core.WikiPageVersion{}, false, err
		}
		createdAt = parsed
	}
	return core.WikiPageVersion{
		ID:          core.StableID(projectID, rel, filepath.Base(archivePath)),
		ProjectID:   projectID,
		PageID:      core.StableID(projectID, rel),
		Path:        rel,
		Body:        page.Body,
		Frontmatter: page.Frontmatter,
		Sources:     page.Sources,
		Reason:      strings.TrimSpace(meta["reason"]),
		CreatedAt:   createdAt,
	}, true, nil
}

func parseArchiveMetadata(header string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "<!-- kbcore-page-version" || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func ParseWikiPage(projectID, relPath, content string) core.WikiPage {
	frontmatter, body := splitFrontmatter(content)
	fm := parseFrontmatter(frontmatter)
	title := stringField(fm, "title")
	if title == "" {
		title = firstHeading(body)
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	}
	pageType := stringField(fm, "type")
	if pageType == "" {
		pageType = "page"
	}
	sources := stringListField(fm, "sources")
	if sources == nil {
		sources = []string{}
	}
	return core.WikiPage{
		ID:          core.StableID(projectID, relPath),
		ProjectID:   projectID,
		Path:        filepath.ToSlash(relPath),
		Type:        pageType,
		Title:       title,
		Body:        strings.TrimSpace(body),
		Frontmatter: fm,
		Sources:     sources,
		UpdatedAt:   time.Now(),
	}
}

func splitFrontmatter(content string) (string, string) {
	content = strings.TrimLeft(content, "\ufeff\r\n\t ")
	if !strings.HasPrefix(content, "---\n") {
		return "", content
	}
	rest := strings.TrimPrefix(content, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", content
	}
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	return rest[:end], body
}

func parseFrontmatter(frontmatter string) map[string]any {
	out := map[string]any{}
	lines := strings.Split(frontmatter, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if value != "" {
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				out[key] = splitListValue(value)
			} else {
				out[key] = trimYAMLString(value)
			}
			continue
		}
		var values []string
		for j := i + 1; j < len(lines); j++ {
			child := strings.TrimSpace(lines[j])
			if child == "" {
				continue
			}
			if !strings.HasPrefix(child, "- ") {
				break
			}
			values = append(values, trimYAMLString(strings.TrimSpace(strings.TrimPrefix(child, "- "))))
			i = j
		}
		out[key] = values
	}
	return out
}

func splitListValue(value string) []string {
	value = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(value), "]"), "[")
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = trimYAMLString(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func trimYAMLString(value string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
}

func stringField(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func stringListField(values map[string]any, key string) []string {
	value, ok := values[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return typed
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}
