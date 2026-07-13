package promptbudget

import (
	"fmt"
	"strings"
)

const TruncationMarker = "\n\n[...prompt context truncated to stay within the configured LLM input budget...]\n\n"

// TrimMiddle keeps the beginning and end of text while bounding rune count.
// This preserves headings and recent evidence better than a head-only or
// tail-only truncation when an LLM prompt grows too large.
func TrimMiddle(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	marker := []rune(TruncationMarker)
	if maxRunes <= len(marker) {
		return string(runes[:maxRunes])
	}
	remaining := maxRunes - len(marker)
	head := remaining / 2
	tail := remaining - head
	return string(runes[:head]) + string(marker) + string(runes[len(runes)-tail:])
}

// TrimEnd bounds rune count while preserving the start of text.
func TrimEnd(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	marker := "..."
	if maxRunes <= len([]rune(marker)) {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-len([]rune(marker))]) + marker
}

// BudgetChatInput applies a conservative character budget before a
// tokenizer-specific API can reject the request.
func BudgetChatInput(system, user string, maxInputRunes int) (string, string) {
	if maxInputRunes <= 0 {
		return system, user
	}
	const separatorReserve = 64
	systemRunes := len([]rune(system))
	userRunes := len([]rune(user))
	if systemRunes+userRunes+separatorReserve <= maxInputRunes {
		return system, user
	}
	userBudget := maxInputRunes - systemRunes - separatorReserve
	if userBudget < 0 {
		systemBudget := maxInputRunes / 3
		if systemBudget < 512 {
			systemBudget = 512
		}
		system = TrimEnd(system, systemBudget)
		userBudget = maxInputRunes - len([]rune(system)) - separatorReserve
	}
	if userBudget < 0 {
		userBudget = 0
	}
	return system, TrimMiddle(user, userBudget)
}

func Annotation(original, kept int) string {
	if original <= kept {
		return ""
	}
	return fmt.Sprintf("truncated from %d to %d runes", original, kept)
}

func JoinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n")
}
