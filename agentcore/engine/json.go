package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Pi catch sites use Error.message, otherwise String(value). Keep Go error
// identity while formatting JSON-shaped panic values with the source's rules.
func failureError(value any) error {
	if _, ok := value.(*runtime.PanicNilError); ok {
		return errors.New("null")
	}
	if failure, ok := value.(error); ok {
		return failure
	}
	return errors.New(jsValueString(value))
}

func jsValueString(value any) string {
	switch value := value.(type) {
	case nil:
		return "null"
	case float64:
		switch {
		case math.IsNaN(value):
			return "NaN"
		case math.IsInf(value, 1):
			return "Infinity"
		case math.IsInf(value, -1):
			return "-Infinity"
		}
		raw, _ := marshalJSScalar(value)
		return string(raw)
	case map[string]any:
		return "[object Object]"
	case []any:
		items := make([]string, len(value))
		for i, item := range value {
			if item != nil {
				items[i] = jsValueString(item)
			}
		}
		return strings.Join(items, ",")
	default:
		return fmt.Sprint(value)
	}
}

// marshalArguments preserves the insertion order of source object keys after
// coercion. Tools can observe that order by stringifying validated arguments.
// JavaScript array-index properties enumerate first, in numeric order.
func marshalArguments(value any, source json.RawMessage) ([]byte, error) {
	switch value := value.(type) {
	case map[string]any:
		keys := []string{}
		original := map[string]json.RawMessage{}
		decoder := json.NewDecoder(bytes.NewReader(source))
		if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key := token.(string)
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err != nil {
					return nil, err
				}
				if _, seen := original[key]; !seen {
					if _, exists := value[key]; exists {
						keys = append(keys, key)
					}
				}
				original[key] = raw
			}
		}
		additional := []string{}
		for key := range value {
			if _, exists := original[key]; !exists {
				additional = append(additional, key)
			}
		}
		slices.Sort(additional)
		keys = append(keys, additional...)
		index := func(key string) (uint64, bool) {
			n, err := strconv.ParseUint(key, 10, 32)
			return n, err == nil && n < 4294967295 && strconv.FormatUint(n, 10) == key
		}
		slices.SortStableFunc(keys, func(a, b string) int {
			x, ax := index(a)
			y, bx := index(b)
			if ax && bx {
				if x < y {
					return -1
				}
				if x > y {
					return 1
				}
				return 0
			}
			if ax {
				return -1
			}
			if bx {
				return 1
			}
			return 0
		})
		var out bytes.Buffer
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			name, _ := marshalJSScalar(key)
			child, err := marshalArguments(value[key], original[key])
			if err != nil {
				return nil, err
			}
			out.Write(name)
			out.WriteByte(':')
			out.Write(child)
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	case []any:
		var original []json.RawMessage
		_ = json.Unmarshal(source, &original)
		var out bytes.Buffer
		out.WriteByte('[')
		for i, child := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			var template json.RawMessage
			if i < len(original) {
				template = original[i]
			}
			raw, err := marshalArguments(child, template)
			if err != nil {
				return nil, err
			}
			out.Write(raw)
		}
		out.WriteByte(']')
		return out.Bytes(), nil
	default:
		return marshalJSScalar(value)
	}
}

func marshalJSScalar(value any) ([]byte, error) {
	if number, ok := value.(float64); ok && number == 0 {
		return []byte("0"), nil
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	// Go escapes line/paragraph separators even with HTML escaping disabled;
	// JSON.stringify preserves them. Skip escaped backslashes so literal
	// strings such as "\\u2028" retain their spelling.
	result := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			if i+6 <= len(raw) && (string(raw[i:i+6]) == `\u2028` || string(raw[i:i+6]) == `\u2029`) {
				if raw[i+5] == '8' {
					result = append(result, []byte("\u2028")...)
				} else {
					result = append(result, []byte("\u2029")...)
				}
				i += 5
				continue
			}
			result = append(result, raw[i], raw[i+1])
			i++
			continue
		}
		result = append(result, raw[i])
	}
	return result, nil
}
