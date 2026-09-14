package agent

import (
	"encoding/json"
	"testing"
)

func TestPiRejectsInvalidToolArgumentsBeforeExecution(t *testing.T) {
	definitions := []ToolDefinition{toolDefinition("read", "", `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","minimum":1},"scope":{"enum":["wiki","raw"]}},"required":["path"],"additionalProperties":false}`)}
	for _, raw := range []string{`{"path":`, `{}`, `[]`, `{"path":7}`, `{"path":"x","limit":0}`, `{"path":"x","limit":1.2}`, `{"path":"x","scope":"other"}`, `{"path":"x","extra":true}`} {
		if err := validatePiToolCall(ToolCall{Name: "read", Arguments: json.RawMessage(raw)}, definitions); err == nil {
			t.Errorf("accepted invalid arguments %s", raw)
		}
	}
	if err := validatePiToolCall(ToolCall{Name: "read", Arguments: json.RawMessage(`{"path":"x","limit":2,"scope":"raw"}`)}, definitions); err != nil {
		t.Fatal(err)
	}
	if err := validatePiToolCall(ToolCall{Name: "unknown", Arguments: json.RawMessage(`{}`)}, definitions); err == nil {
		t.Fatal("accepted unknown tool")
	}
}

func TestPiTransportPreservesMalformedArguments(t *testing.T) {
	completion, err := parseOpenAIPiResponse([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var original string
	if len(completion.ToolCalls) != 1 || json.Unmarshal(completion.ToolCalls[0].Arguments, &original) != nil || original != `{"path":` {
		t.Fatalf("malformed arguments were rewritten: %+v", completion)
	}
}
