//go:build !linux

package storage

import (
	"log"
	"sync"
)

// duckdb_sandbox_spawn_other.go — the sandbox's process bound off Linux.
//
// Production is Linux (the api image is debian:bookworm-slim), and that is
// where the kernel bound is enforced. Darwin rejects both RLIMIT_AS and
// RLIMIT_DATA with EINVAL, so there is no address-space bound to apply here;
// the isolation that remains is the process boundary itself (a hostile query
// takes down the child, never the API), plus the engine's own memory_limit and
// the child-side result caps.

var sandboxNoRlimitLogged sync.Once

func applyChildProcessBounds(budget int64) (uint64, error) {
	if budget > 0 {
		sandboxNoRlimitLogged.Do(func() {
			log.Printf("agentray: sandbox child has no address-space bound on this platform (no RLIMIT_AS); process isolation and the engine memory_limit still apply")
		})
	}
	return 0, nil
}
