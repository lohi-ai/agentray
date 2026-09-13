package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The sandbox's resource contract, exercised against real child processes.
//
// The threat is an agent-authored query, so these tests run the real machinery:
// a real child, the real engine, a real ingest writer on the main store. What
// they assert is the promise the API makes — a hostile query can cost the
// sandbox, never the API or another tenant's work.

// newTestSandboxPool builds a pool with the production defaults, optionally
// tightened, and always tears it down.
func newTestSandboxPool(t *testing.T, d *DuckDB, mutate func(*sandboxLimits)) (*sqlSandboxPool, context.Context) {
	t.Helper()
	limits := defaultSandboxLimits()
	if mutate != nil {
		mutate(&limits)
	}
	pool := newSQLSandboxPoolWithLimits(d, limits)
	t.Cleanup(pool.closeAll)
	return pool, context.Background()
}

func seedProject(t *testing.T, d *DuckDB, projectID string, n int) {
	t.Helper()
	events := make([]Event, 0, n)
	for range n {
		events = append(events, duckEvent(projectID, uuid.NewString(), "user-a", time.Now().UTC()))
	}
	if err := d.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
}

// A1 — a hostile query cannot terminate the trusted process, and cannot be
// answered with a wrong result either.
func TestSandboxIsolationHostileQuery(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	victim, sibling := uuid.NewString(), uuid.NewString()
	seedProject(t, d, victim, 1)
	seedProject(t, d, sibling, 3)
	pool, _ := newTestSandboxPool(t, d, nil)

	// The trusted path: this is the API's own writer, the one a cgroup OOM used
	// to take with it. It must keep committing across the whole burst.
	stop := make(chan struct{})
	writerErr := make(chan error, 1)
	var commits atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				writerErr <- nil
				return
			default:
			}
			if err := d.InsertEvents(ctx, []Event{duckEvent(victim, uuid.NewString(), "user-w", time.Now().UTC())}); err != nil {
				writerErr <- err
				return
			}
			commits.Add(1)
		}
	}()

	// The kernel bound is only real where the kernel has one: on Linux the child
	// caps its own address space, and this is the assertion that the cap was
	// actually applied rather than merely intended.
	if _, err := pool.query(ctx, victim, `SELECT count(*) AS n FROM events`, nil); err != nil {
		t.Fatalf("warm-up query: %v", err)
	}
	pool.mu.Lock()
	warm := pool.sandboxes[victim]
	pool.mu.Unlock()
	if warm == nil {
		t.Fatal("no sandbox for the warmed project")
	}
	if runtime.GOOS == "linux" && warm.rlimitBytes == 0 {
		t.Fatal("sandbox child reports no address-space limit on linux")
	}
	if runtime.GOOS == "linux" {
		t.Logf("child address-space limit = %d MiB (own virtual size at open = %d MiB, budget = %d MiB)",
			warm.rlimitBytes>>20, (warm.rlimitBytes-uint64(sandboxRlimitBudget))>>20, sandboxRlimitBudget>>20)
	}
	if runtime.GOOS != "linux" && warm.rlimitBytes != 0 {
		t.Fatalf("sandbox child claims an address-space limit of %d on %s", warm.rlimitBytes, runtime.GOOS)
	}

	hostileQueries := []string{
		`SELECT repeat('x', 50000000) AS s`,
		`SELECT lpad('x', 40000000, 'y') AS s`,
		`SELECT string_agg(repeat('x', 1000), '') FROM range(200000)`,
	}
	if runtime.GOOS == "linux" {
		// The exact query the parent review measured: half a gigabyte minted
		// against a 122 MiB engine limit. With no address-space cap it took the
		// whole container down; with one it must fail inside the child.
		hostileQueries = append(hostileQueries, `SELECT repeat('x', 500000000) AS s`)
	}
	for _, hostile := range hostileQueries {
		rows, err := pool.query(ctx, victim, hostile, nil)
		if err == nil {
			t.Fatalf("hostile query %q was answered with %d rows", hostile, len(rows))
		}
		var se *SandboxError
		if !errors.As(err, &se) {
			t.Fatalf("hostile query %q error = %v (%T), want a typed sandbox error", hostile, err, err)
		}
		// Either the child refused it under a cap, or the child died and the
		// parent noticed. Both are containment; neither is a wrong answer.
		if !IsSandboxLimit(err) && !IsSandboxUnavailable(err) {
			t.Fatalf("hostile query %q error kind = %q, want a limit or an unavailable sandbox", hostile, se.Kind)
		}
		t.Logf("hostile %q -> kind=%s: %v", hostile, se.Kind, err)
	}

	// Another tenant's work is untouched.
	rows, err := pool.query(ctx, sibling, `SELECT count(*) AS n FROM events`, nil)
	if err != nil {
		t.Fatalf("sibling query after the burst: %v", err)
	}
	if got := rows[0]["n"]; got != int64(3) {
		t.Fatalf("sibling sees %v events, want 3", got)
	}

	close(stop)
	if err := <-writerErr; err != nil {
		t.Fatalf("trusted writer died during the hostile burst: %v", err)
	}
	if commits.Load() == 0 {
		t.Fatal("trusted writer made no progress during the hostile burst")
	}
}

// A2 — the admission slot is taken before any per-tenant engine exists.
func TestSandboxAdmissionPrecedesChildCreation(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.maxConcurrent = 1
		l.maxProjects = 2
	})

	const projects = 12
	var wg sync.WaitGroup
	for range projects {
		projectID := uuid.NewString()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.query(ctx, projectID, `SELECT count(*) AS n FROM events`, nil); err != nil {
				t.Errorf("query: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := pool.peakInFlight.Load(); got > 1 {
		t.Fatalf("peak in-flight children = %d, want <= 1 (admission must precede creation)", got)
	}
	if got := pool.liveChildren(); got > 2 {
		t.Fatalf("live children = %d, want <= sandboxMaxProjects (2)", got)
	}
	if got := pool.spawns.Load(); got != projects {
		t.Fatalf("spawns = %d, want %d — one child per project, none created while a slot was held", got, projects)
	}
	if got := pool.reaped.Load(); got != projects-2 {
		t.Fatalf("reaped = %d, want %d (every evicted child reaped)", got, projects-2)
	}
}

// A3 — engines, spill directories and connections are bounded and reclaimable.
func TestSandboxPoolBoundsAndReclaims(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.maxProjects = 2
		l.maxConcurrent = 2
	})

	for i := range 5 {
		if _, err := pool.query(ctx, uuid.NewString(), `SELECT count(*) AS n FROM events`, nil); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}

	if got := pool.liveChildren(); got != 2 {
		t.Fatalf("live children = %d, want 2", got)
	}
	if got := pool.reaped.Load(); got != 3 {
		t.Fatalf("reaped = %d, want 3", got)
	}
	// A dead child's spill directory is gone; a live one still has it.
	dirs, err := filepath.Glob(filepath.Join(d.tmpDir(), "sandbox-*"))
	if err != nil {
		t.Fatalf("glob spill dirs: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("spill dirs = %v, want 2", dirs)
	}

	pool.closeAll()
	dirs, err = filepath.Glob(filepath.Join(d.tmpDir(), "sandbox-*"))
	if err != nil {
		t.Fatalf("glob spill dirs: %v", err)
	}
	if len(dirs) != 0 {
		t.Fatalf("spill dirs after closeAll = %v, want none", dirs)
	}
	if got := pool.liveChildren(); got != 0 {
		t.Fatalf("live children after closeAll = %d, want 0", got)
	}
}

// A3 — a tenant that stops querying stops holding a copy of its events.
func TestSandboxIdleChildIsEvicted(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.idleTTL = 60 * time.Millisecond
		l.maxProjects = 4
	})

	if _, err := pool.query(ctx, uuid.NewString(), `SELECT count(*) AS n FROM events`, nil); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := pool.liveChildren(); got != 1 {
		t.Fatalf("live children = %d, want 1", got)
	}

	// Eviction is asynchronous: the janitor removes the entry and then closes
	// the process, so wait for both halves.
	deadline := time.Now().Add(5 * time.Second)
	for (pool.liveChildren() != 0 || pool.reaped.Load() != 1) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := pool.liveChildren(); got != 0 {
		t.Fatalf("live children after the idle TTL = %d, want 0", got)
	}
	if got := pool.reaped.Load(); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
}

// A4 — result materialization is bounded by bytes as well as rows, and a
// refusal costs the tenant nothing.
func TestSandboxResultBytesCapped(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.maxResultBytes = 4096
		l.maxRows = 3
	})

	_, err := pool.query(ctx, uuid.NewString(), `SELECT * FROM range(10)`, nil)
	if !errors.Is(err, ErrSandboxRows) {
		t.Fatalf("row-cap error = %v, want ErrSandboxRows", err)
	}
	if !strings.Contains(err.Error(), "LIMIT") {
		t.Fatalf("row-cap message = %q, want it to tell the author what to do", err.Error())
	}

	_, err = pool.query(ctx, uuid.NewString(), `SELECT repeat('x', 4096) AS s FROM range(2)`, nil)
	if !errors.Is(err, ErrSandboxBytes) {
		t.Fatalf("byte-cap error = %v, want ErrSandboxBytes", err)
	}

	// Both refusals were answered by a healthy child: the tenant keeps it.
	if got := pool.spawns.Load(); got != 2 {
		t.Fatalf("spawns = %d, want 2 (one per project; a refused query is not a dead child)", got)
	}
	if got := pool.liveChildren(); got != 2 {
		t.Fatalf("live children = %d, want 2", got)
	}
}

// A5 — one deadline covers admission, cold start, refresh and execution.
func TestSandboxRequestDeadlineCoversAllPhases(t *testing.T) {
	d := openTestDuckDB(t)

	// Cold project: the spawn and the copy must not ride an unbounded context.
	// The budget is deliberately below a process spawn — opening the engine
	// alone measured ~200ms — so the assertion cannot depend on how loaded the
	// machine is: with a cold project there is no way to answer inside 25ms.
	pool, ctx := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.requestTimeout = 25 * time.Millisecond
	})
	start := time.Now()
	_, err := pool.query(ctx, uuid.NewString(), `SELECT count(*) AS n FROM events`, nil)
	if !errors.Is(err, ErrSandboxTimeout) {
		t.Fatalf("cold-start deadline error = %v, want ErrSandboxTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cold-start deadline took %s, want ~the 150ms budget", elapsed)
	}
	if got := pool.liveChildren(); got != 0 {
		t.Fatalf("live children after a timed-out start = %d, want 0", got)
	}

	// Queued admission: a caller that cannot get a slot times out instead of
	// waiting behind it forever. The slot is taken directly so the wait is the
	// only thing under test — a query holding it would race its own budget.
	queued, ctx2 := newTestSandboxPool(t, d, func(l *sandboxLimits) {
		l.maxConcurrent = 1
		l.requestTimeout = 150 * time.Millisecond
	})
	queued.sem <- struct{}{}
	start = time.Now()
	_, err = queued.query(ctx2, uuid.NewString(), `SELECT count(*) AS n FROM events`, nil)
	<-queued.sem
	if !errors.Is(err, ErrSandboxTimeout) {
		t.Fatalf("queued-admission error = %v, want ErrSandboxTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("queued admission took %s, want ~the 150ms budget", elapsed)
	}
	if got := queued.spawns.Load(); got != 0 {
		t.Fatalf("spawns = %d, want 0: a caller that never got a slot must not create an engine", got)
	}
}

// repoPath resolves a repo-relative file from this test file's own location, so
// the assertion holds wherever the checkout happens to be mounted.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	for dir := filepath.Dir(file); ; {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("cannot locate %s from %s", rel, file)
		}
		dir = parent
	}
}

// A6 — the envelope is the one that is declared, and it fits the container.
func TestSandboxBudgetFitsContainer(t *testing.T) {
	const (
		// The reserve left for the Go runtime's non-heap memory, the driver's
		// cgo allocations and the page cache: the sum below must leave it.
		reserve = 96 << 20
		// A child's own runtime plus protocol buffers, measured at ~29 MB.
		childOverhead = 32 << 20
	)

	for _, env := range []string{"dev", "prod"} {
		path := repoPath(t, filepath.Join("infra", "gce", env, "docker-compose.yml"))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		memLimit, gomemlimit, err := parseAPIBudget(string(raw))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := memLimit / (1 << 20); got != 768 {
			t.Fatalf("%s: api mem_limit = %d MiB, want 768", path, got)
		}
		if got := gomemlimit / (1 << 20); got != 256 {
			t.Fatalf("%s: GOMEMLIMIT = %d MiB, want 256", path, got)
		}

		mainEngine := duckdbSizeBytes(t, sandboxMainMemoryLimit)
		childEngine := duckdbSizeBytes(t, sandboxMemoryLimit)
		total := gomemlimit + mainEngine + int64(sandboxMaxProjects)*(childEngine+childOverhead) + reserve
		if total > memLimit {
			t.Fatalf("%s: envelope %d MiB exceeds mem_limit %d MiB (GOMEMLIMIT + main engine %s + %d children of %s + %d MiB reserve)",
				path, total/(1<<20), memLimit/(1<<20), sandboxMainMemoryLimit, sandboxMaxProjects, sandboxMemoryLimit, reserve/(1<<20))
		}
		t.Logf("%s: %d MiB of %d MiB declared (GOMEMLIMIT + main %s + %d×%s + %d MiB reserve)",
			path, total/(1<<20), memLimit/(1<<20), sandboxMainMemoryLimit, sandboxMaxProjects, sandboxMemoryLimit, reserve/(1<<20))
	}

	// The budget that is claimed is the budget that is set: both engines report
	// the constant they were given.
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, nil)
	projectID := uuid.NewString()
	if _, err := pool.query(ctx, projectID, `SELECT count(*) AS n FROM events`, nil); err != nil {
		t.Fatalf("query: %v", err)
	}
	pool.mu.Lock()
	sb := pool.sandboxes[projectID]
	pool.mu.Unlock()
	if sb == nil {
		t.Fatal("no sandbox for the queried project")
	}
	assertEngineLimit(t, "sandbox child", sb.engineMemoryLimit, sandboxMemoryLimit, 1<<20)

	var mainReported string
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "SELECT current_setting('memory_limit')").Scan(&mainReported)
	}); err != nil {
		t.Fatalf("main instance memory_limit: %v", err)
	}
	assertEngineLimit(t, "main instance", mainReported, sandboxMainMemoryLimit, 1<<20)
}

// A8 — the trusted path returns what it returned in-process: same values, same
// Go types, through the codec.
func TestSandboxRowValuesSurviveTheChild(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := d.InsertEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "user-a", at)}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	s := &Store{duck: d, sandboxes: newSQLSandboxPool(d)}
	t.Cleanup(s.sandboxes.closeAll)

	rows, err := s.RunSQL(ctx, projectID,
		`SELECT event_name, count(*) AS n, min("timestamp") AS at, max(cost_usd) AS cost FROM events GROUP BY event_name`)
	if err != nil {
		t.Fatalf("RunSQL: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1", rows)
	}
	if got, ok := rows[0]["event_name"].(string); !ok || got != "user.signed_up" {
		t.Fatalf("event_name = %#v, want the string \"user.signed_up\"", rows[0]["event_name"])
	}
	if got, ok := rows[0]["n"].(int64); !ok || got != 1 {
		t.Fatalf("count = %#v (%T), want int64(1)", rows[0]["n"], rows[0]["n"])
	}
	stamp, ok := rows[0]["at"].(time.Time)
	if !ok {
		t.Fatalf("timestamp = %#v (%T), want a time.Time", rows[0]["at"], rows[0]["at"])
	}
	if !stamp.UTC().Equal(at) {
		t.Fatalf("timestamp = %s, want %s", stamp.UTC(), at)
	}
	if rows[0]["cost"] != nil {
		t.Fatalf("NULL cost = %#v, want nil", rows[0]["cost"])
	}
}

// A9 — the kinds of answer the UI sees are unchanged, and a dead child is
// reported as unavailable rather than as bad SQL.
func TestSandboxErrorSurfacesUnchanged(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, nil)
	projectID := uuid.NewString()
	seedProject(t, d, projectID, 1)

	// The engine's own answer stays a SQL error.
	if _, err := pool.query(ctx, projectID, `SELECT * FROM nope.this_table`, nil); err == nil {
		t.Fatal("a bad query was answered")
	} else {
		var se *SandboxError
		if !errors.As(err, &se) || se.Kind != SandboxKindSQL {
			t.Fatalf("bad-query error = %v (kind %v), want a SQL error", err, se)
		}
		if IsSandboxLimit(err) || IsSandboxUnavailable(err) {
			t.Fatalf("bad-query error = %v, want it not to classify as a sandbox failure", err)
		}
	}

	// Empty is empty, not an error.
	rows, err := pool.query(ctx, projectID, `SELECT * FROM range(5) WHERE range > 100`, nil)
	if err != nil {
		t.Fatalf("empty result: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty result rows = %v, want none", rows)
	}
	if rows == nil {
		// gob carries no nil/empty distinction; the JSON surface does, and a
		// zero-row answer is `rows: []` today, not `rows: null`.
		t.Fatal("empty result is a nil slice: the API would render rows:null where it rendered rows:[]")
	}

	// A child that died is the API's problem to retry, never the author's SQL.
	pool.mu.Lock()
	sb := pool.sandboxes[projectID]
	pool.mu.Unlock()
	if sb == nil {
		t.Fatal("no sandbox for the queried project")
	}
	// Break the child deterministically — its stdin closes and it is killed,
	// which is exactly the state a crashed or OOM-killed child leaves behind —
	// then require the pool to report it, reap it and evict it.
	if err := sb.stdin.Close(); err != nil {
		t.Fatalf("close child stdin: %v", err)
	}
	if err := sb.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_, err = pool.query(ctx, projectID, `SELECT count(*) AS n FROM events`, nil)
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("dead-child error = %v, want ErrSandboxUnavailable", err)
	}
	if IsSandboxLimit(err) {
		t.Fatalf("dead-child error = %v, want it not to look like the author's query was too big", err)
	}
	liveDeadline := time.Now().Add(5 * time.Second)
	for (pool.liveChildren() != 0 || pool.reaped.Load() != 1) && time.Now().Before(liveDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := pool.liveChildren(); got != 0 {
		t.Fatalf("live children after a dead child = %d, want 0 (kill + reap + evict)", got)
	}
	if got := pool.reaped.Load(); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}

	// And the next caller is served by a fresh child rather than inheriting the
	// failure: an evicted sandbox must be recoverable.
	rows, err = pool.query(ctx, projectID, `SELECT count(*) AS n FROM events`, nil)
	if err != nil {
		t.Fatalf("query after eviction: %v", err)
	}
	if got := rows[0]["n"]; got != int64(1) {
		t.Fatalf("query after eviction returned %v events, want 1", got)
	}
}

// The child cannot outlive the parent: its only input is the parent's pipe, so
// a crashed API closes it and the child sees EOF. That is the portable
// substitute for Pdeathsig, which Go delivery cannot guarantee (the signal
// fires when the *thread* that forked exits, and the runtime retires threads
// under a live process).
func TestSandboxChildExitsWhenParentPipeCloses(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	pool, _ := newTestSandboxPool(t, d, nil)

	sb, err := spawnSQLSandbox(ctx, pool, uuid.NewString())
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(sb.close)

	// Exactly what a crashed parent leaves behind. The child exiting closes its
	// end of stdout, which is the portable liveness signal: a reaped-but-not-yet
	// -waited child would still answer kill(pid, 0) as a zombie.
	if err := sb.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	exited := make(chan error, 1)
	go func() {
		_, err := sb.stdout.ReadByte()
		exited <- err
	}()
	select {
	case err := <-exited:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("child stdout ended with %v, want EOF (the child exited)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("sandbox child %d outlived its parent's pipe", sb.cmd.Process.Pid)
	}
}

// A child is started without the API's environment: a process that executes
// untrusted SQL must not be able to read a credential out of its own env.
func TestSandboxChildInheritsNoSecrets(t *testing.T) {
	env := sandboxChildEnv()
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH", "TMPDIR":
		default:
			t.Fatalf("sandbox child env carries %q; only PATH and TMPDIR are allowed", key)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// parseAPIBudget reads the api service's mem_limit and GOMEMLIMIT out of a
// compose file. It scans the api anchor rather than the file, so the sibling
// web service's limit in the same file cannot be mistaken for it.
func parseAPIBudget(raw string) (memLimit, gomemlimit int64, err error) {
	lines := strings.Split(raw, "\n")
	anchor := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "x-agentray-api:") {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return 0, 0, errors.New("no x-agentray-api anchor")
	}
	for _, line := range lines[anchor:] {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "mem_limit:"):
			memLimit, err = dockerSizeBytes(strings.TrimSpace(strings.TrimPrefix(trimmed, "mem_limit:")))
			if err != nil {
				return 0, 0, err
			}
		case strings.HasPrefix(trimmed, "GOMEMLIMIT:"):
			gomemlimit, err = dockerSizeBytes(strings.TrimSpace(strings.TrimPrefix(trimmed, "GOMEMLIMIT:")))
			if err != nil {
				return 0, 0, err
			}
		}
		if memLimit > 0 && gomemlimit > 0 {
			return memLimit, gomemlimit, nil
		}
	}
	return 0, 0, errors.New("api mem_limit/GOMEMLIMIT not both declared")
}

// dockerSizeBytes parses the compose/Dockerfile size form: "768m" is MiB,
// "256MiB" is MiB.
func dockerSizeBytes(s string) (int64, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(lower, "mib"):
		lower, mult = strings.TrimSuffix(lower, "mib"), 1<<20
	case strings.HasSuffix(lower, "gib"):
		lower, mult = strings.TrimSuffix(lower, "gib"), 1<<30
	case strings.HasSuffix(lower, "m"):
		lower, mult = strings.TrimSuffix(lower, "m"), 1<<20
	case strings.HasSuffix(lower, "g"):
		lower, mult = strings.TrimSuffix(lower, "g"), 1<<30
	}
	n, err := strconv.ParseInt(strings.TrimSpace(lower), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return n * mult, nil
}

// duckdbSizeBytes parses a duckdb SET value ("96MB") into bytes. DuckDB reads
// MB as decimal megabytes (its own report of 128MB is "122.0 MiB").
func duckdbSizeBytes(t *testing.T, s string) int64 {
	t.Helper()
	lower := strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(lower, "mb"):
		lower, mult = strings.TrimSuffix(lower, "mb"), 1_000_000
	case strings.HasSuffix(lower, "gb"):
		lower, mult = strings.TrimSuffix(lower, "gb"), 1_000_000_000
	case strings.HasSuffix(lower, "kb"):
		lower, mult = strings.TrimSuffix(lower, "kb"), 1_000
	}
	n, err := strconv.ParseInt(strings.TrimSpace(lower), 10, 64)
	if err != nil {
		t.Fatalf("engine size %q: %v", s, err)
	}
	return n * mult
}

// assertEngineLimit checks the value duckdb reports back equals the constant
// that was set, within one MiB of rounding.
func assertEngineLimit(t *testing.T, who, reported, want string, tolerance int64) {
	t.Helper()
	var mib float64
	if _, err := fmt.Sscanf(reported, "%f MiB", &mib); err != nil {
		t.Fatalf("%s reported memory_limit %q, which is not a MiB figure: %v", who, reported, err)
	}
	got := int64(mib * float64(1<<20))
	expected := duckdbSizeBytes(t, want)
	if diff := got - expected; diff > tolerance || diff < -tolerance {
		t.Fatalf("%s reports memory_limit %q (%d bytes), want %s (%d bytes)", who, reported, got, want, expected)
	}
}
