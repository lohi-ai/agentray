// Package opcore is the one-definition operation layer. An Operation is declared
// exactly once and projected onto four adapters: an in-process agent Tool
// (tool.go), a REST endpoint (http.go), a client CLI transport (client.go), and
// an MCP server for external agents (mcp.go).
//
// The point is twofold. First, it kills the drift that comes from implementing a
// capability twice (once as an agent tool, once as a web endpoint) — both now
// share a single handler. Second, a handler receives infra ONLY through
// CallContext.Deps, an opaque bundle of repository interfaces, so the agent that
// drives these operations can never reach the database or the queue directly:
//
//	[Tool | REST | CLI]  ->  Operation handler  ->  Deps (repo interfaces)  ->  infra
//
// opcore depends on agentcore (a leaf) and Echo; it knows nothing about storage.
package opcore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// CallContext carries the per-invocation scope a handler runs under: the project
// it acts on, the agent run that triggered it (empty for web/CLI callers), and
// the dependency bundle it may touch. Deps is opaque to opcore (the usecase layer
// type-asserts it) which keeps opcore free of any storage import.
type CallContext struct {
	ProjectID string
	RunID     string
	Deps      any
}

// Operation is one capability, defined once. I is the decoded input struct, O the
// result value (JSON-marshalled on the way out). The handler is pure usecase: it
// reads infra through cc.Deps and never imports a pool or a queue.
type Operation[I any, O any] struct {
	Name     string // stable id the model/CLI calls, e.g. "run_sql"
	Summary  string // one-line description shown to the model and in CLI help
	Scope    string // permission-scope key (agentruntime policy); "" = unrestricted
	Terminal bool   // tool adapter: end the agent run after this call succeeds
	Handler  func(ctx context.Context, cc CallContext, in I) (O, error)
}

// Spec is the type-erased view of an Operation that the registry and the three
// adapters consume. Operation[I,O] satisfies it.
type Spec interface {
	OpName() string
	OpSummary() string
	OpScope() string
	OpTerminal() bool
	OpSchema() map[string]any
	OpInvoke(ctx context.Context, cc CallContext, rawArgs string) (string, error)
}

func (o Operation[I, O]) OpName() string         { return o.Name }
func (o Operation[I, O]) OpSummary() string      { return o.Summary }
func (o Operation[I, O]) OpScope() string        { return o.Scope }
func (o Operation[I, O]) OpTerminal() bool        { return o.Terminal }
func (o Operation[I, O]) OpSchema() map[string]any { return schemaOf[I]() }

// OpInvoke decodes rawArgs into I, runs the handler, and marshals O back to JSON.
// An empty/null argument string decodes to the zero input, so callers that take
// no parameters can pass "" or "{}". Required fields (per the input struct tags)
// are validated up front, uniformly, before the handler runs — so every
// operation rejects a missing argument the same actionable way, instead of each
// handler hand-rolling its own check.
func (o Operation[I, O]) OpInvoke(ctx context.Context, cc CallContext, rawArgs string) (string, error) {
	if err := validateRequired[I](rawArgs); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	}
	var in I
	if raw := strings.TrimSpace(rawArgs); raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", o.Name, err)
		}
	}
	out, err := o.Handler(ctx, cc, in)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// schemaOf derives a JSON-Schema object from the input struct's fields using
// `json` tags for names, `desc` tags for descriptions, and `required:"true"` to
// mark required fields. Unexported and json:"-" fields are skipped.
func schemaOf[I any]() map[string]any {
	t := reflect.TypeOf(*new(I))
	if t == nil || t.Kind() != reflect.Struct {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonName(f)
		if name == "-" {
			continue
		}
		prop := map[string]any{"type": jsonType(f.Type)}
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Array {
			prop["items"] = map[string]any{"type": jsonType(f.Type.Elem())}
		}
		if d := f.Tag.Get("desc"); d != "" {
			prop["description"] = d
		}
		props[name] = prop
		if f.Tag.Get("required") == "true" {
			required = append(required, name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// requiredOf returns the json names of the input struct's required fields (tagged
// `required:"true"`). Shared by schemaOf (what the model is told) and
// validateRequired (what is enforced) so the advertised contract and the checked
// contract can never drift.
func requiredOf[I any]() []string {
	t := reflect.TypeOf(*new(I))
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	var req []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && f.Tag.Get("required") == "true" {
			if name := jsonName(f); name != "-" {
				req = append(req, name)
			}
		}
	}
	return req
}

// validateRequired enforces that every required field is present and non-empty in
// the raw arguments. The error names the exact missing fields so the model can
// self-correct in one turn rather than looping.
func validateRequired[I any](rawArgs string) error {
	req := requiredOf[I]()
	if len(req) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if raw := strings.TrimSpace(rawArgs); raw != "" && raw != "null" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	var missing []string
	for _, f := range req {
		if v, ok := m[f]; !ok || isEmptyJSON(v) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// isEmptyJSON treats a JSON null, empty string, or whitespace as "not provided".
func isEmptyJSON(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s == "" || s == `""` || s == "null"
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	if name := strings.Split(tag, ",")[0]; name != "" {
		return name
	}
	return f.Name
}

func jsonType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Ptr:
		return jsonType(t.Elem())
	default:
		return "string"
	}
}
