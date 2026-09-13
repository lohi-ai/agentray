package storage

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// duckdb_sandbox_worker.go — the untrusted-SQL execution environment, as a
// child process of the API binary.
//
// run_sql reaches untrusted, agent-authored SQL. Everything about that SQL is
// dangerous, and the measurements behind this design settled which danger is
// containable where:
//
//   - duckdb's `memory_limit` does not bound scalar length-minting functions.
//     `SELECT repeat('x', 5e8)` handed Go a 500 MB cell from an engine that
//     believed its limit was 122 MiB, and an engine-side projection guard still
//     OOM-killed a 192 MiB container. No in-process check can be a hard bound.
//   - the kernel can bound a *process*, not an in-process callback: the child
//     is killed, the API and the single ingest writer are untouched.
//
// So the engine, the copy of the tenant's rows, the untrusted SELECT and the
// materialization of its result all live here, in a process the parent can
// kill. The parent keeps every trusted read (it is the only side that opens
// the analytics file) and streams rows down the pipe; the child never sees the
// file, the network, or the parent's environment.
//
// The child is also where the operator-visible limits are enforced, so a
// hostile query cannot make the parent allocate on its behalf: rows and bytes
// are capped while the result is built, before a frame is written.

// SandboxWorkerArgv is the argv[1] the parent re-execs this binary with. It is
// exported because a test binary plays the child role too: a package whose
// tests execute run_sql must dispatch this argv to RunSandboxWorker from
// TestMain (see duckdb_sandbox_worker_test.go).
const SandboxWorkerArgv = "sql-sandbox-worker"

// sandboxFrameMaxBytes bounds one encoded frame the child will read. The parent
// never sends more than one copy batch, so this is pure headroom.
const sandboxFrameMaxBytes = 64 << 20

// Sandbox error kinds. The parent maps these to the operator-visible answer;
// "sql" is the engine's own answer to the author, everything else means the
// sandbox refused, ran out of budget, or died, and must never masquerade as bad
// SQL.
const (
	SandboxKindSQL         = "sql"
	SandboxKindRows        = "rows"
	SandboxKindBytes       = "bytes"
	SandboxKindTimeout     = "timeout"
	SandboxKindUnavailable = "unavailable"
	SandboxKindChild       = "child"
)

// SandboxError is the typed failure every sandbox path returns. Kind is one of
// the SandboxKind* constants; Err carries the sentinel a caller can test with
// errors.Is.
type SandboxError struct {
	Kind    string
	Message string
	Err     error
}

func (e *SandboxError) Error() string { return e.Message }

func (e *SandboxError) Unwrap() error { return e.Err }

// ErrSandboxRows/ErrSandboxBytes/ErrSandboxTimeout/ErrSandboxUnavailable are
// the sentinels callers test with errors.Is; ErrSandboxSQL is deliberately not
// a limit — it is the engine telling the author their query is wrong.
var (
	ErrSandboxSQL         = errors.New("sandbox: sql error")
	ErrSandboxRows        = errors.New("sandbox: row limit exceeded")
	ErrSandboxBytes       = errors.New("sandbox: result byte limit exceeded")
	ErrSandboxTimeout     = errors.New("sandbox: request deadline exceeded")
	ErrSandboxUnavailable = errors.New("sandbox: unavailable")
)

func sandboxSentinel(kind string) error {
	switch kind {
	case SandboxKindSQL:
		return ErrSandboxSQL
	case SandboxKindRows:
		return ErrSandboxRows
	case SandboxKindBytes:
		return ErrSandboxBytes
	case SandboxKindTimeout:
		return ErrSandboxTimeout
	default:
		return ErrSandboxUnavailable
	}
}

// IsSandboxLimit reports whether err is a sandbox budget refusal (as opposed to
// a SQL error the author can fix, or a transport failure).
func IsSandboxLimit(err error) bool {
	return errors.Is(err, ErrSandboxRows) ||
		errors.Is(err, ErrSandboxBytes) ||
		errors.Is(err, ErrSandboxTimeout)
}

// IsSandboxUnavailable reports whether the sandbox itself could not run the
// query — a category the API answers with 503 rather than 400, because the
// caller's SQL is not the problem.
func IsSandboxUnavailable(err error) bool {
	return errors.Is(err, ErrSandboxUnavailable)
}

// sandboxRequest is one parent→child message.
type sandboxRequest struct {
	Op    string  `json:"op"`    // cursor | delete | insert | query | close
	Table string  `json:"table"` // delete | insert
	Rows  [][]any `json:"rows"`  // insert
	SQL   string  `json:"sql"`   // query
	Args  []any   `json:"args"`  // query
	// DeadlineNanos is the absolute instant the parent's request budget ends.
	// The child bounds its own work by it so a query that outlives the parent's
	// deadline cannot keep a slot warm after the caller has been answered.
	DeadlineNanos int64 `json:"deadline_nanos"`
}

// sandboxResponse is one child→parent message.
type sandboxResponse struct {
	Kind    string `json:"kind"` // ready | ok | rows | err
	Message string `json:"message"`
	// ErrorKind classifies an err frame: SandboxKindSQL is the engine answering
	// the author, every other kind is the sandbox refusing, exhausting its
	// budget, or dying.
	ErrorKind string `json:"error_kind"`
	// EngineMemoryLimit is duckdb's own view of the instance limit, reported in
	// the ready frame so the budget that is claimed is the budget that is set.
	EngineMemoryLimit string `json:"engine_memory_limit"`
	// RlimitBytes is the address-space bound the child applied to itself, 0
	// where the platform has none.
	RlimitBytes uint64           `json:"rlimit_bytes"`
	Columns     []string         `json:"columns"`
	Rows        []map[string]any `json:"rows"`
	// Count/Watermark/WatermarkID are the child's own event cursor, reported to
	// the parent so the incremental copy can resume from where the child
	// actually is rather than from where the parent assumes it is.
	Count       int64     `json:"count"`
	Watermark   time.Time `json:"watermark"`
	WatermarkID string    `json:"watermark_id"`
}

func init() {
	// Interface values (map[string]any results, []any insert rows) are encoded
	// by concrete type name, so both ends must know the same set. The same
	// binary runs both ends; the set is exactly what normalizeSQLValue and a
	// duckdb scan of the sandbox schema can produce.
	gob.Register("")
	gob.Register([]byte(nil))
	gob.Register(false)
	gob.Register(int64(0))
	gob.Register(int32(0))
	gob.Register(int(0))
	gob.Register(uint64(0))
	gob.Register(uint32(0))
	gob.Register(uint16(0))
	gob.Register(uint8(0))
	gob.Register(float64(0))
	gob.Register(float32(0))
	gob.Register(time.Time{})
	gob.Register([]any{})
	gob.Register(map[string]any{})
}

// writeSandboxFrame encodes one message with a length prefix. Untrusted data
// never reaches this side of the design; the length prefix exists so the reader
// can refuse an unbounded frame before decoding it.
func writeSandboxFrame(w io.Writer, msg any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		return fmt.Errorf("sandbox frame encode: %w", err)
	}
	if buf.Len() > sandboxFrameMaxBytes {
		return fmt.Errorf("sandbox frame of %d bytes exceeds the %d-byte cap", buf.Len(), sandboxFrameMaxBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(buf.Len()))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// readSandboxFrame reads one length-prefixed message, refusing a frame larger
// than the cap before allocating for it.
func readSandboxFrame(r *bufio.Reader, msg any, maxBytes int) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if int(n) > maxBytes {
		return fmt.Errorf("sandbox frame of %d bytes exceeds the %d-byte cap", n, maxBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return gob.NewDecoder(bytes.NewReader(payload)).Decode(msg)
}

// sandboxWorkerOptions is the child's configuration, all of it owned by the
// parent and passed on argv so there is exactly one place the budget is
// declared.
type sandboxWorkerOptions struct {
	tmpDir         string
	memoryLimit    string
	tempSize       string
	threads        int
	maxRows        int
	maxResultBytes int64
	queryTimeout   time.Duration
	// rlimitBudgetBytes is the address space the child may allocate *above* the
	// virtual size it already holds when it opens the engine. A fixed rlimit is
	// not usable: the Go runtime alone holds hundreds of MiB of virtual address
	// space at startup (measured: ~416 MiB on darwin/arm64), so the bound has to
	// be relative to the process's own footprint or it would forbid the runtime
	// itself.
	rlimitBudgetBytes int64
}

func parseSandboxWorkerOptions(args []string) sandboxWorkerOptions {
	opts := sandboxWorkerOptions{
		threads:           1,
		maxRows:           sandboxMaxRows,
		maxResultBytes:    sandboxMaxResultBytes,
		queryTimeout:      sandboxQueryTimeout,
		memoryLimit:       sandboxMemoryLimit,
		tempSize:          sandboxTempSize,
		rlimitBudgetBytes: sandboxRlimitBudget,
	}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			continue
		}
		switch key {
		case "--tmp-dir":
			opts.tmpDir = value
		case "--memory-limit":
			opts.memoryLimit = value
		case "--temp-size":
			opts.tempSize = value
		case "--threads":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				opts.threads = n
			}
		case "--max-rows":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				opts.maxRows = n
			}
		case "--max-result-bytes":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				opts.maxResultBytes = n
			}
		case "--query-timeout":
			if d, err := time.ParseDuration(value); err == nil && d > 0 {
				opts.queryTimeout = d
			}
		case "--rlimit-bytes":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				opts.rlimitBudgetBytes = n
			}
		}
	}
	return opts
}

// RunSandboxWorker is the child's entry point: open the engine, apply the
// process bound, then serve frames until the parent closes the pipe.
//
// A hostile query cannot escape this process: the instance has no filesystem or
// network access (enable_external_access=false, lock_configuration=true), it
// holds only the rows the parent chose to send, and its address space is capped
// by its own rlimit where the platform has one.
func RunSandboxWorker(args []string, stdin io.Reader, stdout io.Writer) error {
	opts := parseSandboxWorkerOptions(args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Measure first, then bound: the rlimit is the process's own current virtual
	// size plus the budget, so the Go runtime's reservations are already
	// accounted for and DuckDB is the thing that gets limited.
	rlimitBytes, err := applyChildProcessBounds(opts.rlimitBudgetBytes)
	if err != nil {
		return fmt.Errorf("sandbox worker: set memory limit: %w", err)
	}

	engine, err := openSandboxEngine(ctx, opts)
	if err != nil {
		return err
	}
	defer engine.close()

	writer := bufio.NewWriter(stdout)
	ready := sandboxResponse{
		Kind:              "ready",
		EngineMemoryLimit: engine.memoryLimit,
		RlimitBytes:       rlimitBytes,
	}
	if err := writeSandboxFrame(writer, ready); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	reader := bufio.NewReader(stdin)
	for {
		var req sandboxRequest
		if err := readSandboxFrame(reader, &req, sandboxFrameMaxBytes); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if req.Op == "close" {
			return nil
		}
		resp := engine.handle(ctx, req)
		if err := writeSandboxFrame(writer, resp); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
}

// sandboxEngine is the child's isolated in-memory DuckDB.
type sandboxEngine struct {
	opts        sandboxWorkerOptions
	db          *sql.DB
	feeder      *sql.Conn
	runner      *sql.Conn
	mu          sync.Mutex
	memoryLimit string
}

// openSandboxEngine creates the in-memory instance, builds the project-scoped
// tables and views, and locks the whole instance down before any row lands.
// This is the sandbox's own half of what used to be openSQLSandbox; the parent
// half is the trusted copy that streams rows to it.
func openSandboxEngine(ctx context.Context, opts sandboxWorkerOptions) (*sandboxEngine, error) {
	connector, err := duckdb.NewConnector("", func(execer driver.ExecerContext) error {
		stmts := []string{"SET TimeZone = 'UTC'"}
		if opts.tmpDir != "" {
			stmts = append(stmts,
				"SET temp_directory = '"+strings.ReplaceAll(opts.tmpDir, "'", "''")+"'",
				"SET max_temp_directory_size = '"+opts.tempSize+"'")
		}
		for _, stmt := range stmts {
			if _, err := execer.ExecContext(context.Background(), stmt, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)

	eng := &sandboxEngine{opts: opts, db: db}
	feeder, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	eng.feeder = feeder
	runner, err := db.Conn(ctx)
	if err != nil {
		eng.close()
		return nil, err
	}
	eng.runner = runner

	// Lockdown is instance-wide: enable_external_access is a global setting in
	// DuckDB, so it must be set before the feeder needs no files (it never
	// does — the parent copies rows through the pipe). After this the instance
	// cannot touch the filesystem or network at all.
	for _, stmt := range []string{
		"SET memory_limit = '" + opts.memoryLimit + "'",
		fmt.Sprintf("SET threads = %d", opts.threads),
		"SET enable_external_access = false",
		"SET lock_configuration = true",
	} {
		if _, err := feeder.ExecContext(ctx, stmt); err != nil {
			eng.close()
			return nil, fmt.Errorf("sandbox %q: %w", stmt, err)
		}
	}
	if err := feeder.QueryRowContext(ctx, "SELECT current_setting('memory_limit')").Scan(&eng.memoryLimit); err != nil {
		eng.close()
		return nil, fmt.Errorf("sandbox memory_limit: %w", err)
	}
	for _, stmt := range sandboxSchema {
		if _, err := feeder.ExecContext(ctx, stmt); err != nil {
			eng.close()
			return nil, fmt.Errorf("sandbox schema: %w", err)
		}
	}
	return eng, nil
}

// handle runs one request. Ops are strictly request/response and the parent
// serializes them, so the engine needs only the coarse mutex below to keep a
// refresh from racing the query that follows it.
func (e *sandboxEngine) handle(ctx context.Context, req sandboxRequest) sandboxResponse {
	switch req.Op {
	case "cursor":
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.cursor(ctx, req.Table)
	case "delete":
		if !sandboxTableNames[req.Table] {
			return errResponse(SandboxKindChild, fmt.Sprintf("unknown sandbox table %q", req.Table))
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, err := e.feeder.ExecContext(ctx, "DELETE FROM "+req.Table); err != nil {
			return errResponse(SandboxKindChild, err.Error())
		}
		return sandboxResponse{Kind: "ok"}
	case "insert":
		if !sandboxTableNames[req.Table] {
			return errResponse(SandboxKindChild, fmt.Sprintf("unknown sandbox table %q", req.Table))
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.insertRows(ctx, req.Table, req.Rows); err != nil {
			return errResponse(SandboxKindChild, err.Error())
		}
		return sandboxResponse{Kind: "ok"}
	case "query":
		return e.query(ctx, req)
	default:
		return errResponse(SandboxKindChild, fmt.Sprintf("unknown sandbox op %q", req.Op))
	}
}

// cursor reports this instance's own high-water mark for the append-only
// events table: the parent resumes its incremental copy from here, so a row
// that shares a timestamp with the last copied one is not skipped, and a count
// drift (a delete upstream) is visible as child > main.
func (e *sandboxEngine) cursor(ctx context.Context, table string) sandboxResponse {
	if !sandboxTableNames[table] {
		return errResponse(SandboxKindChild, fmt.Sprintf("unknown sandbox table %q", table))
	}
	resp := sandboxResponse{Kind: "ok"}
	if table != "events" {
		if err := e.feeder.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&resp.Count); err != nil {
			return errResponse(SandboxKindChild, err.Error())
		}
		return resp
	}
	if err := e.feeder.QueryRowContext(ctx,
		`SELECT coalesce(max(inserted_at), TIMESTAMPTZ '1970-01-01') FROM events`).Scan(&resp.Watermark); err != nil {
		return errResponse(SandboxKindChild, err.Error())
	}
	if err := e.feeder.QueryRowContext(ctx,
		`SELECT coalesce(max(event_id::VARCHAR), '') FROM events WHERE inserted_at = ?`, resp.Watermark).Scan(&resp.WatermarkID); err != nil {
		return errResponse(SandboxKindChild, err.Error())
	}
	if err := e.feeder.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&resp.Count); err != nil {
		return errResponse(SandboxKindChild, err.Error())
	}
	return resp
}

// insertRows applies one batch the parent asked for, using the same batched
// multi-row INSERT the in-process copy used.
func (e *sandboxEngine) insertRows(ctx context.Context, table string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	nCols := len(rows[0])
	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", nCols), ",") + ")"
	single, err := e.feeder.PrepareContext(ctx, fmt.Sprintf("INSERT INTO %s VALUES %s", table, placeholder))
	if err != nil {
		return err
	}
	defer single.Close()
	for _, row := range rows {
		if len(row) != nCols {
			return fmt.Errorf("row has %d columns, want %d", len(row), nCols)
		}
	}
	flat := make([]any, 0, len(rows)*nCols)
	for _, row := range rows {
		flat = append(flat, row...)
	}
	if len(rows) == 1 {
		_, err := single.ExecContext(ctx, flat...)
		return err
	}
	batchPlaceholder := strings.TrimSuffix(strings.Repeat(placeholder+",", len(rows)), ",")
	_, err = e.feeder.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s VALUES %s", table, batchPlaceholder), flat...)
	return err
}

// query runs one already-validated, already-rewritten SELECT on the locked
// runner connection and materializes it under the row and byte caps.
//
// The caps are enforced here, not after the fact: a result that would exceed
// either one fails without ever being framed, so the parent never allocates on
// a hostile query's behalf. SIGINT from the parent cancels the in-flight query
// through the driver's context mapping, which is how a deadline is served
// without losing the tenant's warm copy.
func (e *sandboxEngine) query(ctx context.Context, req sandboxRequest) sandboxResponse {
	e.mu.Lock()
	defer e.mu.Unlock()

	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if req.DeadlineNanos > 0 {
		deadline := time.Unix(0, req.DeadlineNanos)
		d, stop := context.WithDeadline(qctx, deadline)
		defer stop()
		qctx = d
	} else {
		d, stop := context.WithTimeout(qctx, e.opts.queryTimeout)
		defer stop()
		qctx = d
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	interrupted := make(chan struct{})
	defer close(interrupted)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-interrupted:
		}
	}()

	rows, err := e.runner.QueryContext(qctx, req.SQL, req.Args...)
	if err != nil {
		return engineErrResponse(qctx, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return errResponse(SandboxKindChild, err.Error())
	}
	results := []map[string]any{}
	var bytes int64
	for rows.Next() {
		if len(results) >= e.opts.maxRows {
			return errResponse(SandboxKindRows,
				fmt.Sprintf("query exceeded %d rows; add a LIMIT", e.opts.maxRows))
		}
		// Scan into *any: the driver's ScanType() reports the primitive type
		// without nullability, so a NULL in a VARCHAR column scanned into
		// *string errors. *any takes whatever the driver hands back.
		valuePtrs := make([]any, len(columns))
		for i := range columns {
			valuePtrs[i] = new(any)
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return errResponse(SandboxKindChild, err.Error())
		}
		item := make(map[string]any, len(columns))
		for i, column := range columns {
			value := normalizeSQLValue(*(valuePtrs[i].(*any)))
			bytes += sandboxValueBytes(column, value)
			item[column] = value
		}
		if e.opts.maxResultBytes > 0 && bytes > e.opts.maxResultBytes {
			return errResponse(SandboxKindBytes, fmt.Sprintf(
				"query result exceeded %d bytes; select fewer columns or add a LIMIT", e.opts.maxResultBytes))
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return engineErrResponse(qctx, err)
	}
	return sandboxResponse{Kind: "rows", Columns: columns, Rows: results}
}

// sandboxValueBytes counts what a cell costs once it is retained: the column
// name, the string or binary payload, and a fixed per-value overhead for the
// interface + map entry.
func sandboxValueBytes(column string, value any) int64 {
	const overhead = 48
	n := int64(len(column)) + overhead
	switch v := value.(type) {
	case string:
		n += int64(len(v))
	case []byte:
		n += int64(len(v))
	case []any:
		for _, item := range v {
			n += int64(len(fmt.Sprint(item))) + overhead
		}
	case map[string]any:
		for k, item := range v {
			n += int64(len(k)) + int64(len(fmt.Sprint(item))) + 2*overhead
		}
	}
	return n
}

// engineErrResponse separates "the author's SQL is wrong" from "the sandbox ran
// out of its budget or died" — the caller shows the first to the author and
// must not show the second as if the query were at fault.
func engineErrResponse(ctx context.Context, err error) sandboxResponse {
	if ctx.Err() != nil {
		return errResponse(SandboxKindTimeout, "query exceeded the sandbox deadline")
	}
	msg := err.Error()
	if strings.Contains(msg, "Out of Memory Error") {
		return errResponse(SandboxKindBytes, "query exceeded the sandbox memory limit; narrow the query or add a LIMIT")
	}
	return errResponse(SandboxKindSQL, msg)
}

func errResponse(kind, message string) sandboxResponse {
	return sandboxResponse{Kind: "err", ErrorKind: kind, Message: message}
}

func (e *sandboxEngine) close() {
	if e.feeder != nil {
		_ = e.feeder.Close()
	}
	if e.runner != nil {
		_ = e.runner.Close()
	}
	if e.db != nil {
		_ = e.db.Close()
	}
}

// sandboxTableNames guards the two ops whose table is named by the parent, so a
// malformed frame cannot turn into an arbitrary identifier.
var sandboxTableNames = map[string]bool{
	"events":        true,
	"aliases":       true,
	"external_rows": true,
}
