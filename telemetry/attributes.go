package telemetry

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
)

// MarshalJSON uses Pi's attribute value domain rather than encoding/json's
// special cases for Go types. Byte slices are numeric arrays; every number is
// binary64, nonfinite numbers become null, and negative zero becomes zero.
// Extracting primitive values avoids invoking custom serialization methods.
// In-memory snapshots retain their original Go scalar/slice types and values.
func (attributes Attributes) MarshalJSON() ([]byte, error) {
	copied, ok := copyAttributes(attributes)
	if !ok {
		return nil, errors.New("unsupported telemetry attribute payload")
	}
	values := make(map[string]any, len(copied))
	for name, value := range copied {
		values[name] = attributeJSONValue(reflect.ValueOf(value))
	}
	return json.Marshal(values)
}

// Input has already passed copyAttributes, so only primitives and flat slices
// can reach this conversion. It never calls user-defined methods.
func attributeJSONValue(value reflect.Value) any {
	if value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String()
	case reflect.Bool:
		return value.Bool()
	case reflect.Slice:
		array := make([]any, value.Len())
		for i := range array {
			array[i] = attributeJSONValue(value.Index(i))
		}
		return array
	}
	var number float64
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number = float64(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		number = float64(value.Uint())
	case reflect.Float32, reflect.Float64:
		number = value.Float()
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	if number == 0 {
		return float64(0)
	}
	return number
}

func copyAttributes(attributes Attributes) (Attributes, bool) {
	copy := make(Attributes, len(attributes))
	for name, value := range attributes {
		// Pi assigns onto a plain {}. The inherited __proto__ setter never
		// creates an own attribute, so it disappears from detached snapshots.
		// Other Object.prototype names are ordinary own attributes when set.
		if name == "__proto__" {
			continue
		}
		if value == nil {
			continue
		}
		v := reflect.ValueOf(value)
		if attributeScalarKind(v.Kind()) != reflect.Invalid {
			copy[name] = value
			continue
		}
		if v.Kind() != reflect.Slice {
			return nil, false
		}
		if kind := v.Type().Elem().Kind(); kind != reflect.Interface && attributeScalarKind(kind) == reflect.Invalid {
			return nil, false
		}
		// Pi has one numeric type: mixed Go integer/float widths still form a
		// homogeneous number array. Nested values or mixed scalar categories
		// remain outside the attribute contract and reject the whole mutation.
		var elementKind reflect.Kind
		for i := 0; i < v.Len(); i++ {
			element := v.Index(i)
			if element.Kind() == reflect.Interface {
				element = element.Elem()
			}
			kind := attributeScalarKind(element.Kind())
			if kind == reflect.Invalid || (elementKind != reflect.Invalid && kind != elementKind) {
				return nil, false
			}
			elementKind = kind
		}
		cloned := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(cloned, v)
		copy[name] = cloned.Interface()
	}
	return copy, true
}

func attributeScalarKind(kind reflect.Kind) reflect.Kind {
	switch kind {
	case reflect.String, reflect.Bool:
		return kind
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return reflect.Float64
	default:
		return reflect.Invalid
	}
}
