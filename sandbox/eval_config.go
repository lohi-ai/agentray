package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	ToolEval                    = "eval"
	defaultEvalTimeoutSeconds   = 30
	defaultEvalMaxOutputBytes   = 50 * 1024
	maxEvalOutputBytes          = 1024 * 1024
	maxEvalCellTimeoutSeconds   = 300
	defaultEvalRegistryCapacity = 16
)

// EvalConfig selects the operator-provisioned Python runtime used by the
// persistent eval tool. The zero-value JSON object is useful on a laptop
// (python3 on PATH); a server normally supplies an image containing Python.
type EvalConfig struct {
	Python         EvalPythonConfig `json:"python,omitempty"`
	TimeoutSeconds int              `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int              `json:"max_output_bytes,omitempty"`
}

type EvalPythonConfig struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Image   string   `json:"image,omitempty"`
}

func ParseEvalConfig(raw string) (EvalConfig, error) {
	var cfg EvalConfig
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid eval config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return cfg, fmt.Errorf("invalid eval config: expected one JSON object")
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func ValidateEvalConfig(raw string) error {
	_, err := ParseEvalConfig(raw)
	return err
}

func (c *EvalConfig) validate() error {
	c.Python.Command = strings.TrimSpace(c.Python.Command)
	c.Python.Image = strings.TrimSpace(c.Python.Image)
	if c.Python.Command == "" {
		c.Python.Command = "python3"
	}
	for i, arg := range c.Python.Args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("invalid eval config: python.args[%d] contains NUL", i)
		}
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = defaultEvalTimeoutSeconds
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > maxEvalCellTimeoutSeconds {
		return fmt.Errorf("invalid eval config: timeout_seconds must be between 1 and %d", maxEvalCellTimeoutSeconds)
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = defaultEvalMaxOutputBytes
	}
	if c.MaxOutputBytes < 4096 || c.MaxOutputBytes > maxEvalOutputBytes {
		return fmt.Errorf("invalid eval config: max_output_bytes must be between 4096 and %d", maxEvalOutputBytes)
	}
	return nil
}
