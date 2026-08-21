package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File is the single durable schema for .kbcore/source-manifest.json. Keep all
// readers and writers on this type so a partial decoder cannot erase fields it
// does not know about when the manifest is written back.
type File struct {
	Version    int                      `json:"version"`
	Sources    map[string]Entry         `json:"sources"`
	PageOwners map[string]PageOwnership `json:"page_owners,omitempty"`
}

type Entry struct {
	QMFileID                 string          `json:"qm_file_id,omitempty"`
	QMProjectID              string          `json:"qm_project_id,omitempty"`
	QMScopeID                string          `json:"qm_scope_id,omitempty"`
	QMSourceSHA256           string          `json:"qm_source_sha256,omitempty"`
	OriginalPath             string          `json:"original_path"`
	PipelineVersion          int             `json:"pipeline_version,omitempty"`
	SHA256                   string          `json:"sha256"`
	RawPath                  string          `json:"raw_path"`
	ArchivePath              string          `json:"archive_path,omitempty"`
	OriginalRawPath          string          `json:"original_raw_path,omitempty"`
	ContentPath              string          `json:"content_path,omitempty"`
	OriginalSHA256           string          `json:"original_sha256,omitempty"`
	ContentSHA256            string          `json:"content_sha256,omitempty"`
	Title                    string          `json:"title"`
	Files                    []string        `json:"files"`
	GenerationContractSHA256 string          `json:"generation_contract_sha256,omitempty"`
	NewPageBudget            int             `json:"new_page_budget,omitempty"`
	NewPageCount             int             `json:"new_page_count,omitempty"`
	CreatedPages             []string        `json:"created_pages,omitempty"`
	ReviewCount              int             `json:"review_count"`
	UpdatedAt                string          `json:"updated_at"`
	Extraction               json.RawMessage `json:"extraction,omitempty"`
	Versions                 []SourceVersion `json:"versions,omitempty"`
}

// SourceVersion preserves immutable provenance when the same logical input
// path is imported again with different content. Entry always describes the
// current version; Versions contains prior versions, oldest first.
type SourceVersion struct {
	SHA256          string          `json:"sha256"`
	RawPath         string          `json:"raw_path"`
	ArchivePath     string          `json:"archive_path,omitempty"`
	OriginalRawPath string          `json:"original_raw_path,omitempty"`
	ContentPath     string          `json:"content_path,omitempty"`
	OriginalSHA256  string          `json:"original_sha256,omitempty"`
	ContentSHA256   string          `json:"content_sha256,omitempty"`
	Files           []string        `json:"files,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
	Extraction      json.RawMessage `json:"extraction,omitempty"`
}

// PageOwnership distinguishes compiler-managed pages from durable pages made
// by people or other supported workflows. Unknown pages are intentionally not
// assumed to be disposable.
type PageOwnership struct {
	ManagedBy  string   `json:"managed_by"`
	SourceKeys []string `json:"source_keys,omitempty"`
}

func Load(projectPath string) (File, error) {
	value := File{Version: 1, Sources: map[string]Entry{}, PageOwners: map[string]PageOwnership{}}
	data, err := os.ReadFile(Path(projectPath))
	if os.IsNotExist(err) {
		return value, nil
	}
	if err != nil {
		return File{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return value, nil
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return File{}, fmt.Errorf("read source manifest: %w", err)
	}
	if value.Version == 0 {
		value.Version = 1
	}
	if value.Sources == nil {
		value.Sources = map[string]Entry{}
	}
	if value.PageOwners == nil {
		value.PageOwners = map[string]PageOwnership{}
	}
	// Migrate manifests written before explicit ownership existed. Files listed
	// by source entries were compiler managed; unrelated pages remain unknown
	// and therefore human-preserved.
	for key, entry := range value.Sources {
		for _, pagePath := range OwnedFiles(entry) {
			owner, ok := value.PageOwners[filepath.ToSlash(pagePath)]
			if !ok || owner.ManagedBy == "source" {
				RegisterPageOwner(&value, pagePath, "source", key)
			}
		}
	}
	return value, nil
}

// OwnedFiles returns every live page ever attributed to the logical source,
// including pages emitted by retained immutable source versions. A corrected
// current version must not silently orphan still-readable historical evidence.
func OwnedFiles(entry Entry) []string {
	files := append([]string(nil), entry.Files...)
	for _, version := range entry.Versions {
		files = append(files, version.Files...)
	}
	return unique(files)
}

// ActiveOwnedFiles is the subset that may remain in the live wiki. Historical
// entity/concept/synthesis evidence remains durable, but a logical source has
// exactly one live source-summary: the summary listed by the current entry.
func ActiveOwnedFiles(entry Entry) []string {
	files := append([]string(nil), entry.Files...)
	for _, version := range entry.Versions {
		for _, pagePath := range version.Files {
			pagePath = filepath.ToSlash(strings.TrimSpace(pagePath))
			if strings.HasPrefix(pagePath, "wiki/sources/") {
				continue
			}
			files = append(files, pagePath)
		}
	}
	return unique(files)
}

func Save(projectPath string, value File) error {
	if value.Version == 0 {
		value.Version = 1
	}
	if value.Sources == nil {
		value.Sources = map[string]Entry{}
	}
	if value.PageOwners == nil {
		value.PageOwners = map[string]PageOwnership{}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := Path(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".source-manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func Path(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "source-manifest.json")
}

func Key(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(abs)
}

func PriorVersion(entry Entry) SourceVersion {
	return SourceVersion{
		SHA256: entry.SHA256, RawPath: entry.RawPath, ArchivePath: entry.ArchivePath,
		OriginalRawPath: entry.OriginalRawPath, ContentPath: entry.ContentPath,
		OriginalSHA256: entry.OriginalSHA256, ContentSHA256: entry.ContentSHA256,
		Files: append([]string(nil), entry.Files...), UpdatedAt: entry.UpdatedAt,
		Extraction: append(json.RawMessage(nil), entry.Extraction...),
	}
}

func AppendPriorVersion(previous Entry, next *Entry) {
	if next == nil {
		return
	}
	next.Versions = append([]SourceVersion(nil), previous.Versions...)
	if strings.TrimSpace(previous.SHA256) == "" || previous.SHA256 == next.SHA256 {
		return
	}
	for _, version := range next.Versions {
		if version.SHA256 == previous.SHA256 && filepath.ToSlash(version.RawPath) == filepath.ToSlash(previous.RawPath) {
			return
		}
	}
	next.Versions = append(next.Versions, PriorVersion(previous))
}

func RegisterPageOwner(value *File, pagePath, managedBy, sourceKey string) {
	if value == nil {
		return
	}
	if value.PageOwners == nil {
		value.PageOwners = map[string]PageOwnership{}
	}
	pagePath = filepath.ToSlash(strings.TrimSpace(pagePath))
	if pagePath == "" {
		return
	}
	owner := value.PageOwners[pagePath]
	managedBy = strings.TrimSpace(managedBy)
	if managedBy != "" && (owner.ManagedBy == "" || managedBy != "source" || owner.ManagedBy == "source") {
		owner.ManagedBy = managedBy
	}
	if sourceKey != "" {
		owner.SourceKeys = append(owner.SourceKeys, filepath.ToSlash(sourceKey))
	}
	owner.SourceKeys = unique(owner.SourceKeys)
	value.PageOwners[pagePath] = owner
}

func RemovePageOwner(value *File, pagePath string) {
	if value != nil && value.PageOwners != nil {
		delete(value.PageOwners, filepath.ToSlash(strings.TrimSpace(pagePath)))
	}
}

func UnregisterSourceOwner(value *File, pagePath, sourceKey string) {
	if value == nil || value.PageOwners == nil {
		return
	}
	pagePath = filepath.ToSlash(strings.TrimSpace(pagePath))
	owner, ok := value.PageOwners[pagePath]
	if !ok {
		return
	}
	var remaining []string
	for _, key := range owner.SourceKeys {
		if filepath.ToSlash(key) != filepath.ToSlash(sourceKey) {
			remaining = append(remaining, key)
		}
	}
	owner.SourceKeys = unique(remaining)
	if owner.ManagedBy == "source" && len(owner.SourceKeys) == 0 {
		delete(value.PageOwners, pagePath)
		return
	}
	value.PageOwners[pagePath] = owner
}

func unique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = filepath.ToSlash(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
