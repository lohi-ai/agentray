//go:build linux

package storage

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// duckdb_sandbox_spawn_linux.go — the Linux half of the process bound.
//
// A hostile query cannot be bounded from inside the engine (measured: a scalar
// minted a 500 MB cell under a 122 MiB `memory_limit`, and an engine-side guard
// still OOM-killed a constrained container). The kernel can bound a process, so
// the child bounds itself here: RLIMIT_AS caps its address space, and its OOM
// score makes it the kernel's first choice if the container is ever under
// pressure. Both are set by the child itself, before the engine opens, because
// the Go runtime's own virtual reservations must be accounted for first.

// sandboxOOMScoreAdj is the OOM score a child asks for. Raising it is always
// permitted without privileges, and it makes the kernel's cgroup OOM killer
// pick the sandbox over the API when the container is under pressure.
const sandboxOOMScoreAdj = 1000

// applyChildProcessBounds caps this process's address space at its current
// virtual size plus budget, and returns the applied limit (0 where no bound was
// applied). The budget is what DuckDB may additionally allocate; a fixed limit
// is unusable because the Go runtime alone holds hundreds of MiB of virtual
// address space before any engine exists.
func applyChildProcessBounds(budget int64) (uint64, error) {
	// Best effort, and deliberately not fatal: the OOM score is a second line of
	// defence, not the bound itself.
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(sandboxOOMScoreAdj)), 0o644)

	if budget <= 0 {
		return 0, nil
	}
	current, err := processVirtualBytes()
	if err != nil {
		return 0, err
	}
	limit := current + uint64(budget)
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		return 0, fmt.Errorf("setrlimit(RLIMIT_AS, %d): %w", limit, err)
	}
	return limit, nil
}

// processVirtualBytes reads this process's virtual size from /proc/self/statm,
// whose first field is the total program size in pages.
func processVirtualBytes() (uint64, error) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, fmt.Errorf("statm: no fields")
	}
	pages, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * uint64(os.Getpagesize()), nil
}
