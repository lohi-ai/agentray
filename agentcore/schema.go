package agentcore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// validateArgs ensures the model emitted parseable JSON arguments AND that they
// satisfy the tool's advertised JSON Schema (required fields, primitive types,
// enums). Schema validation is shallow by design — our tool parameters are flat
// objects — but it catches the common model failure (right JSON, wrong shape)
// before execution and feeds a precise, self-correctable reason back to the
// model (pi's validateToolArguments). A nil/empty/non-object schema falls back
// to the JSON-parse check only.
func validateArgs(args string, schema map[string]any) error {
	args = strings.TrimSpace(args)
	if args == "" {
		// No-arg call: only valid if the schema requires nothing.
		if missing := missingRequired(map[string]any{}, schema); len(missing) > 0 {
			return fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
		}
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return fmt.Errorf("arguments are not valid JSON")
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		// Non-object arguments: nothing more we can check against an object schema.
		return nil
	}
	return validateObject(obj, schema)
}

// validateObject checks an arguments object against a JSON Schema object node:
// required presence first, then per-property type/enum for the fields present.
func validateObject(obj, schema map[string]any) error {
	if !isObjectSchema(schema) {
		return nil
	}
	if missing := missingRequired(obj, schema); len(missing) > 0 {
		return fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range obj {
		propSchema, ok := props[name].(map[string]any)
		if !ok {
			continue // unknown / additional property — permitted
		}
		if err := validateValue(name, raw, propSchema); err != nil {
			return err
		}
	}
	return nil
}

// validateValue checks one field value against its property schema (type + enum).
// Unknown or absent type constraints pass.
func validateValue(name string, value any, propSchema map[string]any) error {
	if enum, ok := propSchema["enum"].([]any); ok && len(enum) > 0 {
		if !enumContains(enum, value) {
			return fmt.Errorf("field %q must be one of %s", name, formatEnum(enum))
		}
	}
	typ, _ := propSchema["type"].(string)
	if typ == "" || matchesJSONType(value, typ) {
		return nil
	}
	return fmt.Errorf("field %q must be a %s", name, typ)
}

// missingRequired returns the names listed in schema.required that are absent
// from obj.
func missingRequired(obj, schema map[string]any) []string {
	req, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	var missing []string
	for _, r := range req {
		name, ok := r.(string)
		if !ok {
			continue
		}
		if _, present := obj[name]; !present {
			missing = append(missing, name)
		}
	}
	return missing
}

// isObjectSchema reports whether schema describes an object (so required/
// properties are meaningful). A schema with no explicit type but with
// properties/required is treated as an object.
func isObjectSchema(schema map[string]any) bool {
	if len(schema) == 0 {
		return false
	}
	if typ, ok := schema["type"].(string); ok {
		return typ == "object"
	}
	_, hasProps := schema["properties"]
	_, hasReq := schema["required"]
	return hasProps || hasReq
}

// matchesJSONType reports whether a value decoded from JSON matches a JSON
// Schema primitive type. JSON numbers decode to float64, so "integer" accepts a
// whole-valued float.
func matchesJSONType(value any, typ string) bool {
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return true // unknown type constraint — don't reject
	}
}

func enumContains(enum []any, value any) bool {
	for _, e := range enum {
		if e == value {
			return true
		}
	}
	return false
}

func formatEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprintf("%v", e))
	}
	return strings.Join(parts, ", ")
}
