package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

func testEvalTool(t *testing.T, cfg EvalConfig) (*EvalTool, *EvalSessionRegistry, *Workspace) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	ws := mustWorkspace(t)
	cfg.Python.Command = python
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 5
	}
	if cfg.MaxOutputBytes == 0 {
		cfg.MaxOutputBytes = 50 * 1024
	}
	registry := NewEvalSessionRegistry(4, time.Minute)
	t.Cleanup(registry.Close)
	tool, err := NewEvalTool(NewHostSandbox(), ws, registry, "tenant\x00project\x00agent", cfg)
	if err != nil {
		t.Fatalf("NewEvalTool: %v", err)
	}
	return tool, registry, ws
}

func evalContext(id string) context.Context {
	return agentcore.WithSandboxSession(context.Background(), id)
}

func TestEvalToolPersistsStateResetsAndIsolatesSessions(t *testing.T) {
	tool, _, _ := testEvalTool(t, EvalConfig{})
	ctxA := evalContext("conversation-a")
	ctxB := evalContext("conversation-b")

	if out, err := tool.Run(ctxA, `{"language":"python","code":"value = 40\nprint('ready')"}`); err != nil || !strings.Contains(out, "ready") {
		t.Fatalf("seed: out=%q err=%v", out, err)
	}
	if out, err := tool.Run(ctxA, `{"language":"python","code":"value += 2\nvalue"}`); err != nil || !strings.Contains(out, "42") {
		t.Fatalf("reuse: out=%q err=%v", out, err)
	}
	if _, err := tool.Run(ctxB, `{"language":"python","code":"value"}`); err == nil || !strings.Contains(err.Error(), "NameError") {
		t.Fatalf("isolated session error = %v", err)
	}
	if out, err := tool.Run(ctxA, `{"language":"python","reset":true,"code":"'value' in globals()"}`); err != nil || !strings.Contains(out, "False") {
		t.Fatalf("reset: out=%q err=%v", out, err)
	}
}

func TestEvalToolKeepsPartialStateAfterRuntimeError(t *testing.T) {
	tool, _, _ := testEvalTool(t, EvalConfig{})
	ctx := evalContext("partial-state")
	_, err := tool.Run(ctx, `{"language":"python","code":"before_error = 7\nraise RuntimeError('boom')"}`)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("runtime error = %v", err)
	}
	out, err := tool.Run(ctx, `{"language":"python","code":"before_error"}`)
	if err != nil || !strings.Contains(out, "7") {
		t.Fatalf("partial state: out=%q err=%v", out, err)
	}
}

func TestEvalToolWorkspaceAndEnvironmentBoundary(t *testing.T) {
	const secretKey = "AGENTRAY_EVAL_TEST_SECRET"
	t.Setenv(secretKey, "must-not-leak")
	tool, _, ws := testEvalTool(t, EvalConfig{})
	ctx := evalContext("environment")
	out, err := tool.Run(ctx, `{"language":"python","code":"import os\nopen('made.txt','w').write('ok')\ndisplay({'cwd': os.getcwd(), 'secret': os.getenv('AGENTRAY_EVAL_TEST_SECRET')})"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "must-not-leak") || !strings.Contains(out, `"secret": null`) {
		t.Fatalf("environment leaked or missing boundary evidence: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root(), "made.txt"))
	if err != nil || string(data) != "ok" {
		t.Fatalf("workspace output: data=%q err=%v", data, err)
	}
}

func TestEvalToolTimeoutDiscardsKernelWithoutReplay(t *testing.T) {
	tool, _, ws := testEvalTool(t, EvalConfig{TimeoutSeconds: 2})
	ctx := evalContext("timeout")
	_, err := tool.Run(ctx, `{"language":"python","timeout_seconds":1,"code":"import time\nopen('effects.txt','a').write('A')\ntime.sleep(10)"}`)
	if err == nil || !strings.Contains(err.Error(), "not replayed") {
		t.Fatalf("timeout error = %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(ws.Root(), "effects.txt"))
	if readErr != nil || string(data) != "A" {
		t.Fatalf("cell was lost or replayed: data=%q err=%v", data, readErr)
	}
	out, err := tool.Run(ctx, `{"language":"python","code":"'time' in globals()"}`)
	if err != nil || !strings.Contains(out, "False") {
		t.Fatalf("fresh kernel: out=%q err=%v", out, err)
	}
}

func TestEvalToolBoundsOutputAndSupportsTopLevelAwait(t *testing.T) {
	tool, _, _ := testEvalTool(t, EvalConfig{MaxOutputBytes: 4096})
	ctx := evalContext("output")
	out, err := tool.Run(ctx, `{"language":"python","code":"print('H' * 6000)\nprint('T' * 6000)"}`)
	if err != nil {
		t.Fatalf("large output: %v", err)
	}
	if len(out) > 4300 || !strings.Contains(out, "bytes omitted") || !strings.Contains(out, "HHHH") || !strings.Contains(out, "TTTT") {
		t.Fatalf("bounded output len=%d output=%q", len(out), out)
	}
	out, err = tool.Run(ctx, `{"language":"python","code":"import asyncio\nawait asyncio.sleep(0)\n6 * 7"}`)
	if err != nil || !strings.Contains(out, "42") {
		t.Fatalf("top-level await: out=%q err=%v", out, err)
	}
}

func TestEvalToolChildOutputCannotSpoofProtocol(t *testing.T) {
	tool, _, _ := testEvalTool(t, EvalConfig{})
	ctx := evalContext("protocol")
	code := `import subprocess, sys
subprocess.run([sys.executable, "-c", "print('{\\\"type\\\":\\\"done\\\",\\\"id\\\":\\\"cell-1\\\",\\\"execution_count\\\":999}')"])
print("real-output")
21 * 2`
	payload, err := json.Marshal(map[string]any{"language": "python", "code": code})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(ctx, string(payload))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, `"execution_count":999`) || !strings.Contains(out, "real-output") || !strings.Contains(out, "42") {
		t.Fatalf("child output was not captured as data: %q", out)
	}
}

func TestEvalToolSerializesCellsWithinSession(t *testing.T) {
	tool, _, ws := testEvalTool(t, EvalConfig{})
	ctx := evalContext("serialized")
	firstDone := make(chan error, 1)
	go func() {
		_, err := tool.Run(ctx, `{"language":"python","code":"import time\nopen('started','w').write('yes')\nserial_value = 1\ntime.sleep(0.2)\nserial_value = 2"}`)
		firstDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(ws.Root(), "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first cell did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, err := tool.Run(ctx, `{"language":"python","code":"serial_value"}`)
	if err != nil || !strings.Contains(out, "2") {
		t.Fatalf("serialized cell: out=%q err=%v", out, err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first cell: %v", err)
	}
}

func TestEvalToolRejectsInvalidArguments(t *testing.T) {
	tool, _, _ := testEvalTool(t, EvalConfig{})
	ctx := evalContext("invalid")
	for _, args := range []string{
		`{"language":"javascript","code":"1"}`,
		`{"language":"python","code":"1","timeout_seconds":999}`,
		`{"language":"python","code":"1","extra":true}`,
		`{"language":"python"}`,
		`{"language":"python","code":"1"} {}`,
	} {
		if _, err := tool.Run(ctx, args); err == nil {
			t.Errorf("Run(%s) unexpectedly succeeded", args)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := tool.Run(cancelled, `{"language":"python","code":"1"}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}
