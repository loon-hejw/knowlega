package core

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

var unsafeSlugChars = regexp.MustCompile(`[^a-z0-9._-]+`)

func Slug(input string) string {
	name := strings.TrimSpace(strings.ToLower(input))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.ReplaceAll(name, " ", "-")
	name = unsafeSlugChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		return "untitled"
	}
	if len(name) > 80 {
		return name[:80]
	}
	return name
}

func StableID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
