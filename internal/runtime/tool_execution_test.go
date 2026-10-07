package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2found/2ai/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Exercise the product composition through its real tool executor. Removing
// the infrastructure plugins must preserve gating before secret resolution,
// fail-closed resolution, and placeholder-only argument traces.
func TestBuildToolExecutionInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sandbox     bool
		credentials bool
		readOnly    bool
		resolveErr  bool
		payload     string
		wantResolve bool
		wantExecute bool
		wantReason  string
	}{
		{name: "sandbox and credentials", sandbox: true, credentials: true, wantResolve: true, wantExecute: true},
		{name: "credentials without sandbox", credentials: true, wantResolve: true, wantExecute: true},
		{name: "no infrastructure", wantExecute: true},
		{name: "guard denies before resolution", sandbox: true, credentials: true, payload: "/proc/self/environ", wantReason: "injection guard"},
		{name: "policy denies before resolution", sandbox: true, credentials: true, readOnly: true},
		{name: "resolver fails closed", credentials: true, resolveErr: true, wantResolve: true, wantReason: "credential unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var toolArgs string
			resolved, executed := false, false
			p := BuildParams{
				ProjectID: "project", Trigger: "chat", ReadOnly: tc.readOnly,
				Data:  (*storage.Store)(nil),
				Rungs: []agentcore.ModelRung{{Provider: stubProvider{name: "fixture"}, Model: "fixture"}},
				Tools: []agentcore.Tool{executionProbe{run: func(args string) {
					executed, toolArgs = true, args
				}}},
			}
			if tc.sandbox {
				p.Sandbox = paritySandbox{}
			}
			if tc.credentials {
				p.Credentials = executionResolver(func(args string) (string, error) {
					resolved = true
					if tc.resolveErr {
						return "", errors.New("credential unavailable")
					}
					return strings.ReplaceAll(args, "{{cred:KEY}}", "test-secret-value"), nil
				})
			}
			a, err := Build(p)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			host, err := a.OpenPiTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			args, _ := json.Marshal(map[string]string{"key": "{{cred:KEY}}", "payload": tc.payload})
			call, _ := json.Marshal(map[string]any{"toolCallId": "probe-1", "toolName": "execution_probe", "args": json.RawMessage(args)})
			result, audit, err := host.Execute(ctx, call, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resolved != tc.wantResolve || executed != tc.wantExecute || audit.Executed != tc.wantExecute || audit.Trace.Allowed != tc.wantExecute {
				t.Fatalf("resolved=%v executed=%v audit=%+v", resolved, executed, audit)
			}
			if tc.wantReason != "" && !strings.Contains(audit.Trace.Reason, tc.wantReason) {
				t.Fatalf("denial reason=%q", audit.Trace.Reason)
			}
			if audit.Trace.Args != string(args) || strings.Contains(string(result), "test-secret-value") {
				t.Fatal("executor exposed or changed credential placeholders")
			}
			if executed {
				want := string(args)
				if tc.credentials {
					want = strings.ReplaceAll(want, "{{cred:KEY}}", "test-secret-value")
				}
				if toolArgs != want {
					t.Fatal("tool received incorrect arguments")
				}
			}
		})
	}
}

type executionProbe struct{ run func(string) }

func (executionProbe) Name() string { return "execution_probe" }
func (executionProbe) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "execution_probe", Parameters: map[string]any{"type": "object"}}
}
func (p executionProbe) Run(_ context.Context, args string) (string, error) {
	p.run(args)
	return "ok", nil
}

type executionResolver func(string) (string, error)

func (r executionResolver) Resolve(_ context.Context, args string) (string, error) { return r(args) }
