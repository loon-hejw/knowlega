package sourcearchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

const (
	MetadataFileName = "metadata.json"
	LayoutVersion    = 1
)

// Metadata is the durable description of one immutable source archive. Paths
// are project-relative and use slash separators so the archive remains
// portable between operating systems.
type Metadata struct {
	Version          int    `json:"version"`
	SourceID         string `json:"source_id"`
	ArchivePath      string `json:"archive_path"`
	OriginalName     string `json:"original_name"`
	OriginalLocation string `json:"original_location,omitempty"`
	OriginalRawPath  string `json:"original_raw_path"`
	ContentPath      string `json:"content_path"`
	OriginalSHA256   string `json:"original_sha256"`
	ContentSHA256    string `json:"content_sha256"`
	SourceExt        string `json:"source_ext,omitempty"`
	MediaType        string `json:"media_type,omitempty"`
	Size             int64  `json:"size"`
	ImportedAt       string `json:"imported_at"`
	ExtractedAt      string `json:"extracted_at,omitempty"`
	Extractor        string `json:"extractor"`
	Status           string `json:"status"`
}

type Archive struct {
	Metadata Metadata
	AbsDir   string
}

type ImportOptions struct {
	ProjectPath      string
	Collection       string
	RelativePath     string
	OriginalLocation string
	Reader           io.Reader
}

// ImportReader imports original bytes exactly once. Re-importing the same
// collection/name/content returns the existing archive; different content gets
// a different hash-suffixed directory.
func ImportReader(opts ImportOptions) (Archive, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return Archive{}, fmt.Errorf("project path is required")
	}
	if opts.Reader == nil {
		return Archive{}, fmt.Errorf("source reader is required")
	}
	relative, err := cleanRelative(opts.RelativePath, false)
	if err != nil {
		return Archive{}, fmt.Errorf("invalid source path: %w", err)
	}
	collection, err := cleanRelative(opts.Collection, true)
	if err != nil {
		return Archive{}, fmt.Errorf("invalid collection: %w", err)
	}
	projectAbs, rawRoot, err := projectRoots(opts.ProjectPath)
	if err != nil {
		return Archive{}, err
	}
	collection = path.Join(collection, path.Dir(relative))
	if collection == "." {
		collection = ""
	}
	collectionAbs := filepath.Join(rawRoot, filepath.FromSlash(collection))
	if !inside(rawRoot, collectionAbs) {
		return Archive{}, fmt.Errorf("collection escapes raw/sources: %s", collection)
	}
	if err := os.MkdirAll(collectionAbs, 0o755); err != nil {
		return Archive{}, err
	}

	tmpFile, err := os.CreateTemp(collectionAbs, ".source-import-*")
	if err != nil {
		return Archive{}, err
	}
	tmpFileName := tmpFile.Name()
	defer func() { _ = os.Remove(tmpFileName) }()
	h := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmpFile, h), opts.Reader)
	closeErr := tmpFile.Close()
	if copyErr != nil {
		return Archive{}, copyErr
	}
	if closeErr != nil {
		return Archive{}, closeErr
	}
	hash := hex.EncodeToString(h.Sum(nil))
	originalName := safeBase(path.Base(relative))
	slug := core.Slug(originalName)
	archiveName := slug + "-" + hash[:12]
	archiveRel := slashJoin("raw/sources", collection, archiveName)
	archiveAbs := filepath.Join(projectAbs, filepath.FromSlash(archiveRel))
	if !inside(rawRoot, archiveAbs) {
		return Archive{}, fmt.Errorf("archive path escapes raw/sources: %s", archiveRel)
	}
	if info, statErr := os.Stat(archiveAbs); statErr == nil && info.IsDir() {
		archive, loadErr := Load(opts.ProjectPath, archiveRel)
		if loadErr != nil {
			return Archive{}, fmt.Errorf("existing source archive is invalid: %w", loadErr)
		}
		if archive.Metadata.OriginalSHA256 != hash {
			return Archive{}, fmt.Errorf("existing source archive hash mismatch: %s", archiveRel)
		}
		if err := os.Chmod(filepath.Join(projectAbs, filepath.FromSlash(archive.Metadata.OriginalRawPath)), 0o444); err != nil {
			return Archive{}, err
		}
		return archive, nil
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return Archive{}, statErr
	}

	tmpDir, err := os.MkdirTemp(collectionAbs, ".archive-build-*")
	if err != nil {
		return Archive{}, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	originalDir := filepath.Join(tmpDir, "original")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		return Archive{}, err
	}
	originalAbs := filepath.Join(originalDir, originalName)
	if err := renameOrCopy(tmpFileName, originalAbs); err != nil {
		return Archive{}, err
	}
	if err := os.Chmod(originalAbs, 0o444); err != nil {
		return Archive{}, err
	}
	originalRel := slashJoin(archiveRel, "original", originalName)
	ext := strings.ToLower(filepath.Ext(originalName))
	mediaType := mime.TypeByExtension(ext)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	metadata := Metadata{
		Version:          LayoutVersion,
		SourceID:         core.StableID("source-archive", archiveRel),
		ArchivePath:      archiveRel,
		OriginalName:     originalName,
		OriginalLocation: filepath.ToSlash(strings.TrimSpace(opts.OriginalLocation)),
		OriginalRawPath:  originalRel,
		ContentPath:      originalRel,
		OriginalSHA256:   hash,
		ContentSHA256:    hash,
		SourceExt:        ext,
		MediaType:        mediaType,
		Size:             size,
		ImportedAt:       now,
		Extractor:        "direct",
		Status:           "ready",
	}
	if ext == ".pdf" || ext == ".docx" {
		metadata.Extractor = "pending"
		metadata.Status = "pending_extraction"
	}
	if err := writeMetadata(filepath.Join(tmpDir, MetadataFileName), metadata); err != nil {
		return Archive{}, err
	}
	if err := os.Rename(tmpDir, archiveAbs); err != nil {
		if info, statErr := os.Stat(archiveAbs); statErr == nil && info.IsDir() {
			return Load(opts.ProjectPath, archiveRel)
		}
		return Archive{}, err
	}
	return Archive{Metadata: metadata, AbsDir: archiveAbs}, nil
}

func ImportBytes(opts ImportOptions, data []byte) (Archive, error) {
	opts.Reader = bytes.NewReader(data)
	return ImportReader(opts)
}

// SetExtracted persists normalized Markdown next to the immutable original and
// makes it the archive's content path. The original file is never changed.
func SetExtracted(projectPath, sourcePath string, content []byte, extractor string) (Archive, error) {
	archive, ok, err := FindBySourcePath(projectPath, sourcePath)
	if err != nil {
		return Archive{}, err
	}
	if !ok {
		return Archive{}, fmt.Errorf("source is not inside an archive: %s", sourcePath)
	}
	contentRel := slashJoin(archive.Metadata.ArchivePath, "extracted.md")
	contentAbs := filepath.Join(projectPath, filepath.FromSlash(contentRel))
	if !inside(archive.AbsDir, contentAbs) {
		return Archive{}, fmt.Errorf("extracted path escapes archive: %s", contentRel)
	}
	if err := writeFileAtomic(contentAbs, content, 0o644); err != nil {
		return Archive{}, err
	}
	h := sha256.Sum256(content)
	archive.Metadata.ContentPath = contentRel
	archive.Metadata.ContentSHA256 = hex.EncodeToString(h[:])
	archive.Metadata.Extractor = strings.TrimSpace(extractor)
	if archive.Metadata.Extractor == "" {
		archive.Metadata.Extractor = "unknown"
	}
	archive.Metadata.ExtractedAt = time.Now().UTC().Format(time.RFC3339)
	archive.Metadata.Status = "ready"
	if err := writeMetadata(filepath.Join(archive.AbsDir, MetadataFileName), archive.Metadata); err != nil {
		return Archive{}, err
	}
	return archive, nil
}

// Load reads and validates an archive metadata file. archivePath may be either
// the archive directory or its metadata.json path.
func Load(projectPath, archivePath string) (Archive, error) {
	projectAbs, rawRoot, err := projectRoots(projectPath)
	if err != nil {
		return Archive{}, err
	}
	archivePath = filepath.ToSlash(strings.TrimSpace(archivePath))
	if strings.HasSuffix(archivePath, "/"+MetadataFileName) {
		archivePath = strings.TrimSuffix(archivePath, "/"+MetadataFileName)
	}
	archiveAbs := archivePath
	if !filepath.IsAbs(archiveAbs) {
		archiveAbs = filepath.Join(projectAbs, filepath.FromSlash(archivePath))
	}
	archiveAbs, err = filepath.Abs(archiveAbs)
	if err != nil {
		return Archive{}, err
	}
	if !inside(rawRoot, archiveAbs) || archiveAbs == rawRoot {
		return Archive{}, fmt.Errorf("archive must be under raw/sources: %s", archivePath)
	}
	data, err := os.ReadFile(filepath.Join(archiveAbs, MetadataFileName))
	if err != nil {
		return Archive{}, err
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Archive{}, fmt.Errorf("read source archive metadata: %w", err)
	}
	if metadata.Version != LayoutVersion {
		return Archive{}, fmt.Errorf("unsupported source archive version %d", metadata.Version)
	}
	rel, err := filepath.Rel(projectAbs, archiveAbs)
	if err != nil {
		return Archive{}, err
	}
	actualRel := filepath.ToSlash(rel)
	if metadata.ArchivePath != actualRel {
		return Archive{}, fmt.Errorf("archive_path mismatch: metadata=%s actual=%s", metadata.ArchivePath, actualRel)
	}
	for label, relPath := range map[string]string{"original_raw_path": metadata.OriginalRawPath, "content_path": metadata.ContentPath} {
		abs := filepath.Join(projectAbs, filepath.FromSlash(relPath))
		if !inside(archiveAbs, abs) || abs == archiveAbs {
			return Archive{}, fmt.Errorf("%s escapes archive: %s", label, relPath)
		}
		if _, err := os.Stat(abs); err != nil {
			return Archive{}, fmt.Errorf("%s %s: %w", label, relPath, err)
		}
	}
	return Archive{Metadata: metadata, AbsDir: archiveAbs}, nil
}

// FindBySourcePath walks from a raw source path to the nearest metadata.json.
// Legacy flat sources return ok=false without an error.
func FindBySourcePath(projectPath, sourcePath string) (Archive, bool, error) {
	projectAbs, rawRoot, err := projectRoots(projectPath)
	if err != nil {
		return Archive{}, false, err
	}
	abs := sourcePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projectAbs, filepath.FromSlash(sourcePath))
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return Archive{}, false, err
	}
	if !inside(rawRoot, abs) || abs == rawRoot {
		return Archive{}, false, nil
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	for inside(rawRoot, abs) && abs != rawRoot {
		metadataPath := filepath.Join(abs, MetadataFileName)
		if _, statErr := os.Stat(metadataPath); statErr == nil {
			archive, loadErr := Load(projectPath, abs)
			return archive, loadErr == nil, loadErr
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return Archive{}, false, statErr
		}
		abs = filepath.Dir(abs)
	}
	return Archive{}, false, nil
}

func Discover(projectPath string) ([]Archive, error) {
	_, rawRoot, err := projectRoots(projectPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(rawRoot); os.IsNotExist(err) {
		return nil, nil
	}
	var archives []Archive
	err = filepath.WalkDir(rawRoot, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Name() != MetadataFileName {
			return nil
		}
		archive, loadErr := Load(projectPath, filepath.Dir(current))
		if loadErr != nil {
			return loadErr
		}
		archives = append(archives, archive)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].Metadata.ArchivePath < archives[j].Metadata.ArchivePath
	})
	return archives, nil
}

func cleanRelative(value string, allowEmpty bool) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("parent path segments are not allowed")
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path is required")
	}
	return cleaned, nil
}

func safeBase(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "source"
	}
	return name
}

func projectRoots(projectPath string) (string, string, error) {
	projectAbs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", "", err
	}
	rawRoot, err := filepath.Abs(filepath.Join(projectAbs, "raw", "sources"))
	if err != nil {
		return "", "", err
	}
	return projectAbs, rawRoot, nil
}

func inside(root, child string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	childAbs, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, childAbs)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func slashJoin(parts ...string) string {
	var nonEmpty []string
	for _, part := range parts {
		part = strings.Trim(strings.ReplaceAll(part, "\\", "/"), "/")
		if part != "" && part != "." {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return path.Join(nonEmpty...)
}

func renameOrCopy(source, dest string) error {
	if err := os.Rename(source, dest); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func writeMetadata(filename string, metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filename, data, 0o644)
}

func writeFileAtomic(filename string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}
