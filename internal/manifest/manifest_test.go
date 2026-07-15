package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripPreservesGenerationLedgerVersionsAndOwners(t *testing.T) {
	root := t.TempDir()
	value := File{Version: 1, Sources: map[string]Entry{
		"source": {
			OriginalPath: "source", PipelineVersion: 3, SHA256: "current", RawPath: "raw/sources/current.md",
			Files: []string{"wiki/sources/current.md"}, GenerationContractSHA256: "contract",
			NewPageBudget: 3, NewPageCount: 2, CreatedPages: []string{"wiki/entities/a.md"},
			Extraction: json.RawMessage(`{"source_ext":".md","extractor":"direct","future_field":true}`),
			Versions:   []SourceVersion{{SHA256: "old", RawPath: "raw/sources/old.md"}},
		},
	}, PageOwners: map[string]PageOwnership{
		"wiki/entities/manual.md": {ManagedBy: "manual"},
	}}
	if err := Save(root, value); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := got.Sources["source"]
	if entry.GenerationContractSHA256 != "contract" || entry.NewPageBudget != 3 || entry.NewPageCount != 2 || len(entry.CreatedPages) != 1 {
		t.Fatalf("generation ledger was not preserved: %+v", entry)
	}
	if len(entry.Versions) != 1 || entry.Versions[0].RawPath != "raw/sources/old.md" {
		t.Fatalf("versions=%+v", entry.Versions)
	}
	if !json.Valid(entry.Extraction) || string(entry.Extraction) == "" {
		t.Fatalf("extraction=%s", entry.Extraction)
	}
	if got.PageOwners[filepath.ToSlash("wiki/entities/manual.md")].ManagedBy != "manual" {
		t.Fatalf("owners=%+v", got.PageOwners)
	}
}

func TestAppendPriorVersionKeepsImmutableLineage(t *testing.T) {
	previous := Entry{SHA256: "a", RawPath: "raw/sources/a.md", Versions: []SourceVersion{{SHA256: "older", RawPath: "raw/sources/older.md"}}}
	next := Entry{SHA256: "b", RawPath: "raw/sources/b.md"}
	AppendPriorVersion(previous, &next)
	if len(next.Versions) != 2 || next.Versions[1].SHA256 != "a" {
		t.Fatalf("versions=%+v", next.Versions)
	}
}

func TestLoadMigratesOwnershipForCurrentAndHistoricalVersionFiles(t *testing.T) {
	root := t.TempDir()
	path := Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{
  "version": 1,
  "sources": {
    "source": {
      "sha256": "current",
      "raw_path": "raw/sources/current.md",
      "files": ["wiki/entities/current.md"],
      "versions": [{
        "sha256": "old",
        "raw_path": "raw/sources/old.md",
        "files": ["wiki/entities/historical.md"]
      }]
    }
  }
}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	value, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"wiki/entities/current.md", "wiki/entities/historical.md"} {
		owner := value.PageOwners[page]
		if owner.ManagedBy != "source" || len(owner.SourceKeys) != 1 || owner.SourceKeys[0] != "source" {
			t.Fatalf("owner %s=%+v", page, owner)
		}
	}
}
