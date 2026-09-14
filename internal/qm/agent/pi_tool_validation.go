package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
)

// Validate the JSON Schema vocabulary used by core tool definitions before
// dispatch. Individual tools still enforce semantic and authorization rules.
func validatePiToolCall(call ToolCall, definitions []ToolDefinition) error {
	var definition *ToolDefinition
	for i := range definitions {
		if definitions[i].Name == call.Name {
			definition = &definitions[i]
			break
		}
	}
	if definition == nil {
		return fmt.Errorf("unknown tool %q", call.Name)
	}
	var value any
	if err := json.Unmarshal(call.Arguments, &value); err != nil {
		return fmt.Errorf("invalid arguments for %s: %w", call.Name, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("arguments for %s must be an object", call.Name)
	}
	if len(definition.InputSchema) == 0 {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return fmt.Errorf("invalid schema for %s: %w", call.Name, err)
	}
	return validatePiSchema(value, schema, call.Name)
}

func validatePiSchema(value any, schema map[string]any, path string) error {
	if kind, ok := schema["type"].(string); ok {
		valid := false
		switch kind {
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number", "integer":
			n, ok := value.(float64)
			valid = ok && (kind == "number" || n == math.Trunc(n))
		case "null":
			valid = value == nil
		default:
			return fmt.Errorf("%s: unsupported schema type %q", path, kind)
		}
		if !valid {
			return fmt.Errorf("%s must be %s", path, kind)
		}
	}
	if choices, ok := schema["enum"].([]any); ok {
		found := false
		for _, choice := range choices {
			found = found || reflect.DeepEqual(value, choice)
		}
		if !found {
			return fmt.Errorf("%s is outside the allowed enum", path)
		}
	}
	if object, ok := value.(map[string]any); ok {
		required, _ := schema["required"].([]any)
		for _, key := range required {
			if _, exists := object[fmt.Sprint(key)]; !exists {
				return fmt.Errorf("%s.%v is required", path, key)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range object {
			if childSchema, ok := properties[key].(map[string]any); ok {
				if err := validatePiSchema(child, childSchema, path+"."+key); err != nil {
					return err
				}
			} else if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
				return fmt.Errorf("%s.%s is not allowed", path, key)
			}
		}
	}
	if items, ok := value.([]any); ok {
		if childSchema, ok := schema["items"].(map[string]any); ok {
			for i, child := range items {
				if err := validatePiSchema(child, childSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	if number, ok := value.(float64); ok {
		if minimum, ok := schema["minimum"].(float64); ok && number < minimum {
			return fmt.Errorf("%s is below minimum", path)
		}
		if maximum, ok := schema["maximum"].(float64); ok && number > maximum {
			return fmt.Errorf("%s is above maximum", path)
		}
	}
	if str, ok := value.(string); ok {
		if pattern, ok := schema["pattern"].(string); ok {
			matched, err := regexp.MatchString(pattern, str)
			if err != nil || !matched {
				return fmt.Errorf("%s does not match the required pattern", path)
			}
		}
	}
	return nil
}
