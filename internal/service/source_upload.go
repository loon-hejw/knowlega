package service

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/hejw/knowledge-core/internal/sourcearchive"
)

type UploadSourceInput struct {
	RelativePath string
	Reader       io.Reader
}

type UploadSourcesOptions struct {
	ProjectPath string
	TargetDir   string
	Queue       bool
	Files       []UploadSourceInput
}

type UploadedSourceFile struct {
	Path         string `json:"path"`
	ArchivePath  string `json:"archive_path"`
	OriginalPath string `json:"original_path"`
	ContentPath  string `json:"content_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	QueuedTaskID string `json:"queued_task_id,omitempty"`
}

type UploadSourcesResult struct {
	Uploaded    []UploadedSourceFile      `json:"uploaded"`
	Queued      int                       `json:"queued"`
	Unsupported []UnsupportedSourceImport `json:"unsupported,omitempty"`
}

func UploadSources(opts UploadSourcesOptions) (UploadSourcesResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return UploadSourcesResult{}, fmt.Errorf("project path is required")
	}
	if len(opts.Files) == 0 {
		return UploadSourcesResult{}, fmt.Errorf("at least one file is required")
	}
	targetDir, err := cleanUploadPath(opts.TargetDir, true)
	if err != nil {
		return UploadSourcesResult{}, fmt.Errorf("invalid target dir: %w", err)
	}
	projectAbs, err := filepath.Abs(opts.ProjectPath)
	if err != nil {
		return UploadSourcesResult{}, err
	}
	result := UploadSourcesResult{}
	for _, file := range opts.Files {
		relFile, err := cleanUploadPath(file.RelativePath, false)
		if err != nil {
			return UploadSourcesResult{}, fmt.Errorf("invalid file path %q: %w", file.RelativePath, err)
		}
		if file.Reader == nil {
			return UploadSourcesResult{}, fmt.Errorf("file %q reader is required", relFile)
		}
		archive, err := sourcearchive.ImportReader(sourcearchive.ImportOptions{
			ProjectPath:      projectAbs,
			Collection:       targetDir,
			RelativePath:     relFile,
			OriginalLocation: path.Join(targetDir, relFile),
			Reader:           file.Reader,
		})
		if err != nil {
			return UploadSourcesResult{}, err
		}
		uploaded := UploadedSourceFile{
			Path:         archive.Metadata.OriginalRawPath,
			ArchivePath:  archive.Metadata.ArchivePath,
			OriginalPath: archive.Metadata.OriginalRawPath,
			ContentPath:  archive.Metadata.ContentPath,
			Size:         archive.Metadata.Size,
			SHA256:       archive.Metadata.OriginalSHA256,
		}
		ext := strings.ToLower(filepath.Ext(archive.Metadata.OriginalName))
		if isSupportedIngestSourceExt(ext) {
			if opts.Queue {
				task, err := QueueIngestSource(QueueIngestOptions{
					ProjectPath: projectAbs,
					SourcePath:  filepath.Join(projectAbs, filepath.FromSlash(archive.Metadata.OriginalRawPath)),
					Title:       strings.TrimSuffix(archive.Metadata.OriginalName, filepath.Ext(archive.Metadata.OriginalName)),
				})
				if err != nil {
					return UploadSourcesResult{}, err
				}
				uploaded.QueuedTaskID = task.ID
				result.Queued++
			}
		} else {
			result.Unsupported = append(result.Unsupported, UnsupportedSourceImport{
				Path:   uploaded.Path,
				Reason: "unsupported extension " + ext,
			})
		}
		result.Uploaded = append(result.Uploaded, uploaded)
	}
	return result, nil
}

func cleanUploadPath(value string, allowEmpty bool) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if trimmed == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(trimmed, "/") || path.IsAbs(trimmed) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", fmt.Errorf("parent path segments are not allowed")
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path is required")
	}
	return cleaned, nil
}

func isPathInside(rootAbs, childAbs string) bool {
	rel, err := filepath.Rel(rootAbs, childAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
