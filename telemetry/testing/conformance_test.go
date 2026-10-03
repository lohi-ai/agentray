package telemetrytest_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/telemetry"
	telemetrytest "github.com/lohi-ai/agentray/telemetry/testing"
)

// Exercise the public callback backend seam as well as the native recorder.
func forwardingContext(parent telemetry.Context) telemetry.Context {
	return telemetry.NewContext(func(options telemetry.SpanOptions, callback func(*telemetry.Span) error) error {
		return parent.StartSpan(options, func(span *telemetry.Span) error {
			return callback(telemetry.NewSpan(forwardingContext(span.Context()), telemetry.SpanCallbacks{
				AddEvent: span.AddEvent, SetAttributes: span.SetAttributes, SetStatus: span.SetStatus,
			}))
		})
	})
}

func TestAdapterConformance(t *testing.T) {
	for _, forward := range []bool{false, true} {
		name := "memory"
		if forward {
			name = "callback-backend"
		}
		t.Run(name, func(t *testing.T) {
			created, closed := 0, 0
			suite := telemetrytest.CreateAdapterConformance(func() (*telemetrytest.AdapterFixture, error) {
				created++
				recorder := telemetry.NewInMemory()
				parent := recorder.Context
				if forward {
					parent = forwardingContext(parent)
				}
				return &telemetrytest.AdapterFixture{Context: parent, GetSpans: func() ([]telemetry.RecordedSpan, error) { return recorder.GetSpans(), nil }, Close: func() error { closed++; return nil }}, nil
			})
			for _, test := range suite {
				t.Run(test.Group+"/"+test.Name, func(t *testing.T) {
					if err := test.Run(); err != nil {
						t.Fatal(err)
					}
				})
			}
			if created != 9 || closed != created {
				t.Fatalf("fixture isolation/cleanup: created=%d closed=%d", created, closed)
			}
			// The reusable suite must cover every case exported by the pinned source.
			data, err := os.ReadFile("../testdata/pi-schema.json")
			if err != nil {
				t.Fatal(err)
			}
			type namePair struct{ Group, Name string }
			var fixture struct{ Conformance []namePair }
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			got := []namePair{}
			for _, test := range suite {
				got = append(got, namePair{test.Group, test.Name})
			}
			if !reflect.DeepEqual(got, fixture.Conformance) {
				t.Fatal("conformance coverage differs from upstream")
			}
		})
	}
}

func TestConformanceRejectsBadAdapterAndCleansUp(t *testing.T) {
	cleanupFailure := errors.New("cleanup failure")
	closed := 0
	suite := telemetrytest.CreateAdapterConformance(func() (*telemetrytest.AdapterFixture, error) {
		return &telemetrytest.AdapterFixture{GetSpans: func() ([]telemetry.RecordedSpan, error) { return []telemetry.RecordedSpan{}, nil }, Close: func() error { closed++; return cleanupFailure }}, nil
	})
	err := suite[0].Run()
	if err == nil || !strings.Contains(err.Error(), "expected recorded span success") || !errors.Is(err, cleanupFailure) || closed != 1 {
		t.Fatalf("failure/cleanup lost: %v closed=%d", err, closed)
	}
	factoryFailure := errors.New("factory failure")
	suite = telemetrytest.CreateAdapterConformance(func() (*telemetrytest.AdapterFixture, error) { return nil, factoryFailure })
	if err := suite[0].Run(); err != factoryFailure {
		t.Fatalf("factory error changed: %v", err)
	}
}
