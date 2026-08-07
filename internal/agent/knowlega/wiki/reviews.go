package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

var reviewHeadingPattern = regexp.MustCompile(`^## \[([0-9]{4}-[0-9]{2}-[0-9]{2})\] ([^|]+)\| (.+)$`)

func ScanReviewItems(opts ScanOptions) ([]core.ReviewItem, error) {
	path := filepath.Join(opts.ProjectPath, "wiki", "reviews.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseReviewItems(opts.ProjectID, string(data)), nil
}

func ParseReviewItems(projectID, content string) []core.ReviewItem {
	lines := strings.Split(content, "\n")
	var items []core.ReviewItem
	for i := 0; i < len(lines); i++ {
		match := reviewHeadingPattern.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if len(match) != 4 {
			continue
		}
		start := i + 1
		end := len(lines)
		for j := start; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "## [") {
				end = j
				break
			}
		}
		item := parseReviewItemSection(projectID, match[1], strings.TrimSpace(match[2]), strings.TrimSpace(match[3]), lines[start:end])
		if item.ID != "" {
			items = append(items, item)
		}
		i = end - 1
	}
	return items
}

func UpdateReviewItemStatus(projectPath, projectID, id, status string, resolvedAt time.Time) (core.ReviewItem, error) {
	return UpdateReviewItemStatusWithAction(projectPath, projectID, id, status, "", resolvedAt)
}

func UpdateReviewItemStatusWithAction(projectPath, projectID, id, status, action string, resolvedAt time.Time) (core.ReviewItem, error) {
	path := filepath.Join(projectPath, "wiki", "reviews.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return core.ReviewItem{}, err
	}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		match := reviewHeadingPattern.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if len(match) != 4 {
			continue
		}
		start := i + 1
		end := len(lines)
		for j := start; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "## [") {
				end = j
				break
			}
		}
		item := parseReviewItemSection(projectID, match[1], strings.TrimSpace(match[2]), strings.TrimSpace(match[3]), lines[start:end])
		if item.ID != id {
			i = end - 1
			continue
		}
		updatedSection := updateReviewStatusLines(lines[start:end], status, action, resolvedAt)
		updated := append([]string{}, lines[:start]...)
		updated = append(updated, updatedSection...)
		updated = append(updated, lines[end:]...)
		content := strings.Join(updated, "\n")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return core.ReviewItem{}, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return core.ReviewItem{}, err
		}
		item.Status = status
		item.ResolvedAction = action
		if !resolvedAt.IsZero() && (status == "resolved" || status == "dismissed") {
			item.ResolvedAt = &resolvedAt
		}
		return item, nil
	}
	return core.ReviewItem{}, fmt.Errorf("review item not found: %s", id)
}

func updateReviewStatusLines(lines []string, status, action string, resolvedAt time.Time) []string {
	out := append([]string(nil), lines...)
	statusLine := "- Status: " + status
	statusUpdated := false
	actionLine := ""
	if strings.TrimSpace(action) != "" {
		actionLine = "- Resolved action: " + strings.TrimSpace(action)
	}
	actionUpdated := actionLine == ""
	resolvedLine := ""
	if !resolvedAt.IsZero() && (status == "resolved" || status == "dismissed") {
		resolvedLine = "- Resolved: " + resolvedAt.UTC().Format(time.RFC3339)
	}
	resolvedUpdated := resolvedLine == ""
	insertAt := 0
	for i, line := range out {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- Status:") {
			out[i] = statusLine
			statusUpdated = true
			insertAt = i + 1
			continue
		}
		if strings.HasPrefix(trimmed, "- Resolved:") {
			if resolvedLine != "" {
				out[i] = resolvedLine
			}
			resolvedUpdated = true
		}
		if strings.HasPrefix(trimmed, "- Resolved action:") {
			if actionLine != "" {
				out[i] = actionLine
			}
			actionUpdated = true
		}
		if strings.HasPrefix(trimmed, "- Source") {
			insertAt = i + 1
		}
	}
	if !statusUpdated {
		out = insertLine(out, insertAt, statusLine)
		insertAt++
	}
	if !resolvedUpdated && resolvedLine != "" {
		out = insertLine(out, insertAt, resolvedLine)
		insertAt++
	}
	if !actionUpdated && actionLine != "" {
		out = insertLine(out, insertAt, actionLine)
	}
	return out
}

func insertLine(lines []string, idx int, line string) []string {
	if idx < 0 || idx > len(lines) {
		idx = len(lines)
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, line)
	out = append(out, lines[idx:]...)
	return out
}

func parseReviewItemSection(projectID, dateText, reviewType, title string, lines []string) core.ReviewItem {
	source := ""
	var sources []string
	status := "open"
	resolvedAt := (*time.Time)(nil)
	resolvedAction := ""
	var affected []string
	var queries []string
	var detail strings.Builder
	inAffected := false
	inDetail := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- Source:"):
			value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- Source:")), "`")
			if value != "" && !containsString(sources, value) {
				sources = append(sources, value)
				if source == "" {
					source = value
				}
			}
			inAffected = false
			inDetail = false
		case strings.HasPrefix(trimmed, "- Status:"):
			status = strings.TrimSpace(strings.TrimPrefix(trimmed, "- Status:"))
			inAffected = false
			inDetail = false
		case strings.HasPrefix(trimmed, "- Resolved:"):
			resolvedText := strings.TrimSpace(strings.TrimPrefix(trimmed, "- Resolved:"))
			if parsed, err := time.Parse(time.RFC3339, resolvedText); err == nil {
				resolvedAt = &parsed
			}
			inAffected = false
			inDetail = false
		case strings.HasPrefix(trimmed, "- Resolved action:"):
			resolvedAction = strings.TrimSpace(strings.TrimPrefix(trimmed, "- Resolved action:"))
			inAffected = false
			inDetail = false
		case trimmed == "### Affected Pages":
			inAffected = true
			inDetail = false
		case trimmed == "### Detail":
			inAffected = false
			inDetail = true
		case inAffected && strings.HasPrefix(trimmed, "- "):
			page := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), "`")
			if page != "" {
				affected = append(affected, filepath.ToSlash(page))
			}
		case inDetail:
			if strings.HasPrefix(trimmed, "SEARCH:") {
				queries = append(queries, splitSearchQueries(strings.TrimSpace(strings.TrimPrefix(trimmed, "SEARCH:")))...)
			}
			detail.WriteString(line)
			detail.WriteString("\n")
		}
	}
	createdAt := time.Time{}
	if parsed, err := time.Parse("2006-01-02", dateText); err == nil {
		createdAt = parsed
	}
	description := strings.TrimSpace(detail.String())
	identityPage := ""
	if len(affected) > 0 {
		identityPage = affected[0]
	}
	return core.ReviewItem{
		// A resolved review and a later open review may intentionally reuse the
		// same type/title. Include durable section anchors so PostgreSQL upserts
		// cannot collapse distinct Markdown tasks and status updates cannot hit
		// the wrong occurrence. Merge operations append sources/pages, preserving
		// the first source and first affected page used here.
		ID:             core.StableID(projectID, "review", reviewType, normalizeReviewTitle(title), dateText, source, identityPage),
		ProjectID:      projectID,
		Type:           reviewType,
		Title:          title,
		Description:    description,
		Severity:       "info",
		Status:         status,
		SourcePath:     source,
		SourcePaths:    sources,
		AffectedPages:  affected,
		SearchQueries:  queries,
		Options:        reviewOptions(reviewType),
		ResolvedAction: resolvedAction,
		CreatedAt:      createdAt,
		ResolvedAt:     resolvedAt,
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func splitSearchQueries(value string) []string {
	var out []string
	for _, part := range strings.Split(value, "|") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func normalizeReviewTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	replacer := strings.NewReplacer(
		"missing page:", "",
		"missing-page:", "",
		"缺失页面：", "",
		"缺失页面:", "",
		"缺少页面：", "",
		"缺少页面:", "",
	)
	title = replacer.Replace(title)
	title = strings.Join(strings.Fields(title), " ")
	return title
}

func reviewOptions(reviewType string) []core.ReviewOption {
	base := []core.ReviewOption{
		{Label: "标记解决", Action: "resolve"},
		{Label: "忽略", Action: "dismiss"},
	}
	switch strings.TrimSpace(reviewType) {
	case "missing-page":
		return append([]core.ReviewOption{
			{Label: "生成页面草稿", Action: "create-page"},
			{Label: "启动深度研究", Action: "deep-research"},
			{Label: "重新检查", Action: "sweep"},
		}, base...)
	case "source-gap", "review-needed", "suggestion":
		return append([]core.ReviewOption{
			{Label: "启动深度研究", Action: "deep-research"},
			{Label: "重新检查", Action: "sweep"},
		}, base...)
	case "duplicate", "contradiction":
		return append([]core.ReviewOption{
			{Label: "重新检查", Action: "sweep"},
		}, base...)
	default:
		return base
	}
}
