package connector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func init() {
	Register("postgres", openPostgres)
}

// postgresSource reads an external PostgreSQL database. One short-lived
// connection per Source; the Engine opens and closes it around each sync run.
// A Source is used from one goroutine at a time, so the type cache is unlocked.
type postgresSource struct {
	conn *pgx.Conn
	// columnTypes caches format_type lookups per "table\x00column" — the type
	// cannot change within a Source's lifetime, and PullRows runs per batch.
	columnTypes map[string]string
}

// openPostgres validates and dials the DSN. Every returned error is sanitized:
// the DSN (which embeds the password) never appears in an error string, so a
// bad-credential or bad-host failure is safe to persist and show in the UI.
func openPostgres(ctx context.Context, dsn string) (Source, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// ParseConfig error text can echo the raw connection string; never
		// propagate it.
		return nil, fmt.Errorf("postgres: invalid connection string")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(dialCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect to %s:%d/%s failed: %s", cfg.Host, cfg.Port, cfg.Database, sanitizePGError(err, cfg.Password))
	}
	return &postgresSource{conn: conn, columnTypes: map[string]string{}}, nil
}

// sanitizePGError renders a connection/query error without ever leaking the
// password: the concrete secret is blanked defensively, and pgconn's verbose
// "failed to connect to `host=… password=…`" preamble is reduced to its root
// cause.
func sanitizePGError(err error, password string) string {
	var pgErr *pgconn.PgError
	msg := err.Error()
	if errors.As(err, &pgErr) {
		msg = pgErr.Message
	}
	if password != "" {
		msg = strings.ReplaceAll(msg, password, "•••")
	}
	// pgconn connect errors repeat the full config between backticks; keep only
	// the trailing cause when that shape is present.
	if i := strings.LastIndex(msg, "`: "); i >= 0 {
		msg = msg[i+3:]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

func (p *postgresSource) Kind() string { return "postgres" }

func (p *postgresSource) Close() {
	if p.conn != nil {
		_ = p.conn.Close(context.Background())
	}
}

func (p *postgresSource) TestConnection(ctx context.Context) error {
	if err := p.conn.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping failed: %s", sanitizePGError(err, p.conn.Config().Password))
	}
	return nil
}

// DiscoverSchema lists ordinary tables (and their columns) in every
// non-system schema the connection can see, with primary-key membership so
// the UI and the AI draft can propose a row key.
func (p *postgresSource) DiscoverSchema(ctx context.Context) ([]Table, error) {
	rows, err := p.conn.Query(ctx, `
SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
	EXISTS (
		SELECT 1
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_name = tc.constraint_name
			AND kcu.table_schema = tc.table_schema
			AND kcu.table_name = tc.table_name
		WHERE tc.constraint_type = 'PRIMARY KEY'
			AND tc.table_schema = c.table_schema
			AND tc.table_name = c.table_name
			AND kcu.column_name = c.column_name
	) AS is_pk
FROM information_schema.columns c
JOIN information_schema.tables t
	ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE t.table_type = 'BASE TABLE'
	AND c.table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY c.table_schema, c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("postgres: discover schema: %s", sanitizePGError(err, p.conn.Config().Password))
	}
	defer rows.Close()

	var tables []Table
	index := map[string]int{}
	for rows.Next() {
		var schema, table, column, dataType string
		var isPK bool
		if err := rows.Scan(&schema, &table, &column, &dataType, &isPK); err != nil {
			return nil, err
		}
		name := table
		if schema != "public" {
			name = schema + "." + table
		}
		i, ok := index[name]
		if !ok {
			i = len(tables)
			index[name] = i
			tables = append(tables, Table{Name: name})
		}
		tables[i].Columns = append(tables[i].Columns, Column{Name: column, Type: dataType, IsPrimaryKey: isPK})
	}
	return tables, rows.Err()
}

// PullRows fetches the next incremental batch, keyset-paginated on
// (cursor, key) so rows tied on one cursor value are never skipped across a
// batch boundary. NULL cursors sort first and are paged by key alone, then the
// scan flows into the non-NULL region. Identifiers are quote-sanitized; cursor
// and key values are cast server-side to their columns' own types so text
// cursors compare correctly against ints, timestamps, and uuids.
func (p *postgresSource) PullRows(ctx context.Context, req PullRequest) (PullResult, error) {
	if req.Table == "" || req.KeyColumn == "" || req.CursorColumn == "" {
		return PullResult{}, fmt.Errorf("postgres: table, key column, and cursor column are required")
	}
	if req.Limit <= 0 {
		req.Limit = 1000
	}
	tableIdent, err := quoteQualified(req.Table)
	if err != nil {
		return PullResult{}, err
	}
	cursorIdent := pgx.Identifier{req.CursorColumn}.Sanitize()
	keyIdent := pgx.Identifier{req.KeyColumn}.Sanitize()

	query := fmt.Sprintf(`SELECT * FROM %s`, tableIdent)
	var args []any
	switch {
	case req.Cursor != "" && req.CursorKey != "":
		cursorType, err := p.columnType(ctx, req.Table, req.CursorColumn)
		if err != nil {
			return PullResult{}, err
		}
		keyType, err := p.columnType(ctx, req.Table, req.KeyColumn)
		if err != nil {
			return PullResult{}, err
		}
		query += fmt.Sprintf(` WHERE %s > CAST($1 AS %s) OR (%s = CAST($1 AS %s) AND %s > CAST($2 AS %s))`,
			cursorIdent, cursorType, cursorIdent, cursorType, keyIdent, keyType)
		args = append(args, req.Cursor, req.CursorKey)
	case req.Cursor != "":
		// Legacy position without a key half: strict cursor comparison.
		cursorType, err := p.columnType(ctx, req.Table, req.CursorColumn)
		if err != nil {
			return PullResult{}, err
		}
		query += fmt.Sprintf(` WHERE %s > CAST($1 AS %s)`, cursorIdent, cursorType)
		args = append(args, req.Cursor)
	case req.CursorKey != "":
		// Still inside the NULL-cursor region (sorted first): page by key,
		// then flow into the non-NULL region.
		keyType, err := p.columnType(ctx, req.Table, req.KeyColumn)
		if err != nil {
			return PullResult{}, err
		}
		query += fmt.Sprintf(` WHERE (%s IS NULL AND %s > CAST($1 AS %s)) OR %s IS NOT NULL`,
			cursorIdent, keyIdent, keyType, cursorIdent)
		args = append(args, req.CursorKey)
	}
	query += fmt.Sprintf(` ORDER BY %s ASC NULLS FIRST, %s ASC LIMIT %d`, cursorIdent, keyIdent, req.Limit)

	rows, err := p.conn.Query(ctx, query, args...)
	if err != nil {
		return PullResult{}, fmt.Errorf("postgres: pull %s: %s", req.Table, sanitizePGError(err, p.conn.Config().Password))
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	out := PullResult{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return PullResult{}, err
		}
		data := make(map[string]any, len(fields))
		var key, cursor string
		for i, fd := range fields {
			v := normalizePGValue(values[i])
			data[fd.Name] = v
			switch fd.Name {
			case req.KeyColumn:
				key = stringifyPGValue(v)
			case req.CursorColumn:
				cursor = stringifyPGValue(v)
			}
		}
		if key == "" {
			return PullResult{}, fmt.Errorf("postgres: row in %s has empty key column %s", req.Table, req.KeyColumn)
		}
		out.Rows = append(out.Rows, Row{Key: key, Cursor: cursor, Data: data})
		// Rows arrive in (cursor NULLS FIRST, key) order, so the last row is
		// the keyset position the next pull resumes from. Cursor stays "" while
		// still inside the NULL region; the key carries the progress there.
		out.NextCursor = cursor
		out.NextCursorKey = key
	}
	if err := rows.Err(); err != nil {
		return PullResult{}, fmt.Errorf("postgres: pull %s: %s", req.Table, sanitizePGError(err, p.conn.Config().Password))
	}
	out.HasMore = len(out.Rows) == req.Limit
	return out, nil
}

// columnType returns the server-rendered type (format_type) of one column, for
// the cursor/key CASTs, cached for the Source's lifetime. The lookup itself is
// parameterized, so untrusted config can only ever name a column, never inject
// SQL.
func (p *postgresSource) columnType(ctx context.Context, table, column string) (string, error) {
	cacheKey := table + "\x00" + column
	if typ, ok := p.columnTypes[cacheKey]; ok {
		return typ, nil
	}
	schema, bare := splitQualified(table)
	var typ string
	err := p.conn.QueryRow(ctx, `
SELECT format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped`,
		schema, bare, column).Scan(&typ)
	if err != nil {
		return "", fmt.Errorf("postgres: cursor column %s.%s not found", table, column)
	}
	// format_type output is server-generated; quote-free validation keeps the
	// later fmt.Sprintf CAST safe even so.
	if strings.ContainsAny(typ, `"'`+";") {
		return "", fmt.Errorf("postgres: unsupported cursor column type %q", typ)
	}
	p.columnTypes[cacheKey] = typ
	return typ, nil
}

// splitQualified splits "schema.table" (default schema public).
func splitQualified(table string) (schema, bare string) {
	if i := strings.IndexByte(table, '.'); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "public", table
}

// quoteQualified renders a possibly schema-qualified table name as safely
// quoted identifiers.
func quoteQualified(table string) (string, error) {
	schema, bare := splitQualified(table)
	if bare == "" || schema == "" {
		return "", fmt.Errorf("postgres: invalid table name %q", table)
	}
	return pgx.Identifier{schema, bare}.Sanitize(), nil
}

// normalizePGValue converts pgx scan values into JSON-encodable shapes: times
// become RFC3339 UTC strings, uuid/bytea bytes become text, and anything the
// JSON encoder cannot handle is stringified rather than dropped.
func normalizePGValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case [16]byte: // uuid
		return uuid.UUID(t).String()
	case []byte:
		return "\\x" + hex.EncodeToString(t)
	case string, bool, int, int8, int16, int32, int64, uint8, uint16, uint32, uint64, float32, float64:
		return t
	default:
		if _, err := json.Marshal(t); err == nil {
			return t
		}
		return fmt.Sprint(t)
	}
}

// stringifyPGValue renders a normalized value as the string key/cursor form.
func stringifyPGValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
