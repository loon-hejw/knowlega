package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func AppendIndexEntry(projectPath, section, title, rel string) error {
	if strings.TrimSpace(section) == "" {
		section = "Other"
	}
	path := filepath.Join(projectPath, "wiki", "index.md")
	contentBytes, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		contentBytes = []byte("# Index\n\n")
	} else if err != nil {
		return err
	}
	content := string(contentBytes)
	entry := fmt.Sprintf("- [[%s|%s]] - `%s`\n", strings.TrimSuffix(filepath.Base(rel), ".md"), title, filepath.ToSlash(rel))
	if strings.Contains(content, entry) {
		return nil
	}
	updated := insertIndexEntry(content, section, entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func insertIndexEntry(content, section, entry string) string {
	lines := strings.SplitAfter(content, "\n")
	heading := "## " + section
	sectionStart := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			sectionStart = i
			break
		}
	}
	if sectionStart < 0 {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if !strings.HasSuffix(content, "\n\n") {
			content += "\n"
		}
		return content + heading + "\n\n" + entry
	}
	insertAt := len(lines)
	for i := sectionStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			insertAt = i
			break
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	if insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) != "" {
		out = append(out, "\n")
	}
	out = append(out, entry)
	if insertAt < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[insertAt]), "## ") {
		out = append(out, "\n")
	}
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "")
}
