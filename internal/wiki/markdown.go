package wiki

import (
	"fmt"
	"sort"
	"strings"
)

type Page struct {
	Title       string
	Type        string
	Sources     []string
	Extra       map[string]string
	Body        string
	GeneratedBy string
}

func RenderPage(p Page) string {
	var b strings.Builder
	b.WriteString("---\n")
	writeYAMLScalar(&b, "type", p.Type)
	writeYAMLScalar(&b, "title", p.Title)
	if p.GeneratedBy != "" {
		writeYAMLScalar(&b, "generated_by", p.GeneratedBy)
	}
	if len(p.Sources) > 0 {
		b.WriteString("sources:\n")
		for _, source := range p.Sources {
			b.WriteString("  - ")
			b.WriteString(quoteYAML(source))
			b.WriteString("\n")
		}
	}
	keys := make([]string, 0, len(p.Extra))
	for key := range p.Extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeYAMLScalar(&b, key, p.Extra[key])
	}
	b.WriteString("---\n\n")
	if strings.TrimSpace(p.Body) == "" {
		b.WriteString("# ")
		b.WriteString(p.Title)
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString(strings.TrimSpace(p.Body))
	b.WriteString("\n")
	return b.String()
}

func writeYAMLScalar(b *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quoteYAML(value))
	b.WriteString("\n")
}

func quoteYAML(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return fmt.Sprintf("%q", escaped)
}
