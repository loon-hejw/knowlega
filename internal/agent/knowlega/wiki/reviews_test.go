package wiki

import (
	"strings"
	"testing"
)

func TestParseReviewItems(t *testing.T) {
	items := ParseReviewItems("project-1", `# Reviews

## [2026-07-02] contradiction | Conflicting OAuth owners

- Source: `+"`raw/sources/oauth.md`"+`
- Source title: OAuth Notes
- Status: open
- Resolved action: manual

### Affected Pages

- `+"`wiki/concepts/oauth.md`"+`
- `+"`wiki/entities/auth-service.md`"+`
### Detail

SEARCH: OAuth owner | AuthService token validation
OAuth ownership conflicts between two source notes.
`)
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	item := items[0]
	if item.ProjectID != "project-1" || item.Type != "contradiction" || item.Title != "Conflicting OAuth owners" {
		t.Fatalf("identity=%+v", item)
	}
	if item.Status != "open" || item.Severity != "info" {
		t.Fatalf("status/severity=%+v", item)
	}
	if item.SourcePath != "raw/sources/oauth.md" || item.ResolvedAction != "manual" {
		t.Fatalf("source/action=%+v", item)
	}
	if len(item.AffectedPages) != 2 || item.AffectedPages[0] != "wiki/concepts/oauth.md" {
		t.Fatalf("affected=%+v", item.AffectedPages)
	}
	if len(item.SearchQueries) != 2 || item.SearchQueries[0] != "OAuth owner" {
		t.Fatalf("queries=%+v", item.SearchQueries)
	}
	if len(item.Options) == 0 {
		t.Fatalf("options missing: %+v", item)
	}
	if !strings.Contains(item.Description, "ownership conflicts") {
		t.Fatalf("description=%q", item.Description)
	}
	if item.ID == "" || item.CreatedAt.IsZero() {
		t.Fatalf("id/date missing: %+v", item)
	}
}

func TestParseReviewItemsKeepsRepeatedTitleTasksDistinct(t *testing.T) {
	items := ParseReviewItems("project-1", `# Reviews

## [2026-07-14] review-needed | Durability gate rejected wiki/entities/孙悟空.md

- Source: `+"`raw/sources/chapter-002.txt`"+`
- Status: resolved

### Affected Pages

- `+"`wiki/entities/孙悟空.md`"+`
### Detail

Earlier task.

## [2026-07-14] review-needed | Durability gate rejected wiki/entities/孙悟空.md

- Source: `+"`raw/sources/chapter-046.txt`"+`
- Status: open

### Affected Pages

- `+"`wiki/entities/孙悟空.md`"+`
### Detail

Later task.
`)
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].ID == items[1].ID {
		t.Fatalf("distinct Markdown review tasks share id %q", items[0].ID)
	}
	if items[0].Status != "resolved" || items[1].Status != "open" {
		t.Fatalf("statuses=%q,%q", items[0].Status, items[1].Status)
	}
}
