package wiki

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateReviewItemStatus(t *testing.T) {
	root := t.TempDir()
	reviewsPath := filepath.Join(root, "wiki", "reviews.md")
	if err := os.MkdirAll(filepath.Dir(reviewsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `# Reviews

## [2026-07-02] missing-page | Alpha concept

- Source: ` + "`raw/sources/a.md`" + `
- Status: open

### Affected Pages

- ` + "`wiki/sources/a.md`" + `
### Detail

Create a durable page for Alpha.
`
	if err := os.WriteFile(reviewsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	items := ParseReviewItems("project", content)
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	resolvedAt := time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)
	item, err := UpdateReviewItemStatus(root, "project", items[0].ID, "resolved", resolvedAt)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "resolved" || item.ResolvedAt == nil {
		t.Fatalf("item=%+v", item)
	}
	updated, err := os.ReadFile(reviewsPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseReviewItems("project", string(updated))
	if len(parsed) != 1 || parsed[0].Status != "resolved" || parsed[0].ResolvedAt == nil {
		t.Fatalf("parsed=%+v", parsed)
	}
}
