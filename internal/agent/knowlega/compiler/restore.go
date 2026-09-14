package compiler

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/projectlock"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

// RestoreQuarantinedSourcePages repairs legacy ownership quarantine. Only
// missing paths citing an active immutable source are restored; archives stay intact.
func RestoreQuarantinedSourcePages(projectPath string) (int, error) {
	lock, err := projectlock.Acquire(projectPath)
	if err != nil {
		return 0, err
	}
	defer lock.Release()
	manifest, err := loadSourceManifest(projectPath)
	if err != nil {
		return 0, err
	}
	active := map[string]bool{}
	for _, entry := range manifest.Sources {
		if validExistingRawSource(projectPath, entry.RawPath) {
			active[entry.RawPath] = true
		}
	}
	candidates := map[string]string{}
	contents := map[string]string{}
	root := filepath.Join(projectPath, ".kbcore", "orphaned-pages")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 || filepath.Ext(path) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		if !strings.HasPrefix(text, "<!-- kbcore-quarantined-page\n") {
			return nil
		}
		end := strings.Index(text, "\n-->\n")
		if end < 0 {
			return nil
		}
		original := ""
		for _, line := range strings.Split(text[:end], "\n") {
			if strings.HasPrefix(line, "original: ") {
				original = strings.TrimPrefix(line, "original: ")
			}
		}
		if validateWikiFilePath(original) != nil {
			return nil
		}
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(original))); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		content := strings.TrimSpace(text[end+len("\n-->\n"):]) + "\n"
		ownedContent := func(value string) bool {
			fm, err := parseGeneratedFrontmatter(value)
			if err != nil {
				return false
			}
			for _, source := range frontmatterStrings(fm["sources"]) {
				if active[source] {
					return true
				}
			}
			return false
		}
		if !ownedContent(content) {
			// Legacy convergence erased provenance before quarantine. Recover
			// the newest earlier version still tied to an active raw source.
			versions, err := filepath.Glob(filepath.Join(projectPath, ".kbcore", "page-versions", filepath.FromSlash(strings.TrimSuffix(original, ".md")), "*.md"))
			if err != nil {
				return err
			}
			sort.Sort(sort.Reverse(sort.StringSlice(versions)))
			for _, version := range versions {
				raw, err := os.ReadFile(version)
				if err != nil {
					return err
				}
				archived := string(raw)
				if strings.HasPrefix(archived, "<!-- kbcore-page-version\n") {
					if end := strings.Index(archived, "\n-->\n"); end >= 0 {
						archived = strings.TrimSpace(archived[end+len("\n-->\n"):]) + "\n"
					}
				}
				if ownedContent(archived) {
					content = archived
					break
				}
			}
		}
		if ownedContent(content) && path > candidates[original] {
			candidates[original] = path
			contents[original] = content
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	paths := make([]string, 0, len(candidates))
	for path := range candidates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	count := 0
	for _, path := range paths {
		content := contents[path]
		if err := wiki.WriteVersionedPage(projectPath, path, []byte(content), "restore active-source page after legacy ownership quarantine"); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
