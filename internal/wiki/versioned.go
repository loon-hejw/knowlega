package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func WriteVersionedPage(projectPath, rel string, data []byte, reason string) error {
	rel = filepath.ToSlash(rel)
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	if old, err := os.ReadFile(abs); err == nil {
		if string(old) != string(data) {
			if err := archivePageVersion(projectPath, rel, old, reason); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp := abs + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, abs)
}

func archivePageVersion(projectPath, rel string, old []byte, reason string) error {
	sum := sha256.Sum256(old)
	hash := hex.EncodeToString(sum[:])[:12]
	versionDir := filepath.Join(projectPath, ".kbcore", "page-versions", filepath.FromSlash(strings.TrimSuffix(rel, ".md")))
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hash + ".md"
	var b strings.Builder
	b.WriteString("<!-- kbcore-page-version\n")
	b.WriteString("original: ")
	b.WriteString(rel)
	b.WriteString("\nreason: ")
	b.WriteString(strings.ReplaceAll(reason, "\n", " "))
	b.WriteString("\narchived_at: ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
	b.WriteString("\n-->\n\n")
	b.Write(old)
	return os.WriteFile(filepath.Join(versionDir, name), []byte(b.String()), 0o644)
}
