package connector

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestExportViewRestrictedDiscoveryAndKeyValidation(t *testing.T) {
	adminDSN := os.Getenv("AGENTRAY_TEST_SOURCE_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set AGENTRAY_TEST_SOURCE_DATABASE_URL for restricted source integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("source database configured but unusable: %v", err)
	}
	defer admin.Close(ctx)
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	schema, role, password := "exports_"+suffix, "reader_"+suffix, "secret-"+suffix
	qi := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+qi(schema)+" CASCADE")
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+qi(role))
	}()
	stmts := []string{
		"CREATE SCHEMA " + qi(schema),
		"CREATE TABLE " + qi(schema) + `.raw_orders(id BIGINT, customer_email TEXT, amount BIGINT)`,
		"INSERT INTO " + qi(schema) + `.raw_orders VALUES (1,'pii@example.test',100),(2,'other@example.test',200)`,
		"CREATE VIEW " + qi(schema) + `.orders_v1 AS SELECT id,amount FROM ` + qi(schema) + `.raw_orders`,
		"CREATE ROLE " + qi(role) + " LOGIN PASSWORD '" + password + "'",
		"GRANT CONNECT ON DATABASE " + qi(admin.Config().Database) + " TO " + qi(role),
		"GRANT USAGE ON SCHEMA " + qi(schema) + " TO " + qi(role),
		"GRANT SELECT ON " + qi(schema) + `.orders_v1 TO ` + qi(role),
	}
	for _, stmt := range stmts {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}

	u, err := url.Parse(adminDSN)
	if err != nil || u.Scheme == "" {
		t.Fatalf("source test URL must be a PostgreSQL URL: %v", err)
	}
	u.User = url.UserPassword(role, password)
	query := u.Query()
	query.Set("statement_timeout", "0")
	query.Set("default_transaction_read_only", "off")
	u.RawQuery = query.Encode()
	restrictedDSN := u.String()
	cfg, err := pgx.ParseConfig(restrictedDSN)
	if err != nil {
		t.Fatal(err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	cidrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		bits := 128
		if ip.IP.To4() != nil {
			bits = 32
		}
		cidrs = append(cidrs, fmt.Sprintf("%s/%d", ip.IP.String(), bits))
	}
	policy := &SourcePolicy{Version: 1, Destinations: []SourceDestination{{Host: cfg.Host, Port: cfg.Port, AllowedIPCIDRs: cidrs}}, Bindings: []SourceBinding{{
		ProjectID: "p", ConnectorID: "c", Schema: schema, Relation: "orders_v1", RelationKind: RelationKindView,
		Columns: []SourcePolicyColumn{{Name: "id", PGType: "bigint"}, {Name: "amount", PGType: "bigint"}}, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique,
		AllowedCursorColumns: []string{"id"},
	}}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenWithPolicy(ctx, "postgres", restrictedDSN, "p", "c", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var readOnly, statementTimeout string
	if err := source.(*postgresSource).conn.QueryRow(ctx, `SELECT current_setting('default_transaction_read_only'),current_setting('statement_timeout')`).Scan(&readOnly, &statementTimeout); err != nil {
		t.Fatal(err)
	}
	if readOnly != "on" || statementTimeout == "0" || statementTimeout == "0ms" {
		t.Fatalf("governed session read_only=%q statement_timeout=%q", readOnly, statementTimeout)
	}
	tables, err := source.DiscoverSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0].Name != schema+".orders_v1" || len(tables[0].Columns) != 2 {
		t.Fatalf("discovery=%+v", tables)
	}
	for _, col := range tables[0].Columns {
		if col.Name == "customer_email" {
			t.Fatal("raw PII column leaked")
		}
	}
	validator := source.(interface {
		ValidateSnapshotKey(context.Context, string, string) error
	})
	if err := validator.ValidateSnapshotKey(ctx, schema+".orders_v1", "id"); err != nil {
		t.Fatal(err)
	}
	pull, err := source.PullRows(ctx, PullRequest{Table: schema + ".orders_v1", KeyColumn: "id", Snapshot: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Rows) != 2 {
		t.Fatalf("rows=%d", len(pull.Rows))
	}
	if _, ok := pull.Rows[0].Data["customer_email"]; ok {
		t.Fatal("unapproved data escaped")
	}
	if _, err := admin.Exec(ctx, "CREATE OR REPLACE VIEW "+qi(schema)+`.orders_v1 AS SELECT id,amount,customer_email FROM `+qi(schema)+`.raw_orders`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DiscoverSchema(ctx); err == nil {
		t.Fatal("unapproved extra view column accepted")
	}
	if _, err := admin.Exec(ctx, "INSERT INTO "+qi(schema)+`.raw_orders VALUES (2,'duplicate@example.test',999)`); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateSnapshotKey(ctx, schema+".orders_v1", "id"); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestRepairG2GovernedIncrementalRejectsActualRelationDrift(t *testing.T) {
	ctx := context.Background()
	admin, restrictedDSN, policy, schema := repairPostgresSource(t, []string{
		`CREATE TABLE %s.orders_v1(id text, updated_at bigint)`,
		`INSERT INTO %s.orders_v1 VALUES ('duplicate',1),('duplicate',2)`,
	}, SourceBinding{
		Relation: "orders_v1", RelationKind: RelationKindView,
		Columns:   []SourcePolicyColumn{{Name: "id", PGType: "bigint"}, {Name: "updated_at", PGType: "bigint"}},
		KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique, AllowedCursorColumns: []string{"updated_at"},
	})
	_ = admin
	source, err := OpenWithPolicy(ctx, "postgres", restrictedDSN, "p", "c", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	_, err = source.PullRows(ctx, PullRequest{Table: schema + ".orders_v1", KeyColumn: "id", CursorColumn: "updated_at", Limit: 10})
	if err == nil {
		t.Fatal("governed incremental execution accepted a base table/text key that policy declares as a bigint export view")
	}
}

func TestRepairR3GovernedIncrementalRejectsDuplicateKey(t *testing.T) {
	ctx := context.Background()
	_, dsn, policy, schema := repairPostgresSource(t, []string{
		`CREATE TABLE %s.raw_rows(id bigint, updated_at bigint)`,
		`INSERT INTO %s.raw_rows VALUES (1,1),(1,2)`,
		`CREATE VIEW %s.rows_v1 AS SELECT id,updated_at FROM %s.raw_rows`,
	}, SourceBinding{
		Relation: "rows_v1", RelationKind: RelationKindView,
		Columns:   []SourcePolicyColumn{{Name: "id", PGType: "bigint"}, {Name: "updated_at", PGType: "bigint"}},
		KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique, AllowedCursorColumns: []string{"updated_at"},
	})
	source, err := OpenWithPolicy(ctx, "postgres", dsn, "p", "c", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if pull, err := source.PullRows(ctx, PullRequest{Table: schema + ".rows_v1", KeyColumn: "id", CursorColumn: "updated_at", Limit: 10}); err == nil {
		t.Fatalf("invalid unique-key contract accepted: rows=%+v", pull.Rows)
	}
}

func TestRepairR4FinalValidationRechecksSchema(t *testing.T) {
	ctx := context.Background()
	admin, dsn, policy, schema := repairPostgresSource(t, []string{
		`CREATE TABLE %s.raw_rows(id bigint)`,
		`INSERT INTO %s.raw_rows VALUES (1)`,
		`CREATE VIEW %s.rows_v1 AS SELECT id FROM %s.raw_rows`,
	}, SourceBinding{
		Relation: "rows_v1", RelationKind: RelationKindView,
		Columns:   []SourcePolicyColumn{{Name: "id", PGType: "bigint"}},
		KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique,
	})
	source, err := OpenWithPolicy(ctx, "postgres", dsn, "p", "c", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	validator := source.(interface {
		ValidateSnapshotKey(context.Context, string, string) error
	})
	if err := validator.ValidateSnapshotKey(ctx, schema+".rows_v1", "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE OR REPLACE VIEW %s.rows_v1 AS SELECT id,'private'::text AS extra FROM %s.raw_rows`, schema, schema)); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateSnapshotKey(ctx, schema+".rows_v1", "id"); err == nil {
		t.Fatal("final validation accepted schema drift after initial validation")
	}
}

func TestRepairG6NumericSnapshotKeyRoundTrips(t *testing.T) {
	ctx := context.Background()
	_, restrictedDSN, policy, schema := repairPostgresSource(t, []string{
		`CREATE TABLE %s.raw_rows(id numeric)`,
		`INSERT INTO %s.raw_rows VALUES (1),(2)`,
		`CREATE VIEW %s.rows_v1 AS SELECT id FROM %s.raw_rows`,
	}, SourceBinding{
		Relation: "rows_v1", RelationKind: RelationKindView,
		Columns: []SourcePolicyColumn{{Name: "id", PGType: "numeric"}}, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique,
	})
	source, err := OpenWithPolicy(ctx, "postgres", restrictedDSN, "p", "c", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, err := source.PullRows(ctx, PullRequest{Table: schema + ".rows_v1", KeyColumn: "id", Snapshot: true, Limit: 1})
	if err != nil || len(first.Rows) != 1 {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	if first.NextCursorKey != "1" {
		t.Fatalf("numeric checkpoint=%q, want round-trippable 1", first.NextCursorKey)
	}
	second, err := source.PullRows(ctx, PullRequest{Table: schema + ".rows_v1", KeyColumn: "id", CursorKey: first.NextCursorKey, Snapshot: true, Limit: 1})
	if err != nil || len(second.Rows) != 1 || second.Rows[0].Key != "2" {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
}

func repairPostgresSource(t *testing.T, statements []string, binding SourceBinding) (*pgx.Conn, string, *SourcePolicy, string) {
	t.Helper()
	adminDSN := os.Getenv("AGENTRAY_TEST_SOURCE_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	}
	if adminDSN == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for source integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("source database configured but unusable: %v", err)
	}
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	schema, role, password := "repair_"+suffix, "reader_"+suffix, "secret-"+suffix
	qi := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+qi(schema)+" CASCADE")
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+qi(role))
		admin.Close(context.Background())
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+qi(schema)); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range statements {
		stmt = strings.ReplaceAll(stmt, "%s", qi(schema))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		"CREATE ROLE " + qi(role) + " LOGIN PASSWORD '" + password + "'",
		"GRANT CONNECT ON DATABASE " + qi(admin.Config().Database) + " TO " + qi(role),
		"GRANT USAGE ON SCHEMA " + qi(schema) + " TO " + qi(role),
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + qi(schema) + " TO " + qi(role),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	restrictedDSN := u.String()
	cfg, err := pgx.ParseConfig(restrictedDSN)
	if err != nil {
		t.Fatal(err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	cidrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		bits := 128
		if ip.IP.To4() != nil {
			bits = 32
		}
		cidrs = append(cidrs, fmt.Sprintf("%s/%d", ip.IP.String(), bits))
	}
	binding.ProjectID, binding.ConnectorID, binding.Schema = "p", "c", schema
	policy := &SourcePolicy{Version: 1, Destinations: []SourceDestination{{Host: cfg.Host, Port: cfg.Port, AllowedIPCIDRs: cidrs}}, Bindings: []SourceBinding{binding}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	return admin, restrictedDSN, policy, schema
}
