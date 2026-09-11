package postgres

import (
	"net/url"
	"strings"
	"testing"
)

func TestSearchPathDSNSetsKnowledgeSchemaAndKeepsExistingParams(t *testing.T) {
	got := SearchPathDSN("postgres://kbcore:kbcore@127.0.0.1:55433/kbcore?sslmode=disable")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse knowledge DSN: %v", err)
	}
	if searchPath := parsed.Query().Get("search_path"); searchPath != SearchPath {
		t.Fatalf("search_path = %q, want %q", searchPath, SearchPath)
	}
	if sslMode := parsed.Query().Get("sslmode"); sslMode != "disable" {
		t.Fatalf("sslmode = %q, want disable", sslMode)
	}
}

func TestSearchPathDSNKeepsPublicForSharedExtensions(t *testing.T) {
	// The `vector` extension lives in `public`; dropping it from the search
	// path breaks every embedding column.
	if !strings.HasSuffix(SearchPath, ",public") {
		t.Fatalf("SearchPath = %q, want it to keep public as a fallback", SearchPath)
	}
	if !strings.HasPrefix(SearchPath, Schema+",") {
		t.Fatalf("SearchPath = %q, want %q first so writes land there", SearchPath, Schema)
	}
}

func TestSearchPathDSNOverridesAnExistingSearchPath(t *testing.T) {
	got := SearchPathDSN("postgres://u:p@h:5432/db?search_path=public")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse knowledge DSN: %v", err)
	}
	if values := parsed.Query()["search_path"]; len(values) != 1 || values[0] != SearchPath {
		t.Fatalf("search_path = %v, want exactly [%q]", values, SearchPath)
	}
}

func TestSearchPathDSNHandlesKeywordValueDSN(t *testing.T) {
	got := SearchPathDSN("host=127.0.0.1 port=55433 dbname=kbcore")

	if !strings.Contains(got, "search_path="+SearchPath) {
		t.Fatalf("SearchPathDSN(%q) = %q, want it to carry the knowledge search_path", "host=...", got)
	}
}