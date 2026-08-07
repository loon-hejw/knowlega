package service

import (
	"encoding/json"
	"path/filepath"
	"testing"

	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
)

func TestDeterministicIngestDoesNotEraseLLMManifestLedger(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "source.md")
	key := manifestfile.Key(source)
	before := manifestfile.File{Version: 1, Sources: map[string]manifestfile.Entry{
		key: {
			OriginalPath: key, PipelineVersion: 3, SHA256: "same", RawPath: "raw/sources/same.md",
			Files:                    []string{"wiki/sources/source.md", "wiki/entities/entity.md"},
			GenerationContractSHA256: "contract", NewPageBudget: 3, NewPageCount: 1,
			CreatedPages: []string{"wiki/entities/entity.md"}, ReviewCount: 2,
			Extraction: json.RawMessage(`{"source_ext":".md","extractor":"direct"}`),
		},
	}}
	if err := manifestfile.Save(root, before); err != nil {
		t.Fatal(err)
	}
	if err := upsertIngestSourceManifest(root, source, "raw/sources/same.md", "Source", "same", []string{"wiki/sources/source.md"}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := manifestfile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := after.Sources[key]
	if entry.PipelineVersion != 3 || entry.GenerationContractSHA256 != "contract" || entry.NewPageBudget != 3 || entry.NewPageCount != 1 || len(entry.CreatedPages) != 1 || entry.ReviewCount != 2 {
		t.Fatalf("LLM manifest ledger regressed: %+v", entry)
	}
	if len(entry.Files) != 2 {
		t.Fatalf("files=%v", entry.Files)
	}
}
