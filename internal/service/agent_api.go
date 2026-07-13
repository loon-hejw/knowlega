package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/wiki"
)

type ProjectFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

type ProjectFileContent struct {
	Path        string         `json:"path"`
	Kind        string         `json:"kind"`
	Size        int64          `json:"size"`
	ModTime     string         `json:"mod_time"`
	Title       string         `json:"title,omitempty"`
	Type        string         `json:"type,omitempty"`
	Sources     []string       `json:"sources,omitempty"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Content     string         `json:"content"`
}

type SourceManifestListEntry struct {
	Key             string   `json:"key"`
	OriginalPath    string   `json:"original_path"`
	RawPath         string   `json:"raw_path"`
	ArchivePath     string   `json:"archive_path,omitempty"`
	OriginalRawPath string   `json:"original_raw_path,omitempty"`
	ContentPath     string   `json:"content_path,omitempty"`
	Title           string   `json:"title"`
	SHA256          string   `json:"sha256"`
	Files           []string `json:"files"`
	ReviewCount     int      `json:"review_count"`
	UpdatedAt       string   `json:"updated_at"`
}

type WikiGraphAPIResult struct {
	Nodes      []WikiGraphAPINode `json:"nodes"`
	Edges      []WikiGraphAPIEdge `json:"edges"`
	Truncated  bool               `json:"truncated,omitempty"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Stats      ProjectGraphStats  `json:"stats"`
}

type WikiGraphAPINode struct {
	ID        string         `json:"id"`
	Path      string         `json:"path"`
	Title     string         `json:"title"`
	Type      string         `json:"type"`
	Domain    string         `json:"domain"`
	Kind      string         `json:"kind"`
	Label     string         `json:"label"`
	ScopeID   string         `json:"scope_id,omitempty"`
	Community string         `json:"community,omitempty"`
	SourceRef string         `json:"source_ref,omitempty"`
	Sources   []string       `json:"sources"`
	InDegree  int            `json:"in_degree"`
	OutDegree int            `json:"out_degree"`
	Props     map[string]any `json:"props,omitempty"`
}

type WikiGraphAPIEdge struct {
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Target          string         `json:"target"`
	Kind            string         `json:"kind"`
	Relation        string         `json:"relation"`
	Confidence      string         `json:"confidence"`
	ConfidenceScore float64        `json:"confidence_score"`
	Weight          float64        `json:"weight"`
	Evidence        []string       `json:"evidence,omitempty"`
	Props           map[string]any `json:"props,omitempty"`
}

type ProjectGraphStats struct {
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByDomain   map[string]int `json:"by_domain"`
	ByKind     map[string]int `json:"by_kind"`
}

func ListProjectFiles(projectPath string) ([]ProjectFile, error) {
	if strings.TrimSpace(projectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	var files []ProjectFile
	for _, root := range []string{"purpose.md", "schema.md", "wiki", filepath.Join("raw", "sources")} {
		abs := filepath.Join(projectPath, root)
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(projectPath, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			kind := "file"
			relSlash := filepath.ToSlash(rel)
			switch {
			case strings.HasPrefix(relSlash, "wiki/"):
				kind = "wiki"
			case strings.HasPrefix(relSlash, "raw/sources/"):
				kind = "raw-source"
			case relSlash == "purpose.md" || relSlash == "schema.md":
				kind = "project"
			}
			files = append(files, ProjectFile{
				Path:    relSlash,
				Kind:    kind,
				Size:    info.Size(),
				ModTime: info.ModTime().UTC().Format(timeRFC3339),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func ReadProjectFile(projectPath, rel string) (string, error) {
	file, err := ReadProjectFileContent(projectPath, rel)
	if err != nil {
		return "", err
	}
	return file.Content, nil
}

func ReadProjectFileContent(projectPath, rel string) (ProjectFileContent, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return ProjectFileContent{}, fmt.Errorf("path is required")
	}
	if !isReadableProjectFile(rel) {
		return ProjectFileContent{}, fmt.Errorf("path must be purpose.md, schema.md, wiki/..., raw/sources/..., or raw/code-graphs/...")
	}
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	cleanProject, err := filepath.Abs(projectPath)
	if err != nil {
		return ProjectFileContent{}, err
	}
	cleanAbs, err := filepath.Abs(abs)
	if err != nil {
		return ProjectFileContent{}, err
	}
	if cleanAbs != cleanProject && !strings.HasPrefix(cleanAbs, cleanProject+string(os.PathSeparator)) {
		return ProjectFileContent{}, fmt.Errorf("path escapes project: %s", rel)
	}
	data, err := os.ReadFile(cleanAbs)
	if err != nil {
		return ProjectFileContent{}, err
	}
	info, err := os.Stat(cleanAbs)
	if err != nil {
		return ProjectFileContent{}, err
	}
	content := string(data)
	result := ProjectFileContent{
		Path:    rel,
		Kind:    projectFileKind(rel),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(timeRFC3339),
		Content: content,
	}
	if strings.HasPrefix(rel, "wiki/") && strings.HasSuffix(strings.ToLower(rel), ".md") {
		page := wiki.ParseWikiPage("local", rel, content)
		result.Title = page.Title
		result.Type = page.Type
		result.Sources = append([]string(nil), page.Sources...)
		result.Frontmatter = page.Frontmatter
	} else if title := firstMarkdownHeading(content); title != "" {
		result.Title = title
	}
	return result, nil
}

func ListSourceManifest(projectPath string) ([]SourceManifestListEntry, error) {
	manifest, err := loadSourceManifestFile(projectPath)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]SourceManifestListEntry, 0, len(keys))
	for _, key := range keys {
		entry := manifest.Sources[key]
		out = append(out, SourceManifestListEntry{
			Key:             key,
			OriginalPath:    entry.OriginalPath,
			RawPath:         entry.RawPath,
			ArchivePath:     entry.ArchivePath,
			OriginalRawPath: entry.OriginalRawPath,
			ContentPath:     entry.ContentPath,
			Title:           entry.Title,
			SHA256:          entry.SHA256,
			Files:           append([]string(nil), entry.Files...),
			ReviewCount:     entry.ReviewCount,
			UpdatedAt:       entry.UpdatedAt,
		})
	}
	return out, nil
}

func WikiGraphForAPI(projectPath string) (WikiGraphAPIResult, error) {
	return QueryProjectGraph(ProjectGraphQuery{ProjectPath: projectPath, Limit: 200})
}

func projectFileKind(rel string) string {
	switch {
	case strings.HasPrefix(rel, "wiki/"):
		return "wiki"
	case strings.HasPrefix(rel, "raw/sources/"):
		return "raw-source"
	case strings.HasPrefix(rel, "raw/code-graphs/"):
		return "code-graph"
	case rel == "purpose.md" || rel == "schema.md":
		return "project"
	default:
		return "file"
	}
}

func isReadableProjectFile(rel string) bool {
	return rel == "purpose.md" ||
		rel == "schema.md" ||
		strings.HasPrefix(rel, "wiki/") ||
		strings.HasPrefix(rel, "raw/sources/") ||
		strings.HasPrefix(rel, "raw/code-graphs/")
}

func firstMarkdownHeading(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
