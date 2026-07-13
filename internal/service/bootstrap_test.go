package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestPrepareProjectInitializesAndReusesWithoutCompile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		path := filepath.Join(sources, fmt.Sprintf("source-%d.md", i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("# Source %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	compileCalls := 0
	compile := func(projectPath, sourcePath string) (ProjectCompileResult, error) {
		compileCalls++
		return writeCompleteBootstrapFixture(t, projectPath, sourcePath), nil
	}
	result, err := PrepareProject(PrepareProjectOptions{
		ProjectPath: root, ProjectName: "demo", SourcePath: sources, ReuseExisting: true, Compile: compile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reused || result.SourceCount != 2 || compileCalls != 1 {
		t.Fatalf("result=%+v compileCalls=%d", result, compileCalls)
	}
	result, err = PrepareProject(PrepareProjectOptions{
		ProjectPath: root, ProjectName: "demo", SourcePath: sources, ReuseExisting: true,
		Compile: func(_, _ string) (ProjectCompileResult, error) {
			compileCalls++
			return ProjectCompileResult{}, fmt.Errorf("must not compile")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.SourceCount != 2 || compileCalls != 1 {
		t.Fatalf("reuse result=%+v compileCalls=%d", result, compileCalls)
	}
}

func TestPrepareProjectRejectsExistingIncompleteAndReuseFalse(t *testing.T) {
	sources := filepath.Join(t.TempDir(), "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sources, "source.md"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareProject(PrepareProjectOptions{ProjectPath: root, SourcePath: sources, ReuseExisting: true})
	if err == nil || !strings.Contains(err.Error(), "existing project is incomplete") {
		t.Fatalf("incomplete error=%v", err)
	}
	_, err = PrepareProject(PrepareProjectOptions{ProjectPath: root, SourcePath: sources, ReuseExisting: false})
	if err == nil || !strings.Contains(err.Error(), "reuse_existing is false") {
		t.Fatalf("reuse false error=%v", err)
	}
}

func TestPrepareProjectPreservesEvidenceOnCompileFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareProject(PrepareProjectOptions{
		ProjectPath: root, ProjectName: "demo", SourcePath: source, ReuseExisting: true,
		Compile: func(projectPath, _ string) (ProjectCompileResult, error) {
			if err := os.WriteFile(filepath.Join(projectPath, "partial-evidence.txt"), []byte("kept"), 0o644); err != nil {
				t.Fatal(err)
			}
			return ProjectCompileResult{}, fmt.Errorf("provider failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "generated evidence was preserved") {
		t.Fatalf("compile error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "partial-evidence.txt")); statErr != nil {
		t.Fatalf("partial evidence was removed: %v", statErr)
	}
	result, err := PrepareProject(PrepareProjectOptions{
		ProjectPath: root, ProjectName: "demo", SourcePath: source, ReuseExisting: true,
		Compile: func(projectPath, sourcePath string) (ProjectCompileResult, error) {
			return writeCompleteBootstrapFixture(t, projectPath, sourcePath), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.Reused || result.SourceCount != 1 {
		t.Fatalf("resume result=%+v", result)
	}
	if _, statErr := os.Stat(filepath.Join(root, "partial-evidence.txt")); statErr != nil {
		t.Fatalf("resume removed partial evidence: %v", statErr)
	}
}

func TestPrepareProjectResumesCompleteStalePipelineManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "stale-pipeline"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("# Source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCompleteBootstrapFixture(t, root, source)
	manifest, err := loadSourceManifestFile(root)
	if err != nil {
		t.Fatal(err)
	}
	for key, entry := range manifest.Sources {
		entry.PipelineVersion = core.SourceManifestPipelineVersion - 1
		manifest.Sources[key] = entry
	}
	if err := saveSourceManifestFile(root, manifest); err != nil {
		t.Fatal(err)
	}

	compileCalls := 0
	result, err := PrepareProject(PrepareProjectOptions{
		ProjectPath: root, ProjectName: "stale-pipeline", SourcePath: source, ReuseExisting: true,
		Compile: func(projectPath, sourcePath string) (ProjectCompileResult, error) {
			compileCalls++
			return writeCompleteBootstrapFixture(t, projectPath, sourcePath), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.Reused || compileCalls != 1 {
		t.Fatalf("result=%+v compileCalls=%d", result, compileCalls)
	}
}

func writeCompleteBootstrapFixture(t *testing.T, projectPath, sourcePath string) ProjectCompileResult {
	t.Helper()
	sources, err := bootstrapSourceFiles(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := struct {
		Version int                       `json:"version"`
		Sources map[string]map[string]any `json:"sources"`
	}{Version: 1, Sources: map[string]map[string]any{}}
	var index strings.Builder
	index.WriteString("# demo Index\n\n## Sources\n\n")
	written := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}
	for _, source := range sources {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		base := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
		rawPath := filepath.ToSlash(filepath.Join("raw", "sources", filepath.Base(source)))
		pagePath := filepath.ToSlash(filepath.Join("wiki", "sources", base+".md"))
		if err := os.WriteFile(filepath.Join(projectPath, filepath.FromSlash(rawPath)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		page := fmt.Sprintf("---\ntype: \"source-summary\"\ntitle: \"%s\"\nsources:\n  - \"%s\"\nconfidence: \"EXTRACTED\"\n---\n\n# %s\n\nSee [[overview]].\n", base, rawPath, base)
		if err := os.WriteFile(filepath.Join(projectPath, filepath.FromSlash(pagePath)), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&index, "- [[%s]]\n", base)
		sum := sha256.Sum256(data)
		abs, _ := filepath.Abs(source)
		manifest.Sources[filepath.ToSlash(abs)] = map[string]any{
			"original_path": filepath.ToSlash(abs), "sha256": hex.EncodeToString(sum[:]),
			"raw_path": rawPath, "title": base, "files": []string{pagePath}, "review_count": 0,
			"pipeline_version": core.SourceManifestPipelineVersion,
		}
		written = append(written, pagePath)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "wiki", "index.md"), []byte(index.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectPath, ".kbcore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, ".kbcore", "source-manifest.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return ProjectCompileResult{SourceCount: len(sources), FileCount: len(sources), WrittenPaths: written}
}
