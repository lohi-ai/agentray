package ingestion

import "encoding/json"

func stringProp(props map[string]any, key string) string {
	if value, ok := props[key].(string); ok {
		return value
	}
	return ""
}

func uint32Prop(props map[string]any, key string) *uint32 {
	switch value := props[key].(type) {
	case float64:
		v := uint32(value)
		return &v
	case int:
		v := uint32(value)
		return &v
	case uint32:
		return &value
	default:
		return nil
	}
}

func float32Prop(props map[string]any, key string) *float32 {
	switch value := props[key].(type) {
	case float64:
		v := float32(value)
		return &v
	case float32:
		return &value
	default:
		return nil
	}
}

func boolProp(props map[string]any, key string) bool {
	value, _ := props[key].(bool)
	return value
}

func optionalJSON(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
