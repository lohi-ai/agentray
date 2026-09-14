package app

import (
	"fmt"
	"os"
	"testing"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// TestMain lets this package's test binary play the sandbox child role.
//
// run_sql executes untrusted SQL by re-executing `os.Executable()` with
// storage.SandboxWorkerArgv — which, under `go test`, is this compiled test
// binary. Without this dispatcher any test in this package that reaches a real
// store would spawn the app test suite again instead of a sandbox child, and
// hang rather than fail. The child runs the same entry point the shipped binary
// dispatches (cmd/server), so a test exercises the production path.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == storage.SandboxWorkerArgv {
		if err := storage.RunSandboxWorker(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", storage.SandboxWorkerArgv, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
