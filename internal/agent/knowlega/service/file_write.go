package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type WriteProjectFileContentOptions struct {
	ProjectPath string
	Path        string
	Content     string
	Reason      string
}

type WriteProjectFileContentResult struct {
	Path      string `json:"path"`
	Versioned bool   `json:"versioned"`
	UpdatedAt string `json:"updated_at"`
}

func WriteProjectFileContent(opts WriteProjectFileContentOptions) (WriteProjectFileContentResult, error) {
	projectPath := strings.TrimSpace(opts.ProjectPath)
	rel := filepath.ToSlash(strings.TrimSpace(opts.Path))
	if projectPath == "" {
		return WriteProjectFileContentResult{}, fmt.Errorf("project path is required")
	}
	release, err := acquireServiceProjectLock(projectPath)
	if err != nil {
		return WriteProjectFileContentResult{}, err
	}
	defer release()
	if !isWritableProjectFile(rel) {
		return WriteProjectFileContentResult{}, fmt.Errorf("path must be purpose.md, schema.md, or wiki/...")
	}
	if rel != "purpose.md" && rel != "schema.md" && !strings.HasSuffix(strings.ToLower(rel), ".md") {
		return WriteProjectFileContentResult{}, fmt.Errorf("wiki file must be markdown: %s", rel)
	}
	cleanProject, err := filepath.Abs(projectPath)
	if err != nil {
		return WriteProjectFileContentResult{}, err
	}
	abs, err := filepath.Abs(filepath.Join(projectPath, filepath.FromSlash(rel)))
	if err != nil {
		return WriteProjectFileContentResult{}, err
	}
	if abs != cleanProject && !strings.HasPrefix(abs, cleanProject+string(os.PathSeparator)) {
		return WriteProjectFileContentResult{}, fmt.Errorf("path escapes project: %s", rel)
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		reason = "manual edit"
	}
	if strings.HasPrefix(rel, "wiki/") {
		if err := wiki.WriteVersionedPage(projectPath, rel, []byte(opts.Content), reason); err != nil {
			return WriteProjectFileContentResult{}, err
		}
		if err := registerPageOwnership(projectPath, rel, "manual"); err != nil {
			return WriteProjectFileContentResult{}, err
		}
		return WriteProjectFileContentResult{Path: rel, Versioned: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return WriteProjectFileContentResult{}, err
	}
	tmp := abs + ".tmp"
	if err := os.WriteFile(tmp, []byte(opts.Content), 0o644); err != nil {
		return WriteProjectFileContentResult{}, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		return WriteProjectFileContentResult{}, err
	}
	return WriteProjectFileContentResult{Path: rel, Versioned: false, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

func isWritableProjectFile(rel string) bool {
	return rel == "purpose.md" || rel == "schema.md" || strings.HasPrefix(rel, "wiki/")
}
