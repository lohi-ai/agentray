package storage

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// duckdb_sandbox.go — the advanced-SQL execution environment.
//
// Untrusted SQL (run_sql, saved queries, custom charts, alert rules) never runs
// against the shared analytics file, and — since the measurements in
// `evidence/2026-09-14-isolation-boundary-probe.md` — never runs in the API
// process either. Each project gets its own *child process* holding an
// in-memory DuckDB instance with only that project's events, aliases and
// external rows; the parent is the only side that opens the analytics file and
// streams rows down the pipe.
//
// Why a process and not a guard: duckdb's `memory_limit` does not bound scalar
// length-minting functions (`SELECT repeat('x', 5e8)` handed Go a 500 MB cell
// under a 122 MiB limit), and an engine-side projection guard still OOM-killed
// a 192 MiB container. Nothing in-process can be a hard bound, so the bound is
// the kernel's: the child caps its own address space (Linux; see
// duckdb_sandbox_spawn_linux.go), prefers itself to the API in the kernel's OOM
// election, and is killed and reaped by the parent when it stops answering.
//
//   - The child instance is opened with enable_external_access=false: it cannot
//     read a file, open a network connection, ATTACH, or INSTALL/LOAD an
//     extension, and lock_configuration=true means the query cannot undo any of
//     that. It receives values over the pipe and nothing else — and it inherits
//     none of the API's environment, so no credential crosses the boundary.
//   - Admission is taken before any per-tenant engine exists: a burst across
//     more projects than the cap cannot spawn more children than the cap, and a
//     query that never gets a slot never costs a process.
//   - One deadline covers admission, refresh and execution. A query that
//     outlives it is interrupted with SIGINT (the driver maps cancellation to
//     DuckDB's interrupt), and killed after a grace period.
//   - Results are capped by rows *and* bytes in the child, before a frame is
//     written, because the engine's memory limit does not cover the Go maps
//     either side builds.
//
// Because the sandbox holds only the project's rows, a catalog escape
// (main.events, duckdb_tables(), a qualified name) can only ever see the
// project's own data — the rewrite in scopedReadonlySQL is a convenience
// layer (canonical_id, soft deletes), not the tenant boundary. The boundary
// is physical, twice over: process and data.
//
// Children are lazily created (single-flight per project), refreshed
// incrementally on each use, leased while a query is in flight so eviction
// cannot close one mid-read, and evicted by an LRU bound *and* an idle
// lifetime so tenant count cannot multiply memory without limit.

const (
	// sandboxMaxProjects bounds live sandbox children; the least-recently-used
	// one is evicted (killed) when a new project needs a slot. It is small on
	// purpose: every live child holds a full in-memory copy of its project's
	// rows, so this is the term that decides whether tenant count can multiply
	// memory.
	sandboxMaxProjects = 2
	// sandboxMemoryLimit caps each child instance's engine memory.
	sandboxMemoryLimit = "96MB"
	// sandboxThreads caps each child instance's worker threads.
	sandboxThreads = 1
	// sandboxMaxConcurrent bounds queries executing across ALL children —
	// per-instance limits alone would let N projects exhaust the container.
	sandboxMaxConcurrent = 2
	// sandboxQueryTimeout bounds one untrusted query when the caller supplies no
	// deadline of its own.
	sandboxQueryTimeout = 30 * time.Second
	// sandboxRequestTimeout bounds the WHOLE path — admission, cold start,
	// refresh and execution — so a query can never ride an unbounded request
	// context through the slow phases. Zero disables the extra deadline (the
	// child's own query timeout still applies).
	sandboxRequestTimeout = 60 * time.Second
	// sandboxIdleTTL evicts a child that has not been used for this long, so a
	// tenant that stops querying stops holding a copy of its events.
	sandboxIdleTTL = 5 * time.Minute
	// sandboxCopyBatch is the row count per insert frame during refresh.
	sandboxCopyBatch = 2048
	// sandboxInsertBatchBytes is the size at which a half-built insert frame is
	// sent. sandboxCopyBatch alone bounds nothing when rows are fat: 2,048 rows
	// of tool output is not a batch, it is the API's heap.
	sandboxInsertBatchBytes = 4 << 20
	// sandboxMaxRowBytes refuses a single row no frame could carry. It sits
	// above the batch threshold and well below sandboxFrameMaxBytes, so a full
	// batch (a threshold's worth plus one row) always fits a frame.
	sandboxMaxRowBytes = 16 << 20
	// sandboxMaxRows caps materialized result rows.
	sandboxMaxRows = 10_000
	// sandboxMaxResultBytes caps materialized result bytes. Row count alone is
	// not a bound: `SELECT properties FROM events` over fat JSON is 10k rows and
	// can be gigabytes.
	sandboxMaxResultBytes = 8 << 20
	// sandboxTempSize caps one sandbox's spill directory.
	sandboxTempSize = "256MB"
	// sandboxRlimitBudget is the address space a child may allocate above its
	// own virtual size at engine open (Linux). It is the per-child allowance the
	// container arithmetic spends: DuckDB's own memory_limit does not bound a
	// scalar, so the kernel bound — not the engine one — is what a child can
	// actually commit. TestSandboxBudgetFitsContainer sums this term.
	//
	// It is measured, not chosen: at 96 MiB the child dies before it answers
	// anything (the engine's own virtual mappings for a 96 MB memory_limit plus
	// its spill file exceed the budget — "sandbox did not start: EOF" at
	// warm-up, and a sibling child's schema creation fails with a DuckDB bad
	// allocation), while 192 MiB runs the whole hostile set with the containment
	// intact: the hostile query fails under a cap and the trusted writer keeps
	// committing.
	sandboxRlimitBudget = 192 << 20
	// sandboxKillGrace is how long a child has to honour SIGINT/close before it
	// is killed outright.
	sandboxKillGrace = 2 * time.Second
	// sandboxMainMemoryLimit caps the trusted instance's engine memory. The
	// container is the real bound; this keeps DuckDB from treating the whole
	// cgroup as its budget.
	sandboxMainMemoryLimit = "128MB"
	// sandboxMainTempSize caps the trusted instance's spill.
	sandboxMainTempSize = "2GB"
)

// sandboxLimits is the whole budget in one place: one struct, one set of
// defaults, and an injection point for tests that need a smaller envelope than
// production ships.
type sandboxLimits struct {
	maxProjects    int
	maxConcurrent  int
	memoryLimit    string
	tempSize       string
	threads        int
	maxRows        int
	maxResultBytes int64
	queryTimeout   time.Duration
	requestTimeout time.Duration
	idleTTL        time.Duration
	rlimitBudget   int64
	killGrace      time.Duration
}

func defaultSandboxLimits() sandboxLimits {
	return sandboxLimits{
		maxProjects:    sandboxMaxProjects,
		maxConcurrent:  sandboxMaxConcurrent,
		memoryLimit:    sandboxMemoryLimit,
		tempSize:       sandboxTempSize,
		threads:        sandboxThreads,
		maxRows:        sandboxMaxRows,
		maxResultBytes: sandboxMaxResultBytes,
		queryTimeout:   sandboxQueryTimeout,
		requestTimeout: sandboxRequestTimeout,
		idleTTL:        sandboxIdleTTL,
		rlimitBudget:   sandboxRlimitBudget,
		killGrace:      sandboxKillGrace,
	}
}

// sandboxWorkerArgs renders the limits the child is started with, so the budget
// has exactly one author.
func (l sandboxLimits) sandboxWorkerArgs(tmpDir string) []string {
	return []string{
		SandboxWorkerArgv,
		"--tmp-dir=" + tmpDir,
		"--memory-limit=" + l.memoryLimit,
		"--temp-size=" + l.tempSize,
		fmt.Sprintf("--threads=%d", l.threads),
		fmt.Sprintf("--max-rows=%d", l.maxRows),
		fmt.Sprintf("--max-result-bytes=%d", l.maxResultBytes),
		"--query-timeout=" + l.queryTimeout.String(),
		fmt.Sprintf("--rlimit-bytes=%d", l.rlimitBudget),
	}
}

// sqlSandboxPool owns the per-project sandbox children for one Store.
type sqlSandboxPool struct {
	main   *DuckDB
	limits sandboxLimits

	// sem bounds concurrent untrusted queries across all children. It is taken
	// BEFORE a child is spawned or refreshed: admission is the gate on how many
	// engines can exist at once, not a gate on how many may run.
	sem chan struct{}

	mu sync.Mutex
	// lru is most-recently-used first.
	lru       []string
	sandboxes map[string]*sqlSandbox
	// opening single-flights a cold spawn per project so a burst of queries
	// for one new project doesn't start N children and discard N-1.
	opening map[string]*sandboxOpen

	// spawns counts children started; inFlight/peakInFlight and reaped expose
	// the admission and reaping invariants to tests without reaching into the
	// pool's internals.
	spawns       atomic.Int64
	inFlight     atomic.Int64
	peakInFlight atomic.Int64
	reaped       atomic.Int64

	// done stops the idle janitor.
	done      chan struct{}
	closeOnce sync.Once
}

type sandboxOpen struct {
	done chan struct{}
	err  error
}

func newSQLSandboxPool(main *DuckDB) *sqlSandboxPool {
	return newSQLSandboxPoolWithLimits(main, defaultSandboxLimits())
}

func newSQLSandboxPoolWithLimits(main *DuckDB, limits sandboxLimits) *sqlSandboxPool {
	p := &sqlSandboxPool{
		main:      main,
		limits:    limits,
		sem:       make(chan struct{}, limits.maxConcurrent),
		sandboxes: map[string]*sqlSandbox{},
		opening:   map[string]*sandboxOpen{},
		done:      make(chan struct{}),
	}
	if limits.idleTTL > 0 {
		go p.reapIdleLoop()
	}
	return p
}

// closeAll kills every child and stops the janitor. Called from CloseDuckDB
// before the main engine closes.
func (p *sqlSandboxPool) closeAll() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() { close(p.done) })
	p.mu.Lock()
	sandboxes := p.sandboxes
	p.sandboxes = map[string]*sqlSandbox{}
	p.lru = nil
	p.mu.Unlock()
	// Each close may wait out the kill grace, and the waits are independent:
	// reaping them one at a time would bound shutdown by N graces instead of
	// one.
	var wg sync.WaitGroup
	for _, sb := range sandboxes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb.close()
		}()
	}
	wg.Wait()
}

// query runs sqlText (already rewritten by scopedReadonlySQL) inside the
// project's sandbox and returns the rows.
//
// One deadline covers the whole path. Admission comes first, so a caller that
// never gets a slot never creates an engine; a child is spawned only by a
// caller that already holds one.
func (p *sqlSandboxPool) query(ctx context.Context, projectID, query string, args []any) ([]map[string]any, error) {
	rctx := ctx
	if p.limits.requestTimeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, p.limits.requestTimeout)
		defer cancel()
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-rctx.Done():
		// Waiting behind other tenants is not the author's SQL being wrong: it
		// is capacity, so it is retryable (503), not a limit refusal (400).
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
			"the analytics sandbox is busy; retry shortly")
	}

	// Admission is now accounted: everything below — spawning, refreshing,
	// executing — rides this one slot.
	if n := p.inFlight.Add(1); n > p.peakInFlight.Load() {
		p.peakInFlight.CompareAndSwap(p.peakInFlight.Load(), n)
	}
	defer p.inFlight.Add(-1)

	sb, err := p.sandboxFor(rctx, projectID)
	if err != nil {
		return nil, err
	}
	// sandboxFor returns the sandbox already leased, so eviction cannot close it
	// between lookup and this query starting.
	defer sb.release()

	if err := sb.refresh(rctx); err != nil {
		p.drop(sb)
		// A refresh reads the TRUSTED store, so its failures are the sandbox's,
		// not the author's: without this wrap a driver or deadline error from
		// the copy reaches the client as "bad SQL" (400).
		return nil, sandboxRefreshError(err)
	}
	rows, err := sb.run(rctx, query, args)
	if err != nil && sb.dead.Load() {
		// The child stopped answering: evict and reap it rather than leaving a
		// wedged process holding the project's slot.
		p.drop(sb)
	}
	return rows, err
}

// sandboxRefreshError classifies a failure from the copy phase. Anything the
// sandbox already typed keeps its kind; a raw driver/context error from the
// trusted store becomes an unavailable sandbox, because the author's SQL never
// ran.
func sandboxRefreshError(err error) error {
	var se *SandboxError
	if errors.As(err, &se) {
		return err
	}
	return sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
		fmt.Sprintf("analytics sandbox could not copy this project's rows: %v", err))
}

// drop removes a broken sandbox from the pool and reaps it. Callers must not
// hold the sandbox mutex or the pool lock.
func (p *sqlSandboxPool) drop(sb *sqlSandbox) {
	p.mu.Lock()
	if cur, ok := p.sandboxes[sb.projectID]; ok && cur == sb {
		delete(p.sandboxes, sb.projectID)
		for i, id := range p.lru {
			if id == sb.projectID {
				p.lru = append(p.lru[:i], p.lru[i+1:]...)
				break
			}
		}
	}
	p.mu.Unlock()
	sb.close()
}

// sandboxFor returns the project's sandbox, spawning it on first use, and
// evicts LRU entries past the cap. Spawning is single-flighted per project and
// happens outside the pool lock so a cold start never blocks other projects.
func (p *sqlSandboxPool) sandboxFor(ctx context.Context, projectID string) (*sqlSandbox, error) {
	for {
		p.mu.Lock()
		if sb, ok := p.sandboxes[projectID]; ok {
			sb.lease()
			p.touchLocked(projectID)
			p.mu.Unlock()
			return sb, nil
		}
		if op, ok := p.opening[projectID]; ok {
			p.mu.Unlock()
			select {
			case <-op.done:
				if op.err != nil {
					return nil, op.err
				}
				// Re-enter through the pool lock: the opener's lease may have
				// ended and allowed eviction before this waiter woke.
				continue
			case <-ctx.Done():
				// Waiting for another caller's cold start is capacity, not the
				// author's SQL: retryable, like the admission wait above.
				return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
					"timed out waiting for the analytics sandbox to start; retry shortly")
			}
		}
		op := &sandboxOpen{done: make(chan struct{})}
		p.opening[projectID] = op
		// Make room BEFORE spawning. A child holds its engine and spill the
		// moment it opens, so counting only the entries already in the map would
		// let two concurrent cold queries run alongside the two warm children
		// they are about to replace — four children against a two-child budget.
		// The opener counts itself through p.opening.
		victims := p.evictLocked()
		p.mu.Unlock()
		for _, victim := range victims {
			victim.close()
		}

		sb, err := spawnSQLSandbox(ctx, p, projectID)
		p.spawns.Add(1)
		if err == nil {
			// The child is published only once it holds the project's rows. A
			// waiter that leased an empty child would otherwise race this
			// refresh, and a waiter that gives up while waiting on the child's
			// mutex must not be able to kill a child another caller is using.
			if rerr := sb.refresh(ctx); rerr != nil {
				sb.close()
				err = sandboxRefreshError(rerr)
			}
		}

		p.mu.Lock()
		delete(p.opening, projectID)
		if err == nil {
			sb.lease()
			p.sandboxes[projectID] = sb
			p.lru = append([]string{projectID}, p.lru...)
		}
		op.err = err
		close(op.done)
		p.mu.Unlock()
		return sb, err
	}
}

// evictLocked selects LRU sandboxes past the cap and removes them from the
// pool. A sandbox with an active lease (refs > 0) is skipped — its releaser
// re-checks the cap. Victims are returned rather than closed here: killing a
// process takes time, and the pool lock must not be held for it. Callers hold
// p.mu.
func (p *sqlSandboxPool) evictLocked() []*sqlSandbox {
	var victims []*sqlSandbox
	// In-flight opens hold memory too, so they count against the cap: without
	// them a burst of cold queries overshoots maxProjects by its own width. Only
	// entries in the map can be evicted — two opens racing for one slot leave
	// the second over the cap for as long as neither has registered, which is
	// what maxConcurrent, not this loop, bounds.
	for len(p.lru) > 0 && len(p.lru)+len(p.opening) > p.limits.maxProjects {
		victim := p.lru[len(p.lru)-1]
		sb, ok := p.sandboxes[victim]
		if !ok {
			p.lru = p.lru[:len(p.lru)-1]
			continue
		}
		if sb.refs.Load() > 0 {
			// In flight — try the next-oldest instead.
			if len(p.lru) == 1 {
				break
			}
			// Rotate it to the front so it isn't picked again this pass.
			p.lru = append([]string{victim}, p.lru[:len(p.lru)-1]...)
			// If every sandbox is leased, stop.
			allLeased := true
			for _, id := range p.lru {
				if s, ok := p.sandboxes[id]; ok && s.refs.Load() == 0 {
					allLeased = false
					break
				}
			}
			if allLeased {
				break
			}
			continue
		}
		p.lru = p.lru[:len(p.lru)-1]
		delete(p.sandboxes, victim)
		victims = append(victims, sb)
	}
	return victims
}

func (p *sqlSandboxPool) touchLocked(projectID string) {
	for i, id := range p.lru {
		if id == projectID {
			copy(p.lru[1:i+1], p.lru[:i])
			p.lru[0] = projectID
			return
		}
	}
	p.lru = append([]string{projectID}, p.lru...)
}

// reapIdleLoop evicts children that have gone unused, so a tenant that stops
// querying stops holding a copy of its events.
func (p *sqlSandboxPool) reapIdleLoop() {
	tick := p.limits.idleTTL / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			for _, sb := range p.reapIdle() {
				sb.close()
			}
		}
	}
}

func (p *sqlSandboxPool) reapIdle() []*sqlSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	var victims []*sqlSandbox
	for id, sb := range p.sandboxes {
		if sb.refs.Load() > 0 || time.Since(sb.lastUsed()) < p.limits.idleTTL {
			continue
		}
		delete(p.sandboxes, id)
		for i, entry := range p.lru {
			if entry == id {
				p.lru = append(p.lru[:i], p.lru[i+1:]...)
				break
			}
		}
		victims = append(victims, sb)
	}
	return victims
}

// liveChildren reports how many sandbox processes this pool currently holds.
func (p *sqlSandboxPool) liveChildren() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sandboxes)
}

// sqlSandbox is one project's isolated in-memory DuckDB, as a child process.
type sqlSandbox struct {
	projectID string
	pool      *sqlSandboxPool
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	// tmpDir is this child's private spill directory.
	tmpDir string
	// engineMemoryLimit and rlimitBytes are what the child reported it applied,
	// so a budget that is claimed can be compared with the budget that is set.
	engineMemoryLimit string
	rlimitBytes       uint64

	// refs counts in-flight refresh/run pairs; eviction skips leased sandboxes
	// so a query can never observe a dead child.
	refs atomic.Int64
	// dead marks a child known to be broken (transport failure or a deadline it
	// did not honour); the owner evicts and reaps it.
	dead   atomic.Bool
	usedAt atomic.Int64
	mu     sync.Mutex
	closed bool
}

func (sb *sqlSandbox) lease() { sb.refs.Add(1) }

// lockCtx takes the sandbox mutex, giving up when ctx ends. Waiting for another
// caller's query is not a reason to touch the child: `call` marks a child dead
// on an expired context, so a waiter that simply gave up would kill a child
// that is serving someone else.
func (sb *sqlSandbox) lockCtx(ctx context.Context) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if sb.mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
				"timed out waiting for the analytics sandbox; retry shortly")
		case <-tick.C:
		}
	}
}

func (sb *sqlSandbox) release() {
	sb.usedAt.Store(time.Now().UnixNano())
	if sb.refs.Add(-1) != 0 {
		return
	}
	// evictLocked/eviction may have deferred enforcement while every sandbox was
	// leased. Re-check as soon as one becomes idle so the pool returns to its
	// configured bound without waiting for another cold project.
	pool := sb.pool
	pool.mu.Lock()
	victims := pool.evictLocked()
	pool.mu.Unlock()
	for _, victim := range victims {
		victim.close()
	}
}

func (sb *sqlSandbox) lastUsed() time.Time {
	at := sb.usedAt.Load()
	if at == 0 {
		return time.Now()
	}
	return time.Unix(0, at)
}

// spawnSQLSandbox starts the child and completes its handshake. The child is
// the same binary, so there is no second artifact to build, ship or pin.
func spawnSQLSandbox(ctx context.Context, pool *sqlSandboxPool, projectID string) (*sqlSandbox, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
			fmt.Sprintf("analytics sandbox unavailable: %v", err))
	}
	// Each child gets its own spill dir — DuckDB names temp files
	// duckdb_temp_storage-* and two instances sharing one directory collide.
	tmpDir := ""
	if base := pool.main.tmpDir(); base != "" {
		// One directory per child, not per project: drop() removes the
		// directory after reaping, and a replacement for the same project can
		// already be running by then — sharing the path would delete a live
		// child's spill out from under it. MkdirTemp, not a process-local
		// counter: after an unclean exit the counter restarts while the old
		// directory is still on disk, and MkdirAll would silently hand a new
		// child the dead one's spill.
		dir, err := os.MkdirTemp(base, "sandbox-"+projectID+"-")
		if err != nil {
			return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
				fmt.Sprintf("analytics sandbox tmp dir: %v", err))
		}
		// The child runs with its working directory in os.TempDir(), so a
		// relative spill path would resolve somewhere else entirely.
		tmpDir, err = filepath.Abs(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
				fmt.Sprintf("analytics sandbox tmp dir: %v", err))
		}
	}

	cmd := exec.Command(exe, pool.limits.sandboxWorkerArgs(tmpDir)...)
	// The child runs untrusted SQL: it gets no part of the API's environment —
	// no database URL, no encryption secret, nothing to leak on a catalog
	// escape — and no working directory containing the repo.
	cmd.Env = sandboxChildEnv()
	cmd.Dir = os.TempDir()
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable, err.Error())
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable, err.Error())
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
			fmt.Sprintf("start analytics sandbox: %v", err))
	}

	sb := &sqlSandbox{
		projectID: projectID,
		pool:      pool,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewReaderSize(stdout, 1<<20),
		tmpDir:    tmpDir,
	}
	sb.usedAt.Store(time.Now().UnixNano())

	resp, err := sb.readFrame(ctx)
	if err != nil || resp.Kind != "ready" {
		sb.close()
		// A cold start cut short by the caller's budget is a timeout — the same
		// query may well work in a moment — not an unavailable sandbox.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
				"timed out starting the analytics sandbox; retry shortly")
		}
		if err == nil {
			err = fmt.Errorf("sandbox handshake failed: %s", resp.Kind)
		}
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable,
			fmt.Sprintf("analytics sandbox did not start: %v", err))
	}
	sb.engineMemoryLimit = resp.EngineMemoryLimit
	sb.rlimitBytes = resp.RlimitBytes
	return sb, nil
}

// sandboxChildEnv is the whole environment a sandbox child is given. The child
// needs a temp directory and nothing else; every secret the API holds stays out
// of a process that executes untrusted SQL.
func sandboxChildEnv() []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"TMPDIR=" + os.TempDir(),
	}
}

// refresh copies the project's rows from the main store into the child. Events
// are append-only, so the copy is incremental on (inserted_at, event_id);
// aliases and external_rows are small enough to reconcile wholesale. A row
// removed from the main file is removed here too — the count check keeps the
// common refresh cheap.
func (sb *sqlSandbox) refresh(ctx context.Context) error {
	if err := sb.lockCtx(ctx); err != nil {
		return err
	}
	defer sb.mu.Unlock()
	if sb.closed {
		return sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable, "analytics sandbox is closed")
	}
	if err := sb.refreshEvents(ctx); err != nil {
		return err
	}
	if err := sb.refreshTable(ctx, "aliases",
		`SELECT project_id, anonymous_id, canonical_id FROM aliases WHERE project_id = ?`, 3); err != nil {
		return err
	}
	if err := sb.refreshTable(ctx, "external_rows",
		`SELECT project_id, connector_id, table_name, row_key, cursor, data, synced_at FROM external_rows WHERE project_id = ?`, 7); err != nil {
		return err
	}
	return nil
}

// refreshEvents appends only what arrived since the last refresh. The cursor
// is (inserted_at, event_id) — a bare inserted_at high-water mark skips rows
// that share the timestamp of the last copied row. A count drift (a delete
// in the main file — not possible today) rebuilds the table.
func (sb *sqlSandbox) refreshEvents(ctx context.Context) error {
	cursor, err := sb.call(ctx, sandboxRequest{Op: "cursor"})
	if err != nil {
		return err
	}
	var mainCount int64
	if err := sb.pool.main.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx,
			`SELECT count(*) FROM events WHERE project_id = ?`, sb.projectID).Scan(&mainCount)
	}); err != nil {
		return err
	}
	highWater, highWaterID := cursor.Watermark, cursor.WatermarkID
	if cursor.Count > mainCount {
		// Rows vanished from the main file; rebuild rather than probe per row.
		if _, err := sb.call(ctx, sandboxRequest{Op: "delete", Table: "events"}); err != nil {
			return err
		}
		highWater = time.Time{}
		highWaterID = ""
	}
	return sb.copyRows(ctx,
		`SELECT * FROM events WHERE project_id = ?
		 AND (inserted_at > ? OR (inserted_at = ? AND event_id::VARCHAR > ?))
		 ORDER BY inserted_at, event_id`,
		[]any{sb.projectID, highWater, highWater, highWaterID},
		"events", 28)
}

// refreshTable reconciles a small table wholesale: delete-then-copy so a row
// removed upstream disappears here too. Both tables are small (aliases are
// one row per identify, external_rows one per synced source row).
func (sb *sqlSandbox) refreshTable(ctx context.Context, table, selectSQL string, nCols int) error {
	if _, err := sb.call(ctx, sandboxRequest{Op: "delete", Table: table}); err != nil {
		return err
	}
	return sb.copyRows(ctx, selectSQL, []any{sb.projectID}, table, nCols)
}

// copyRows streams rows out of the main store and sends them to the child in
// batches. The child never sees the file — only values.
func (sb *sqlSandbox) copyRows(ctx context.Context, selectSQL string, selectArgs []any, table string, nCols int) error {
	batch := make([][]any, 0, sandboxCopyBatch)
	batchBytes := 0
	err := sb.pool.main.Read(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, selectSQL, selectArgs...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		if len(cols) != nCols {
			return fmt.Errorf("column count mismatch: %d != %d", len(cols), nCols)
		}
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			if _, err := sb.call(ctx, sandboxRequest{Op: "insert", Table: table, Rows: batch}); err != nil {
				return err
			}
			batch = batch[:0]
			batchBytes = 0
			return nil
		}
		for rows.Next() {
			dest := make([]any, nCols)
			for i := range dest {
				dest[i] = new(any)
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			// Normalize in place: dest is this row's own scratch slice, so a
			// second same-sized slice per row would be pure copy on the cold
			// path that copies a tenant's whole event set.
			size := 0
			for i := range dest {
				dest[i] = gobSafeValue(*(dest[i].(*any)))
				size += frameValueBytes(dest[i])
			}
			if size > sandboxMaxRowBytes {
				return sandboxError(SandboxKindBytes, ErrSandboxBytes,
					fmt.Sprintf("one %s row is %d bytes, above the %d-byte sandbox row limit", table, size, sandboxMaxRowBytes))
			}
			// Flush before the row that would push the frame past what the child
			// will read, so the row itself only has to fit on its own.
			if batchBytes > 0 && batchBytes+size > sandboxFrameMaxBytes-sandboxMaxRowBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, dest)
			batchBytes += size
			// Bytes as well as rows: this batch is built in the API's own heap,
			// so a tenant whose rows carry fat JSON could otherwise make the
			// trusted process — not the sandbox — the one that dies.
			if len(batch) == sandboxCopyBatch || batchBytes >= sandboxInsertBatchBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return flush()
	})
	batch = nil
	return err
}

// run executes one already-validated, already-rewritten SELECT in the child
// under the caller's deadline and returns the rows it framed back.
func (sb *sqlSandbox) run(ctx context.Context, query string, args []any) ([]map[string]any, error) {
	if err := sb.lockCtx(ctx); err != nil {
		return nil, err
	}
	defer sb.mu.Unlock()
	if sb.closed {
		return nil, sandboxError(SandboxKindUnavailable, ErrSandboxUnavailable, "analytics sandbox is closed")
	}
	deadline := int64(0)
	if d, ok := ctx.Deadline(); ok {
		deadline = d.UnixNano()
	}
	safeArgs := make([]any, len(args))
	for i, arg := range args {
		safeArgs[i] = gobSafeValue(arg)
	}
	resp, err := sb.call(ctx, sandboxRequest{Op: "query", SQL: query, Args: safeArgs, DeadlineNanos: deadline})
	if err != nil {
		return nil, err
	}
	if resp.Rows == nil {
		// gob does not carry the nil/empty distinction, and the JSON surface
		// does: a zero-row answer is `rows: []` today, not `rows: null`.
		return []map[string]any{}, nil
	}
	return resp.Rows, nil
}

// readCapBytes is the largest reply this sandbox will decode. It is the result
// cap plus protocol headroom, so a child that answered with something larger
// than it was allowed to build is refused before the parent allocates for it.
func (sb *sqlSandbox) readCapBytes() int {
	if sb.pool.limits.maxResultBytes <= 0 {
		return sandboxFrameMaxBytes
	}
	return int(sb.pool.limits.maxResultBytes) + (1 << 20)
}

// call writes one request and waits for its reply under ctx. A child that does
// not answer in time is interrupted, then killed: a wedged sandbox must never
// hold an admission slot or a caller's request open.
func (sb *sqlSandbox) call(ctx context.Context, req sandboxRequest) (sandboxResponse, error) {
	type frameResult struct {
		resp sandboxResponse
		err  error
	}
	ch := make(chan frameResult, 1)
	go func() {
		if err := writeSandboxFrame(sb.stdin, req); err != nil {
			ch <- frameResult{err: err}
			return
		}
		var resp sandboxResponse
		if err := readSandboxFrame(sb.stdout, &resp, sb.readCapBytes()); err != nil {
			ch <- frameResult{err: err}
			return
		}
		ch <- frameResult{resp: resp}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			sb.markDead()
			return sandboxResponse{}, sandboxError(SandboxKindChild, ErrSandboxUnavailable,
				fmt.Sprintf("analytics sandbox stopped responding: %v", r.err))
		}
		// Every op converts its error frame here, not at one call site: a
		// refused cursor, delete or insert is a failed refresh, and a refresh
		// that reports success while its copy did not happen hands the caller
		// answers built from a stale or half-copied sandbox.
		if r.resp.Kind == "err" {
			return sandboxResponse{}, sandboxError(r.resp.ErrorKind, sandboxSentinel(r.resp.ErrorKind), r.resp.Message)
		}
		return r.resp, nil
	case <-ctx.Done():
		// Interrupt first: the driver maps a cancelled context to DuckDB's
		// interrupt, so an ordinary timeout does not have to cost the tenant its
		// warm copy. close() gives it the rest of the grace period, then kills.
		sb.markDead()
		return sandboxResponse{}, sandboxError(SandboxKindTimeout, ErrSandboxTimeout,
			"query exceeded the analytics sandbox deadline")
	}
}

// readFrame reads one unsolicited frame (the handshake) under ctx.
func (sb *sqlSandbox) readFrame(ctx context.Context) (sandboxResponse, error) {
	type frameResult struct {
		resp sandboxResponse
		err  error
	}
	ch := make(chan frameResult, 1)
	go func() {
		var resp sandboxResponse
		err := readSandboxFrame(sb.stdout, &resp, sb.readCapBytes())
		ch <- frameResult{resp: resp, err: err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		return sandboxResponse{}, ctx.Err()
	}
}

// markDead flags a child that stopped answering and interrupts it. It takes no
// lock: the caller may already hold the sandbox mutex, and the process signal is
// safe from any goroutine. Reaping happens in close(), which the pool calls
// after evicting it.
func (sb *sqlSandbox) markDead() {
	sb.dead.Store(true)
	if sb.cmd != nil && sb.cmd.Process != nil {
		_ = sb.cmd.Process.Signal(os.Interrupt)
	}
}

// close kills the child, reaps it and removes its spill directory.
func (sb *sqlSandbox) close() {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.closeLocked()
}

func (sb *sqlSandbox) closeLocked() {
	if sb.closed {
		return
	}
	sb.closed = true
	// Ask politely first: a child serving a final frame gets to finish it.
	_ = writeSandboxFrame(sb.stdin, sandboxRequest{Op: "close"})
	_ = sb.stdin.Close()
	stopChild(sb.cmd, sb.pool.limits.killGrace)
	sb.pool.reaped.Add(1)
	if sb.tmpDir != "" {
		_ = os.RemoveAll(sb.tmpDir)
	}
}

// frameValueBytes is the dominant term of one value's encoded size — its text.
// It approximates what the frame will cost, which is all a batch budget needs.
func frameValueBytes(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []byte:
		return len(x)
	case []any:
		total := 0
		for _, item := range x {
			total += frameValueBytes(item)
		}
		return total
	case map[string]any:
		total := 0
		for k, item := range x {
			total += len(k) + frameValueBytes(item)
		}
		return total
	default:
		return 16
	}
}

// stopChild gives the process grace to exit, then kills it, and reaps it
// exactly once — a zombie child would hold a pid and its exit status forever.
func stopChild(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-waited
	}
}

// sandboxError builds the typed failure every sandbox path returns.
func sandboxError(kind string, sentinel error, message string) error {
	return &SandboxError{Kind: kind, Message: message, Err: sentinel}
}

// gobSafeValue keeps the pipe to the child encodable. Both ends are the same
// binary, so the registered set is exactly what a duckdb scan of the sandbox
// schema can produce; anything else is rendered as text, which the child's
// INSERT casts back to the column type.
func gobSafeValue(v any) any {
	switch x := v.(type) {
	case nil, string, []byte, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, time.Time,
		[]any, map[string]any:
		return x
	default:
		return fmt.Sprint(v)
	}
}

// sandboxSchema mirrors the analytics tables inside a sandbox. The project_id
// columns stay: the scoped CTEs still filter on them, and keeping the column
// makes the sandbox schema a strict subset of the main one.
var sandboxSchema = []string{
	// Column-for-column mirror of the main events table — refresh copies
	// SELECT *, so order and count must match exactly.
	`CREATE TABLE IF NOT EXISTS events (
		project_id UUID NOT NULL,
		event_id UUID NOT NULL,
		distinct_id VARCHAR NOT NULL DEFAULT '',
		session_id VARCHAR NOT NULL DEFAULT '',
		event_name VARCHAR NOT NULL DEFAULT '',
		event_type VARCHAR NOT NULL DEFAULT '',
		properties VARCHAR NOT NULL DEFAULT '',
		agent_id VARCHAR,
		tool_name VARCHAR,
		tool_input VARCHAR,
		tool_output VARCHAR,
		tokens_input UINTEGER,
		tokens_output UINTEGER,
		cost_usd FLOAT,
		latency_ms UINTEGER,
		model_name VARCHAR,
		is_error BOOLEAN NOT NULL DEFAULT false,
		error_message VARCHAR,
		"timestamp" TIMESTAMPTZ NOT NULL,
		inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		visitor_class VARCHAR NOT NULL DEFAULT 'human',
		bot_name VARCHAR,
		referrer_host VARCHAR,
		referrer_channel VARCHAR NOT NULL DEFAULT '',
		user_agent VARCHAR,
		insert_id VARCHAR,
		is_unplanned BOOLEAN NOT NULL DEFAULT false,
		platform VARCHAR NOT NULL DEFAULT '',
		PRIMARY KEY (project_id, event_id)
	)`,
	`CREATE TABLE IF NOT EXISTS aliases (
		project_id UUID NOT NULL,
		anonymous_id VARCHAR NOT NULL,
		canonical_id VARCHAR NOT NULL,
		PRIMARY KEY (project_id, anonymous_id)
	)`,
	`CREATE TABLE IF NOT EXISTS external_rows (
		project_id UUID NOT NULL,
		connector_id UUID NOT NULL,
		table_name VARCHAR NOT NULL,
		row_key VARCHAR NOT NULL,
		cursor VARCHAR,
		data VARCHAR NOT NULL DEFAULT '{}',
		synced_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, connector_id, table_name, row_key)
	)`,
	`CREATE VIEW IF NOT EXISTS resolved_events AS
	SELECT e.*, coalesce(a.canonical_id, e.distinct_id) AS canonical_distinct_id
	FROM events e
	LEFT JOIN aliases a
		ON a.project_id = e.project_id AND a.anonymous_id = e.distinct_id`,
	`CREATE VIEW IF NOT EXISTS sessions AS
	SELECT
		project_id,
		session_id,
		distinct_id,
		min("timestamp") AS session_start,
		max("timestamp") AS session_end,
		count(*) AS event_count,
		sum(coalesce(tokens_input, 0)) AS total_tokens_in,
		sum(coalesce(tokens_output, 0)) AS total_tokens_out,
		sum(coalesce(cost_usd, 0)) AS total_cost_usd,
		max("timestamp") AS last_event_at
	FROM events
	WHERE session_id <> ''
	GROUP BY project_id, session_id, distinct_id`,
}
