package engine

import (
	"math"
	"math/big"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Pi uses binary floating-point remainders with an absolute tolerance rather
// than the validator's exact rational divisibility test.
type argumentMultipleOf struct {
	divisor float64
	want    *big.Rat
}

func (rule *argumentMultipleOf) Validate(ctx *jsonschema.ValidatorContext, value any) {
	number, ok := value.(float64)
	if !ok {
		return
	}
	if math.Trunc(number) == number && math.Mod(1/rule.divisor, 1) == 0 {
		return
	}
	remainder := math.Mod(number, rule.divisor)
	distance := math.Min(math.Abs(remainder), math.Min(math.Abs(remainder-rule.divisor), math.Abs(remainder+rule.divisor)))
	if distance < 1e-10 {
		return
	}
	ctx.AddError(&kind.MultipleOf{Got: new(big.Rat).SetFloat64(number), Want: rule.want})
}

func registerArgumentNumbers(compiler *jsonschema.Compiler) {
	compiler.AssertVocabs()
	compiler.RegisterVocabulary(&jsonschema.Vocabulary{
		URL: "https://agentcore.local/pi-numeric-semantics",
		Compile: func(ctx *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
			divisor, ok := obj["multipleOf"].(float64)
			if !ok {
				return nil, nil
			}
			// Built-in keyword compilation precedes vocabulary compilation. Replace
			// only this numeric check on the compiler-owned schema, including refs.
			compiled := ctx.Enqueue(nil)
			want := compiled.MultipleOf
			compiled.MultipleOf = nil
			return &argumentMultipleOf{divisor: divisor, want: want}, nil
		},
	})
}
