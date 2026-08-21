package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

const interruptedToolResult = "[interrupted before this tool call completed]"

type TapeFoldLint struct {
	OK       bool
	Problems []string
}

type TapeSeedPlan struct {
	Seed []PiMessage
	Fold []json.RawMessage
	Lint TapeFoldLint
	Skip string
}

type tapeBoundary struct {
	position, entrySequence int
}

type tapeEvent struct {
	Event    string            `json:"event"`
	Text     string            `json:"text,omitempty"`
	Messages []json.RawMessage `json:"messages,omitempty"`
}

func FoldTape(rows []TapeRecord) []json.RawMessage {
	out := []json.RawMessage{}
	boundaries := []tapeBoundary{}
	for _, row := range rows {
		if row.Kind == "annotation" {
			var annotation struct {
				TurnEnd bool `json:"turnEnd"`
			}
			if json.Unmarshal(row.Payload, &annotation) == nil && annotation.TurnEnd && row.EntrySequence != nil {
				boundaries = append(boundaries, tapeBoundary{position: len(out), entrySequence: *row.EntrySequence})
			}
			continue
		}
		if row.Kind == "context_event" {
			var event tapeEvent
			if json.Unmarshal(row.Payload, &event) != nil {
				continue
			}
			switch event.Event {
			case "legacy_import":
				out = cloneRawMessages(event.Messages)
				boundaries = nil
				if row.CoversEntrySequence != nil {
					boundaries = append(boundaries, tapeBoundary{position: len(out), entrySequence: *row.CoversEntrySequence})
				}
			case "legacy_patch":
				out = append(out, cloneRawMessages(event.Messages)...)
				if row.CoversEntrySequence != nil {
					boundaries = append(boundaries, tapeBoundary{position: len(out), entrySequence: *row.CoversEntrySequence})
				}
			case "compaction":
				cut := -1
				if row.CoversEntrySequence != nil {
					for index := len(boundaries) - 1; index >= 0; index-- {
						if boundaries[index].entrySequence <= *row.CoversEntrySequence {
							cut = index
							break
						}
					}
				}
				position := 0
				if cut >= 0 {
					position = boundaries[cut].position
				}
				summary, _ := json.Marshal(map[string]any{
					"role": "user", "content": []any{map[string]string{"type": "text", "text": "[Earlier conversation summary]\n" + event.Text}},
					"timestamp": row.CreatedAt,
				})
				kept := cloneRawMessages(out[position:])
				out = append([]json.RawMessage{summary}, kept...)
				next := []tapeBoundary{}
				for _, boundary := range boundaries {
					if cut < 0 || boundary.position >= position {
						next = append(next, tapeBoundary{position: boundary.position - position + 1, entrySequence: boundary.entrySequence})
					}
				}
				boundaries = next
			case "interrupt":
				out = healDanglingTapeCalls(out, row.CreatedAt)
			}
			continue
		}
		if row.Kind == "message" && len(row.Payload) > 0 && string(row.Payload) != "null" {
			out = append(out, append(json.RawMessage(nil), row.Payload...))
		}
	}
	return out
}

func PlanTapeSeed(rows []TapeRecord, harness, mode string) TapeSeedPlan {
	for _, row := range rows {
		if row.Kind == "message" && row.Harness != "" && row.Harness != harness {
			return TapeSeedPlan{Skip: "foreign-harness"}
		}
	}
	fold := FoldTape(rows)
	lint := LintTapeFold(fold)
	plan := TapeSeedPlan{Fold: fold, Lint: lint}
	if mode != "serve" || !lint.OK || len(fold) == 0 {
		return plan
	}
	seed, err := piMessagesFromTapeFold(fold)
	if err != nil || len(seed) == 0 {
		plan.Lint.OK = false
		if err != nil {
			plan.Lint.Problems = append(plan.Lint.Problems, "decode: "+err.Error())
		}
		return plan
	}
	plan.Seed = seed
	return plan
}

func LintTapeFold(messages []json.RawMessage) TapeFoldLint {
	problems := []string{}
	openCalls := map[string]bool{}
	seenCalls := map[string]bool{}
	if len(messages) > 0 && tapeMessageRole(messages[0]) != "user" {
		problems = append(problems, fmt.Sprintf("#0: first message must be user-role, got %q", tapeMessageRole(messages[0])))
	}
	for index, raw := range messages {
		role := tapeMessageRole(raw)
		if role != "user" && role != "assistant" && role != "toolResult" {
			problems = append(problems, fmt.Sprintf("#%d: unknown role %q", index, role))
			continue
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil {
			problems = append(problems, fmt.Sprintf("#%d: invalid message", index))
			continue
		}
		for _, block := range tapeContentBlocks(message) {
			if block["type"] == "image" {
				if _, ok := block["data"].(string); !ok {
					problems = append(problems, fmt.Sprintf("#%d: image block without bytes", index))
				}
			}
		}
		if role == "assistant" {
			for _, call := range tapeToolCalls(message) {
				id, _ := call["id"].(string)
				if id == "" {
					continue
				}
				if seenCalls[id] {
					problems = append(problems, fmt.Sprintf("#%d: duplicate tool call id %s", index, id))
				}
				seenCalls[id] = true
				openCalls[id] = true
			}
		} else if role == "toolResult" {
			id, _ := message["toolCallId"].(string)
			if id == "" || !openCalls[id] {
				problems = append(problems, fmt.Sprintf("#%d: toolResult without a preceding open call (%s)", index, id))
			} else {
				delete(openCalls, id)
			}
		} else if len(openCalls) > 0 {
			problems = append(problems, fmt.Sprintf("#%d: user message while %d tool call(s) await results", index, len(openCalls)))
			openCalls = map[string]bool{}
		}
	}
	if len(openCalls) > 0 {
		problems = append(problems, fmt.Sprintf("end: %d dangling tool call(s)", len(openCalls)))
	}
	return TapeFoldLint{OK: len(problems) == 0, Problems: problems}
}

func TapeNeedsInterruptHeal(rows []TapeRecord, folded []json.RawMessage) bool {
	problems := LintTapeFold(folded).Problems
	if len(problems) == 0 {
		return false
	}
	for _, problem := range problems {
		if !strings.HasPrefix(problem, "end:") {
			return false
		}
	}
	return true
}

func HealTapeInterrupt(messages []json.RawMessage, at int64) []json.RawMessage {
	return healDanglingTapeCalls(cloneRawMessages(messages), at)
}

func healDanglingTapeCalls(messages []json.RawMessage, at int64) []json.RawMessage {
	answered := map[string]bool{}
	for _, raw := range messages {
		var message map[string]any
		if tapeMessageRole(raw) == "toolResult" && json.Unmarshal(raw, &message) == nil {
			if id, ok := message["toolCallId"].(string); ok {
				answered[id] = true
			}
		}
	}
	type missingCall struct{ id, name string }
	missing := []missingCall{}
	for _, raw := range messages {
		var message map[string]any
		if tapeMessageRole(raw) != "assistant" || json.Unmarshal(raw, &message) != nil {
			continue
		}
		for _, call := range tapeToolCalls(message) {
			id, _ := call["id"].(string)
			name, _ := call["name"].(string)
			if id != "" && !answered[id] {
				if name == "" {
					name = "tool"
				}
				missing = append(missing, missingCall{id: id, name: name})
			}
		}
	}
	for _, call := range missing {
		healed, _ := json.Marshal(map[string]any{
			"role": "toolResult", "toolCallId": call.id, "toolName": call.name,
			"content": []any{map[string]string{"type": "text", "text": interruptedToolResult}},
			"isError": true, "timestamp": at,
		})
		messages = append(messages, healed)
	}
	return messages
}

func tapeMessageRole(raw json.RawMessage) string {
	var message struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	return message.Role
}

func tapeContentBlocks(message map[string]any) []map[string]any {
	content, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	blocks := make([]map[string]any, 0, len(content))
	for _, raw := range content {
		if block, ok := raw.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func tapeToolCalls(message map[string]any) []map[string]any {
	calls := []map[string]any{}
	for _, block := range tapeContentBlocks(message) {
		if block["type"] == "toolCall" {
			calls = append(calls, block)
		}
	}
	if rawCalls, ok := message["toolCalls"].([]any); ok {
		for _, raw := range rawCalls {
			if call, ok := raw.(map[string]any); ok {
				calls = append(calls, call)
			}
		}
	}
	return calls
}

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(messages))
	for index := range messages {
		cloned[index] = append(json.RawMessage(nil), messages[index]...)
	}
	return cloned
}

func piMessagesFromTapeFold(fold []json.RawMessage) ([]PiMessage, error) {
	messages := make([]PiMessage, 0, len(fold))
	for index, raw := range fold {
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		role, _ := document["role"].(string)
		toolCallID, _ := document["toolCallId"].(string)
		message := PiMessage{Role: role, ToolCallID: toolCallID}
		if content, ok := document["content"].(string); ok {
			message.Content = content
		}
		if rawImages, ok := document["images"].([]any); ok {
			for _, rawImage := range rawImages {
				imageMap, _ := rawImage.(map[string]any)
				mimeType, _ := imageMap["mimeType"].(string)
				data, _ := imageMap["dataBase64"].(string)
				artifactID, _ := imageMap["artifactId"].(string)
				message.Images = append(message.Images, Image{MIMEType: mimeType, DataBase64: data, ArtifactID: artifactID})
			}
		}
		if rawCalls, ok := document["toolCalls"].([]any); ok {
			for _, rawCall := range rawCalls {
				call, _ := rawCall.(map[string]any)
				id, _ := call["id"].(string)
				name, _ := call["name"].(string)
				arguments, _ := json.Marshal(call["arguments"])
				message.ToolCalls = append(message.ToolCalls, ToolCall{ID: id, Name: name, Arguments: arguments})
			}
		}
		for _, block := range tapeContentBlocks(document) {
			typeName, _ := block["type"].(string)
			switch typeName {
			case "text":
				if text, ok := block["text"].(string); ok {
					message.Content += text
				}
			case "image":
				data, _ := block["data"].(string)
				mimeType, _ := block["mimeType"].(string)
				message.Images = append(message.Images, Image{MIMEType: mimeType, DataBase64: data})
			case "toolCall":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				arguments, _ := json.Marshal(block["arguments"])
				message.ToolCalls = append(message.ToolCalls, ToolCall{ID: id, Name: name, Arguments: arguments})
			}
		}
		if message.Role == "toolResult" {
			message.Role = "tool"
		}
		messages = append(messages, message)
	}
	return messages, nil
}
