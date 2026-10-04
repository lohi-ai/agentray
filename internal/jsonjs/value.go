package jsonjs

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
)

// MarshalValue exports JSON-shaped values without invoking user methods.
// Object/array identity and numeric/UTF-16 semantics are shared across ports.
func MarshalValue(value any) ([]byte, error) {
	if !SupportedValue(value) {
		return nil, errors.New("unsupported JSON-shaped value")
	}
	converted, err := jsonValue(reflect.ValueOf(value), make(map[valueIdentity]bool))
	if err != nil {
		return nil, err
	}
	return json.Marshal(converted)
}

func SupportedValue(value any) bool {
	return supportedValue(reflect.ValueOf(value), make(map[valueIdentity]bool))
}

type undefinedValue uint8

const Undefined undefinedValue = 0

func IsUndefined(value any) bool { _, ok := value.(undefinedValue); return ok }

func (o *Object) MarshalJSON() ([]byte, error) { return MarshalValue(o) }

func objectValue(value reflect.Value) (*Object, bool) {
	if !value.IsValid() {
		return nil, false
	}
	if value.Type() == reflect.TypeFor[ObjectValue]() {
		return value.Interface().(ObjectValue).object, true
	}
	if value.Type() == reflect.TypeFor[*Object]() {
		return value.Interface().(*Object), true
	}
	return nil, false
}

func objectIdentity(object *Object) valueIdentity {
	return valueIdentity{kind: reflect.Pointer, pointer: reflect.ValueOf(object).Pointer(), valueType: reflect.TypeFor[*Object]()}
}

// Track active paths during export, so cycles fail without recursing
// forever while shared non-cyclic children can appear more than once.
type valueIdentity struct {
	kind      reflect.Kind
	pointer   uintptr
	length    int
	valueType reflect.Type
}

func valueReference(value reflect.Value) valueIdentity {
	return valueIdentity{kind: value.Kind(), pointer: value.Pointer(), length: value.Len(), valueType: value.Type()}
}

func jsonValue(value reflect.Value, active map[valueIdentity]bool) (any, error) {
	for value.IsValid() && value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	if !value.IsValid() || omittedJSONValue(value) || (value.CanInterface() && IsNull(value.Interface())) {
		return nil, nil
	}
	if array, ok := referenceArray(value); ok {
		if array != nil {
			identity := arrayIdentity(array)
			if active[identity] {
				return nil, &json.UnsupportedValueError{Value: value, Str: "encountered a cycle via jsonjs.Array"}
			}
			active[identity] = true
			defer delete(active, identity)
		}
		result := make([]any, array.Len())
		for i := range result {
			child, err := jsonValue(reflect.ValueOf(array.Get(i)), active)
			if err != nil {
				return nil, err
			}
			result[i] = child
		}
		return result, nil
	}
	if object, ok := objectValue(value); ok {
		if object != nil {
			identity := objectIdentity(object)
			if active[identity] {
				return nil, &json.UnsupportedValueError{Value: value, Str: "encountered a cycle via jsonjs.Object"}
			}
			active[identity] = true
			defer delete(active, identity)
		}
		return jsonProperties(object.Entries(), active)
	}
	if value.Kind() == reflect.Map || value.Kind() == reflect.Slice {
		identity := valueReference(value)
		if active[identity] {
			return nil, &json.UnsupportedValueError{Value: value, Str: "encountered a cycle via " + value.Type().String()}
		}
		active[identity] = true
		defer delete(active, identity)
	}
	switch value.Kind() {
	case reflect.String:
		return json.RawMessage(QuoteString(value.String())), nil
	case reflect.Bool:
		return value.Bool(), nil
	case reflect.Slice, reflect.Array:
		array := make([]any, value.Len())
		for i := range array {
			child, err := jsonValue(value.Index(i), active)
			if err != nil {
				return nil, err
			}
			array[i] = child
		}
		return array, nil
	case reflect.Map:
		// Build escaped keys directly: encoding/json would replace WTF-8
		// surrogate bytes and could collapse distinct JavaScript properties.
		// Native maps have no insertion order; keep deterministic key order,
		// with JavaScript's numeric index keys first.
		keys := value.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		properties := make([]Property, 0, len(keys))
		for _, key := range keys {
			properties = append(properties, Property{Name: key.String(), Value: value.MapIndex(key).Interface()})
		}
		return jsonProperties(properties, active)
	}
	var number float64
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number = float64(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		number = float64(value.Uint())
	case reflect.Float32, reflect.Float64:
		number = value.Float()
	default:
		return nil, &json.UnsupportedTypeError{Type: value.Type()}
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, nil
	}
	if number == 0 {
		return float64(0), nil
	}
	return number, nil
}

func jsonProperties(properties []Property, active map[valueIdentity]bool) (any, error) {
	var object ObjectFields
	for _, property := range properties {
		entry := reflect.ValueOf(property.Value)
		if omittedJSONValue(entry) {
			continue
		}
		child, err := jsonValue(entry, active)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(child)
		if err != nil {
			return nil, err
		}
		object.Set(QuoteString(property.Name), raw)
	}
	return json.RawMessage(object.Marshal()), nil
}

func omittedJSONValue(value reflect.Value) bool {
	for value.IsValid() && value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	return value.IsValid() && (value.Type() == reflect.TypeFor[undefinedValue]() || value.Kind() == reflect.Func)
}

// Arbitrary Go channels, pointers and structs have no JSON-shaped counterpart.
// Mixed arrays, nested objects, functions and cycles remain admissible values;
// export detects cycles separately.
func supportedValue(value reflect.Value, seen map[valueIdentity]bool) bool {
	for value.IsValid() && value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	if array, ok := referenceArray(value); ok {
		if array == nil {
			return true
		}
		identity := arrayIdentity(array)
		if seen[identity] {
			return true
		}
		seen[identity] = true
		for _, child := range array.values {
			if !supportedValue(reflect.ValueOf(child), seen) {
				return false
			}
		}
		return true
	}
	if object, ok := objectValue(value); ok {
		if object == nil {
			return true
		}
		identity := objectIdentity(object)
		if seen[identity] {
			return true
		}
		seen[identity] = true
		for _, property := range object.Entries() {
			if !supportedValue(reflect.ValueOf(property.Value), seen) {
				return false
			}
		}
		return true
	}
	if !value.IsValid() || value.Kind() == reflect.Func || scalarKind(value.Kind()) != reflect.Invalid {
		return true
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return false
		}
	case reflect.Slice, reflect.Array:
	default:
		return false
	}
	if value.Kind() != reflect.Array {
		identity := valueReference(value)
		if seen[identity] {
			return true
		}
		seen[identity] = true
	}
	if value.Kind() == reflect.Map {
		entries := value.MapRange()
		for entries.Next() {
			if !supportedValue(entries.Value(), seen) {
				return false
			}
		}
	} else {
		for i := 0; i < value.Len(); i++ {
			if !supportedValue(value.Index(i), seen) {
				return false
			}
		}
	}
	return true
}

func scalarKind(kind reflect.Kind) reflect.Kind {
	switch kind {
	case reflect.String, reflect.Bool:
		return kind
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return reflect.Float64
	default:
		return reflect.Invalid
	}
}
