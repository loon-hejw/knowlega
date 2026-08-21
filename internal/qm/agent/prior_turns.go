package agent

import (
	"encoding/json"
	"strings"
)

type priorConversationTurn struct {
	Role string `json:"role"`
	Name string `json:"name,omitempty"`
	Text string `json:"text"`
}

func seedPriorTurnsText(raw json.RawMessage) string {
	var turns []priorConversationTurn
	if len(raw) == 0 || json.Unmarshal(raw, &turns) != nil {
		return ""
	}
	seen := map[string]bool{}
	lines := []string{}
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Text)
		key := strings.Join(strings.Fields(text), " ")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		attributes := `from="human"`
		if turn.Role == "assistant" {
			attributes = `from="agent"`
			if strings.TrimSpace(turn.Name) != "" {
				attributes += ` via="` + xmlAttributeEscape(strings.TrimSpace(turn.Name)) + `"`
			}
		} else if strings.TrimSpace(turn.Name) != "" {
			attributes += ` author="` + xmlAttributeEscape(strings.TrimSpace(turn.Name)) + `"`
		}
		lines = append(lines, "<message "+attributes+">"+xmlTextEscape(text)+"</message>")
	}
	return strings.Join(lines, "\n")
}

func xmlTextEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

func xmlAttributeEscape(value string) string {
	return strings.ReplaceAll(xmlTextEscape(value), `"`, "&quot;")
}
