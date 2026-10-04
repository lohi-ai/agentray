package telemetry

import "github.com/lohi-ai/agentray/internal/jsonjs"

// Property, Object and Array share the native JSON value implementation used
// by agent arguments. No JavaScript runtime or interface hierarchy is involved.
type Property = jsonjs.Property
type Object = jsonjs.Object
type Array = jsonjs.Array

func NewObject(properties ...Property) *Object        { return jsonjs.NewObject(properties...) }
func NewArray(values ...any) *Array                   { return jsonjs.NewArray(values...) }
func NewAttributes(properties ...Property) Attributes { return jsonjs.NewObjectValue(properties...) }
