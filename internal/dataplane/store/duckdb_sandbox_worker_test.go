package storage

import (
	"fmt"
	"os"
	"testing"
)

// TestMain lets this package's own test binary play the sandbox child role.
//
// The parent spawns `os.Executable()` with SandboxWorkerArgv, which under `go
// test` is the compiled test binary — so the dispatcher has to exist here or
// every test that executes run_sql would spawn a process that tries to run the
// test suite. The real entry point is the same one the shipped binary uses
// (cmd/server), so the tests exercise the production path, not a stand-in.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SandboxWorkerArgv {
		if err := RunSandboxWorker(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", SandboxWorkerArgv, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
