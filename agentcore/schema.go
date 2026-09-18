package agentcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// outputValidator is the compiled, run-time half of OutputSchema. Keeping the
// compiler behind this small function type means the rest of the loop only
// knows the policy it must enforce: a final text answer either satisfies the
// configured schema or the run fails without accepting it as its result.
type outputValidator func(string) error

// compileOutputValidator turns the provider-neutral schema into a local
// validator at build time. Providers may ignore ChatRequest.OutputSchema, so a
// successful build must establish that the fallback validator itself is valid
// before the first (potentially billable) model call is made.
func compileOutputValidator(spec *OutputSchema) (outputValidator, error) {
	if spec == nil {
		return nil, nil
	}
	if len(spec.Schema) == 0 {
		return nil, fmt.Errorf("agentcore: output schema must be a non-empty JSON Schema object")
	}

	// OutputSchema is commonly authored as Go maps containing []string and
	// integer values. Normalize it through JSON first so the compiler sees the
	// same JSON-native shapes a schema loaded from the wire would contain.
	raw, err := json.Marshal(spec.Schema)
	if err != nil {
		return nil, fmt.Errorf("agentcore: output schema is not JSON-serializable: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("agentcore: output schema is not valid JSON: %w", err)
	}

	const resource = "https://agentcore.local/output-schema.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("agentcore: output schema: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("agentcore: output schema does not compile: %w", err)
	}

	return func(answer string) error {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(answer))
		if err != nil {
			return fmt.Errorf("agentcore: structured output is not valid JSON: %w", err)
		}
		if err := compiled.Validate(doc); err != nil {
			return fmt.Errorf("agentcore: structured output does not match schema: %w", err)
		}
		return nil
	}, nil
}
