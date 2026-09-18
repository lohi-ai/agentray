package sandbox

import (
	"strings"
	"testing"
)

func TestParseEvalConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := ParseEvalConfig(`{}`)
	if err != nil {
		t.Fatalf("ParseEvalConfig: %v", err)
	}
	if cfg.Python.Command != "python3" || cfg.TimeoutSeconds != 30 || cfg.MaxOutputBytes != 50*1024 {
		t.Fatalf("defaults = %+v", cfg)
	}

	for _, raw := range []string{
		`{"unknown":true}`,
		`{} {}`,
		`{"timeout_seconds":301}`,
		`{"max_output_bytes":100}`,
	} {
		if _, err := ParseEvalConfig(raw); err == nil {
			t.Errorf("ParseEvalConfig(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestParseEvalConfigNormalizesRuntime(t *testing.T) {
	cfg, err := ParseEvalConfig(`{
		"python":{"command":"  /opt/python  ","args":["-X","dev"],"image":" python:3.12 "},
		"timeout_seconds":12,"max_output_bytes":4096
	}`)
	if err != nil {
		t.Fatalf("ParseEvalConfig: %v", err)
	}
	if cfg.Python.Command != "/opt/python" || cfg.Python.Image != "python:3.12" || strings.Join(cfg.Python.Args, " ") != "-X dev" {
		t.Fatalf("runtime = %+v", cfg.Python)
	}
}
