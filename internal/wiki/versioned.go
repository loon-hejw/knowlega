package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// atomicWriteTestHook injects failures at the actual atomic replacement
// boundary. It is intentionally package-private and used only by fault tests.
var atomicWriteTestHook func(path, stage string) error

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
	return writeAtomicFile(abs, data, 0o644)
}

// RemoveVersionedPage archives the current Markdown before removing it from
// the live wiki. This keeps migrations and canonical merges reversible.
func RemoveVersionedPage(projectPath, rel, reason string) error {
	rel = filepath.ToSlash(rel)
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	old, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := archivePageVersion(projectPath, rel, old, reason); err != nil {
		return err
	}
	return os.Remove(abs)
}

// QuarantineVersionedPage preserves a readable copy outside the active wiki,
// archives the prior live version, and then removes the active page.
func QuarantineVersionedPage(projectPath, rel, reason string) error {
	rel = filepath.ToSlash(rel)
	abs := filepath.Join(projectPath, filepath.FromSlash(rel))
	old, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(old)
	hash := hex.EncodeToString(sum[:])[:12]
	dir := filepath.Join(projectPath, ".kbcore", "orphaned-pages", filepath.FromSlash(strings.TrimSuffix(rel, ".md")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hash + ".md"
	var quarantined strings.Builder
	quarantined.WriteString("<!-- kbcore-quarantined-page\noriginal: ")
	quarantined.WriteString(rel)
	quarantined.WriteString("\nreason: ")
	quarantined.WriteString(strings.ReplaceAll(reason, "\n", " "))
	quarantined.WriteString("\nquarantined_at: ")
	quarantined.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
	quarantined.WriteString("\n-->\n\n")
	quarantined.Write(old)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(quarantined.String()), 0o644); err != nil {
		return err
	}
	return RemoveVersionedPage(projectPath, rel, reason)
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
