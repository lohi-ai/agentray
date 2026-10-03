package connector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
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
	conn    *pgx.Conn
	binding *SourceBinding
	// columnTypes caches format_type lookups per "table\x00column" — the type
	// cannot change within a Source's lifetime, and PullRows runs per batch.
	columnTypes map[string]string
}

// openPostgres validates and dials the DSN. Every returned error is sanitized:
// the DSN (which embeds the password) never appears in an error string, so a
// bad-credential or bad-host failure is safe to persist and show in the UI.
func openPostgres(ctx context.Context, dsn string) (Source, error) {
	return openPostgresConfig(ctx, dsn, nil, nil)
}

func openPostgresWithPolicy(ctx context.Context, dsn string, policy *SourcePolicy, binding *SourceBinding) (Source, error) {
	return openPostgresConfig(ctx, dsn, policy, binding)
}

func openPostgresConfig(ctx context.Context, dsn string, policy *SourcePolicy, binding *SourceBinding) (Source, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// ParseConfig error text can echo the raw connection string; never
		// propagate it.
		return nil, fmt.Errorf("postgres: invalid connection string")
	}
	if policy != nil {
		if err := admitPostgresConfig(ctx, cfg, policy); err != nil {
			return nil, err
		}
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = map[string]string{}
		}
		for key := range cfg.RuntimeParams {
			switch strings.ToLower(key) {
			case "default_transaction_read_only", "statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout", "application_name":
			default:
				return nil, fmt.Errorf("postgres: connection setting %q is not permitted", key)
			}
		}
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
		cfg.RuntimeParams["statement_timeout"] = "15000"
		cfg.RuntimeParams["lock_timeout"] = "5000"
		cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "15000"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(dialCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect to %s:%d/%s failed: %s", cfg.Host, cfg.Port, cfg.Database, sanitizePGError(err, cfg.Password))
	}
	source := &postgresSource{conn: conn, binding: binding, columnTypes: map[string]string{}}
	if policy != nil {
		var super, createRole, createDB, bypassRLS, broadMembership, canCreateSchema, readOnly bool
		if err := conn.QueryRow(dialCtx, `SELECT r.rolsuper,r.rolcreaterole,r.rolcreatedb,r.rolbypassrls,
EXISTS(SELECT 1 FROM unnest(ARRAY['pg_read_all_data','pg_write_all_data','pg_execute_server_program','pg_read_server_files','pg_write_server_files','pg_signal_backend']) wanted(role_name)
JOIN pg_roles granted ON granted.rolname=wanted.role_name WHERE pg_has_role(current_user,granted.oid,'MEMBER')),
has_database_privilege(current_user,current_database(),'CREATE') OR has_schema_privilege(current_user,$1,'CREATE'),
current_setting('default_transaction_read_only')='on' FROM pg_roles r WHERE r.rolname=current_user`, binding.Schema).
			Scan(&super, &createRole, &createDB, &bypassRLS, &broadMembership, &canCreateSchema, &readOnly); err != nil {
			source.Close()
			return nil, fmt.Errorf("postgres: source role preflight failed")
		}
		if super || createRole || createDB || bypassRLS || broadMembership || canCreateSchema || !readOnly {
			source.Close()
			return nil, fmt.Errorf("postgres: source role has prohibited administrative privileges")
		}
	}
	return source, nil
}

func admitPostgresConfig(ctx context.Context, cfg *pgx.ConnConfig, policy *SourcePolicy) error {
	type endpoint struct {
		host string
		port uint16
		dest SourceDestination
	}
	endpoints := []endpoint{{host: cfg.Host, port: cfg.Port}}
	for _, fallback := range cfg.Fallbacks {
		endpoints = append(endpoints, endpoint{host: fallback.Host, port: fallback.Port})
	}
	for i := range endpoints {
		ep := &endpoints[i]
		if strings.TrimSpace(ep.host) == "" || strings.HasPrefix(ep.host, "/") {
			return fmt.Errorf("postgres: source destination is not approved")
		}
		dest, err := policy.Destination(ep.host, ep.port)
		if err != nil {
			return fmt.Errorf("postgres: source destination is not approved")
		}
		ep.dest = *dest
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, ep.host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("postgres: approved source host did not resolve")
		}
		for _, ip := range ips {
			if !destinationAllowsIP(*dest, ip.IP) {
				return fmt.Errorf("postgres: resolved source address is not approved")
			}
		}
	}
	// Re-resolve and re-check at every socket dial, then pin the socket to the
	// validated address. TLS still sees cfg.Host as its server name.
	cfg.DialFunc = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("postgres: source destination is not approved")
		}
		port64, err := strconv.ParseUint(portText, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("postgres: source destination is not approved")
		}
		dialIP := net.ParseIP(host)
		if dialIP == nil {
			return nil, fmt.Errorf("postgres: source destination is not approved")
		}
		for _, ep := range endpoints {
			if ep.port != uint16(port64) || !destinationAllowsIP(ep.dest, dialIP) {
				continue
			}
			ips, err := net.DefaultResolver.LookupIPAddr(dialCtx, ep.host)
			if err != nil {
				continue
			}
			for _, ip := range ips {
				if ip.IP.Equal(dialIP) {
					return (&net.Dialer{}).DialContext(dialCtx, network, net.JoinHostPort(dialIP.String(), portText))
				}
			}
		}
		return nil, fmt.Errorf("postgres: resolved source address is not approved")
	}
	return nil
}

func destinationAllowsIP(dest SourceDestination, ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, raw := range dest.AllowedIPCIDRs {
		_, network, err := net.ParseCIDR(raw)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
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
	if p.binding != nil {
		return p.discoverApprovedView(ctx)
	}
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

func (p *postgresSource) discoverApprovedView(ctx context.Context) ([]Table, error) {
	b := p.binding
	var relKind string
	if err := p.conn.QueryRow(ctx, `
SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, b.Schema, b.Relation).Scan(&relKind); err != nil {
		return nil, fmt.Errorf("postgres: approved export is unavailable")
	}
	expectedKind := "v"
	if b.RelationKind == RelationKindLegacyTable {
		expectedKind = "r"
	}
	if relKind != expectedKind {
		return nil, fmt.Errorf("postgres: approved export relation kind changed")
	}
	rows, err := p.conn.Query(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, b.Schema, b.Relation)
	if err != nil {
		return nil, fmt.Errorf("postgres: discover approved export failed")
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, fmt.Errorf("postgres: discover approved export failed")
		}
		found[name] = typ
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: discover approved export failed")
	}
	if len(found) != len(b.Columns) {
		return nil, fmt.Errorf("postgres: approved export schema changed")
	}
	cols := make([]Column, 0, len(b.Columns))
	for _, allowed := range b.Columns {
		if found[allowed.Name] != allowed.PGType {
			return nil, fmt.Errorf("postgres: approved export schema changed")
		}
		cols = append(cols, Column{Name: allowed.Name, Type: allowed.PGType, IsPrimaryKey: allowed.Name == b.KeyColumn})
	}
	return []Table{{Name: b.QualifiedRelation(), Columns: cols, RelationKind: b.RelationKind, KeyColumn: b.KeyColumn, KeyStability: b.KeyStability}}, nil
}

// PullRows fetches the next incremental batch, keyset-paginated on
// (cursor, key) so rows tied on one cursor value are never skipped across a
// batch boundary. NULL cursors sort first and are paged by key alone, then the
// scan flows into the non-NULL region. Identifiers are quote-sanitized; cursor
// and key values are cast server-side to their columns' own types so text
// cursors compare correctly against ints, timestamps, and uuids.
func (p *postgresSource) PullRows(ctx context.Context, req PullRequest) (PullResult, error) {
	if req.Table == "" || req.KeyColumn == "" || (!req.Snapshot && req.CursorColumn == "") {
		return PullResult{}, fmt.Errorf("postgres: table, key column, and cursor column are required")
	}
	if p.binding != nil {
		if err := p.validateApprovedRequest(req); err != nil {
			return PullResult{}, err
		}
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

	selectList := "*"
	if p.binding != nil {
		idents := make([]string, 0, len(p.binding.Columns))
		for _, c := range p.binding.Columns {
			idents = append(idents, pgx.Identifier{c.Name}.Sanitize())
		}
		selectList = strings.Join(idents, ", ")
	}
	query := fmt.Sprintf(`SELECT %s FROM %s`, selectList, tableIdent)
	var args []any
	if req.Snapshot {
		keyType, err := p.columnType(ctx, req.Table, req.KeyColumn)
		if err != nil {
			return PullResult{}, err
		}
		if req.CursorKey != "" {
			query += fmt.Sprintf(` WHERE %s > CAST($1 AS %s)`, keyIdent, keyType)
			args = append(args, req.CursorKey)
		}
		query += fmt.Sprintf(` ORDER BY %s ASC LIMIT %d`, keyIdent, req.Limit)
	} else {
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
	}

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

func (p *postgresSource) validateApprovedRequest(req PullRequest) error {
	b := p.binding
	if req.Table != b.QualifiedRelation() || req.KeyColumn != b.KeyColumn {
		return fmt.Errorf("postgres: source relation or key is not approved")
	}
	if req.Snapshot {
		if req.CursorColumn != "" {
			return fmt.Errorf("postgres: snapshot cursor must be empty")
		}
		return nil
	}
	if !b.AllowsCursor(req.CursorColumn) {
		return fmt.Errorf("postgres: cursor column is not approved")
	}
	return nil
}

// ValidateSnapshotKey performs a full-view, fail-closed validation. A sample
// cannot establish uniqueness, so this query scans until it finds a violation
// or proves none under the caller's deadline.
func (p *postgresSource) ValidateSnapshotKey(ctx context.Context, table, keyColumn string) error {
	if p.binding == nil || table != p.binding.QualifiedRelation() || keyColumn != p.binding.KeyColumn {
		return fmt.Errorf("postgres: source relation or key is not approved")
	}
	tableIdent, err := quoteQualified(table)
	if err != nil {
		return err
	}
	keyIdent := pgx.Identifier{keyColumn}.Sanitize()
	query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s IS NULL OR %s::text = '') OR EXISTS (SELECT 1 FROM %s GROUP BY %s HAVING count(*) > 1 LIMIT 1)`, tableIdent, keyIdent, keyIdent, tableIdent, keyIdent)
	var invalid bool
	if err := p.conn.QueryRow(ctx, query).Scan(&invalid); err != nil {
		return fmt.Errorf("postgres: approved key validation failed")
	}
	if invalid {
		return fmt.Errorf("postgres: approved key is null, empty, or duplicate")
	}
	_, err = p.discoverApprovedView(ctx)
	return err
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
