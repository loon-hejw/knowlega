package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	loadOnce sync.Once
	loaded   map[string]string
)

// Value returns the first non-empty setting for keys. Process environment
// variables take precedence over local config files so one-off shell overrides
// keep working.
func Value(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	loadOnce.Do(func() {
		loaded = loadFiles(candidateFiles())
	})
	for _, key := range keys {
		if value := strings.TrimSpace(loaded[key]); value != "" {
			return value
		}
	}
	return ""
}

func candidateFiles() []string {
	if explicit := strings.TrimSpace(os.Getenv("KB_CORE_CONFIG")); explicit != "" {
		return []string{explicit}
	}
	return []string{
		".env.local",
		"kbcore.env",
		filepath.Join(".kbcore", "config.env"),
	}
}

func loadFiles(paths []string) map[string]string {
	values := make(map[string]string)
	for _, path := range paths {
		for key, value := range readFile(path) {
			if _, exists := values[key]; !exists {
				values[key] = value
			}
		}
	}
	return values
}

func readFile(path string) map[string]string {
	values := make(map[string]string)
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseLine(scanner.Text())
		if ok {
			values[key] = value
		}
	}
	return values
}

func parseLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	value = strings.TrimSpace(stripInlineComment(value))
	value = strings.Trim(value, `"'`)
	return key, value, true
}

func stripInlineComment(value string) string {
	inSingle := false
	inDouble := false
	for i, r := range value {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return value[:i]
			}
		}
	}
	return value
}

func resetForTest() {
	loadOnce = sync.Once{}
	loaded = nil
}
