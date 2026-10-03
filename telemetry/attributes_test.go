package telemetry_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/lohi-ai/agentray/telemetry"
)

type attributeValueSpec struct {
	Kind   string               `json:"kind"`
	Value  string               `json:"value"`
	Values []attributeValueSpec `json:"values"`
}

func attributeInput(t *testing.T, spec attributeValueSpec) any {
	t.Helper()
	var err error
	var result any
	switch spec.Kind {
	case "omit":
		return nil
	case "string":
		return spec.Value
	case "bool":
		return spec.Value == "true"
	case "any[]":
		values := make([]any, len(spec.Values))
		for i, value := range spec.Values {
			values[i] = attributeInput(t, value)
		}
		return values
	case "uint8[]":
		var values []uint8
		for _, value := range spec.Values {
			n, err := strconv.ParseUint(value.Value, 10, 8)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, uint8(n))
		}
		return values
	case "float64[]":
		var values []float64
		for _, value := range spec.Values {
			values = append(values, attributeInput(t, value).(float64))
		}
		return values
	case "float64", "float32":
		var n float64
		n, err = strconv.ParseFloat(spec.Value, 64)
		result = n
		if spec.Kind == "float32" {
			result = float32(n)
		}
	case "int", "int8", "int16", "int64":
		var n int64
		n, err = strconv.ParseInt(spec.Value, 10, 64)
		switch spec.Kind {
		case "int":
			result = int(n)
		case "int8":
			result = int8(n)
		case "int16":
			result = int16(n)
		default:
			result = n
		}
	case "uint16", "uint64":
		var n uint64
		n, err = strconv.ParseUint(spec.Value, 10, 64)
		result = n
		if spec.Kind == "uint16" {
			result = uint16(n)
		}
	default:
		t.Fatalf("unknown attribute kind %q", spec.Kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPiAttributeJSONOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/pi-attributes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string `json:"upstreamCommit"`
		Cases          []struct {
			Name     string              `json:"name"`
			Key      string              `json:"key"`
			Start    attributeValueSpec  `json:"start"`
			Update   *attributeValueSpec `json:"update"`
			Expected json.RawMessage     `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != "eeac84ca92498ac18b6832754d01aef1d3c5f654" || len(fixture.Cases) != 26 {
		t.Fatal("unexpected attribute oracle coverage")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			recorder := telemetry.NewInMemory()
			key := tc.Key
			if key == "" {
				key = "value"
			}
			attrs := telemetry.Attributes{key: attributeInput(t, tc.Start)}
			var active []telemetry.RecordedSpan
			if err := recorder.StartSpan(telemetry.SpanOptions{Name: tc.Name, Attributes: attrs}, func(span *telemetry.Span) error {
				span.AddEvent("copy", attrs)
				if tc.Update != nil {
					span.SetAttributes(telemetry.Attributes{key: attributeInput(t, *tc.Update)})
				}
				active = recorder.GetSpans()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(map[string]any{"active": active, "settled": recorder.GetSpans()})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			decoder := json.NewDecoder(bytes.NewReader(actual))
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			decoder = json.NewDecoder(bytes.NewReader(tc.Expected))
			decoder.UseNumber()
			if err := decoder.Decode(&want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go: %s\nPi: %s", actual, tc.Expected)
			}
		})
	}
}

type attributeString string
type attributeNumber int64
type attributeBool bool
type attributeBytes []byte

func (attributeString) MarshalJSON() ([]byte, error) { panic("must not serialize user string") }
func (attributeNumber) MarshalJSON() ([]byte, error) { panic("must not serialize user number") }
func (attributeBool) MarshalJSON() ([]byte, error)   { panic("must not serialize user bool") }
func (attributeBytes) MarshalJSON() ([]byte, error)  { panic("must not serialize user slice") }

func TestAttributeExportIsPassiveAndLeavesSnapshotsUnchanged(t *testing.T) {
	recorder := telemetry.NewInMemory()
	bytes := attributeBytes{1, 2}
	numbers := []any{attributeNumber(3), int8(4), float32(0.5)}
	attrs := telemetry.Attributes{
		"bytes": bytes, "numbers": numbers,
		"text": attributeString("plain"), "flag": attributeBool(true),
		"nan": math.NaN(), "negativeZero": math.Copysign(0, -1),
	}
	if err := recorder.StartSpan(telemetry.SpanOptions{Name: "passive-export", Attributes: attrs}, func(span *telemetry.Span) error {
		span.AddEvent("values", attrs)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bytes[0], numbers[0] = 255, "changed input"
	snapshot := recorder.GetSpans()[0]
	for _, got := range []telemetry.Attributes{snapshot.Attributes, snapshot.Events[0].Attributes} {
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"bytes":[1,2],"flag":true,"nan":null,"negativeZero":0,"numbers":[3,4,0.5],"text":"plain"}` {
			t.Fatalf("export called a user serializer or changed numeric values: %s", raw)
		}
		if got["bytes"].(attributeBytes)[0] != 1 || got["numbers"].([]any)[0] != attributeNumber(3) || !math.IsNaN(got["nan"].(float64)) || !math.Signbit(got["negativeZero"].(float64)) {
			t.Fatal("export changed the detached in-memory snapshot")
		}
		got["bytes"].(attributeBytes)[0] = 200
		got["numbers"].([]any)[0] = "changed snapshot"
	}
	if latest := recorder.GetSpans()[0]; latest.Attributes["bytes"].(attributeBytes)[0] != 1 || latest.Events[0].Attributes["numbers"].([]any)[0] != attributeNumber(3) {
		t.Fatal("snapshot alias reached the recorder")
	}
}

func TestMixedAttributeCategoriesStillRejectAtomically(t *testing.T) {
	for _, invalid := range []any{[]any{1, "text"}, []any{1, true}, []any{1, nil}, [][]int{{1}}, map[string]any{"nested": 1}, []map[string]int{}} {
		recorder := telemetry.NewInMemory()
		called := false
		_ = recorder.StartSpan(telemetry.SpanOptions{Name: "invalid-admission", Attributes: telemetry.Attributes{"invalid": invalid}}, func(*telemetry.Span) error { called = true; return nil })
		if !called || len(recorder.GetSpans()) != 0 {
			t.Fatalf("invalid input affected callback/recorded partial span: %T", invalid)
		}
		_ = recorder.StartSpan(telemetry.SpanOptions{Name: "atomic", Attributes: telemetry.Attributes{"kept": true}}, func(span *telemetry.Span) error {
			span.SetAttributes(telemetry.Attributes{"partial": true, "invalid": invalid})
			span.AddEvent("invalid", telemetry.Attributes{"partial": true, "invalid": invalid})
			return nil
		})
		span := recorder.GetSpans()[0]
		if !reflect.DeepEqual(span.Attributes, telemetry.Attributes{"kept": true}) || len(span.Events) != 0 {
			t.Fatalf("invalid mutation was partially recorded: %+v", span)
		}
		if _, err := json.Marshal(telemetry.Attributes{"invalid": invalid}); err == nil {
			t.Fatalf("invalid direct export accepted: %T", invalid)
		}
	}
}
