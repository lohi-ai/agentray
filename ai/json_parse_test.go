package ai

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func TestPiJSONParsingOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/pi-streaming.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		JSON []struct {
			Input, Repaired     string
			Complete, Streaming json.RawMessage
			CompleteError       bool
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for i, fixture := range fixtures.JSON {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if actual := RepairJSON(fixture.Input); actual != fixture.Repaired {
				t.Fatalf("repair %q: want %q, got %q", fixture.Input, fixture.Repaired, actual)
			}
			complete, err := ParseJSONWithRepair(fixture.Input)
			if (err != nil) != fixture.CompleteError {
				t.Fatalf("complete %q: want error %v, got %v", fixture.Input, fixture.CompleteError, err)
			}
			if err == nil {
				assertPiJSON(t, fixture.Complete, complete)
			}
			assertPiJSON(t, fixture.Streaming, ParseStreamingJSON(fixture.Input))
		})
	}
}
