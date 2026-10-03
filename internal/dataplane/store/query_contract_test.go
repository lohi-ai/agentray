package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
)

func seedCanonicalQueryFixture(t *testing.T, d *DuckDB) querytest.Fixture {
	t.Helper()
	fixture, err := querytest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	events := make([]Event, 0, len(fixture.Events))
	for _, input := range fixture.Events {
		at, err := time.Parse(time.RFC3339Nano, input.Timestamp)
		if err != nil {
			t.Fatalf("event %s timestamp: %v", input.EventID, err)
		}
		events = append(events, Event{
			ProjectID: input.ProjectID, EventID: input.EventID, DistinctID: input.DistinctID,
			SessionID: "query-contract", EventName: input.EventName, EventType: "user",
			Properties: input.Properties, Timestamp: at, VisitorClass: "human",
		})
	}
	if err := d.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	aliases := make([][3]string, 0, len(fixture.Aliases))
	for _, alias := range fixture.Aliases {
		aliases = append(aliases, [3]string{alias.ProjectID, alias.AnonymousID, alias.CanonicalID})
	}
	if err := d.UpsertAliases(context.Background(), aliases); err != nil {
		t.Fatalf("seed aliases: %v", err)
	}
	type externalKey struct{ project, connector, table string }
	groups := map[externalKey][]connector.LandedRow{}
	for _, row := range fixture.ExternalRows {
		key := externalKey{row.ProjectID, row.ConnectorID, row.TableName}
		groups[key] = append(groups[key], connector.LandedRow{Key: row.RowKey, DataJSON: row.Data})
	}
	for key, rows := range groups {
		if err := d.InsertExternalRows(context.Background(), key.project, key.connector, key.table, rows, AppliedMark{}); err != nil {
			t.Fatalf("seed external rows %+v: %v", key, err)
		}
	}
	return fixture
}

func canonicalRows(t *testing.T, rows []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	return string(raw)
}

func TestQueryContractCanonicalRows(t *testing.T) {
	d := openTestDuckDB(t)
	fixture := seedCanonicalQueryFixture(t, d)
	pool, ctx := newTestSandboxPool(t, d, nil)

	for _, contract := range fixture.Queries {
		t.Run(contract.Name, func(t *testing.T) {
			query, args, err := scopedReadonlySQL(contract.SQL, fixture.ProjectID, nil)
			if err != nil {
				t.Fatalf("scope fixture SQL: %v", err)
			}
			rows, err := pool.query(ctx, fixture.ProjectID, query, args)
			if err != nil {
				t.Fatalf("execute fixture SQL: %v\n%s", err, query)
			}
			if got, want := canonicalRows(t, rows), canonicalRows(t, contract.Expected); got != want {
				t.Fatalf("canonical rows differ\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestQueryContractSourceNamesInCommentsAndLiteralsAreNotRewritten(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"SELECT count(*) AS n FROM events WHERE event_name = 'FROM external_rows' -- FROM external_rows\n/* JOIN external_rows */",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scope SQL: %v", err)
	}
	if strings.Count(query, "scoped_external_rows") != 0 {
		t.Fatalf("comment/literal created a phantom external source: %s", query)
	}
	if len(args) != 1 || args[0] != "project-1" {
		t.Fatalf("args = %#v, want only the events project fence", args)
	}
}

func TestQueryContractOrdinaryBackslashLiteralExecutes(t *testing.T) {
	d := openTestDuckDB(t)
	fixture := seedCanonicalQueryFixture(t, d)
	pool, ctx := newTestSandboxPool(t, d, nil)
	query, args, err := scopedReadonlySQL(`SELECT '\' AS slash FROM events LIMIT 1`, fixture.ProjectID, nil)
	if err != nil {
		t.Fatalf("scope ordinary backslash literal: %v", err)
	}
	rows, err := pool.query(ctx, fixture.ProjectID, query, args)
	if err != nil {
		t.Fatalf("execute ordinary backslash literal: %v\n%s", err, query)
	}
	if len(rows) != 1 || rows[0]["slash"] != `\` {
		t.Fatalf("ordinary backslash rows = %#v", rows)
	}
}

func TestQueryContractRecursiveCTEAndCheckedIntegerConversion(t *testing.T) {
	d := openTestDuckDB(t)
	fixture := seedCanonicalQueryFixture(t, d)
	pool, ctx := newTestSandboxPool(t, d, nil)

	recursive := `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 2),
totals AS (SELECT count(*) AS n FROM events)
SELECT max(totals.n) AS n FROM totals CROSS JOIN seq`
	query, args, err := scopedReadonlySQL(recursive, fixture.ProjectID, nil)
	if err != nil {
		t.Fatalf("scope recursive SQL: %v", err)
	}
	rows, err := pool.query(ctx, fixture.ProjectID, query, args)
	if err != nil || len(rows) != 1 || rows[0]["n"] != int64(4) {
		t.Fatalf("recursive CTE rows=%#v err=%v", rows, err)
	}

	overflow, args, err := scopedReadonlySQL(
		`SELECT CAST('9223372036854775808' AS BIGINT) AS value FROM events LIMIT 1`,
		fixture.ProjectID,
		nil,
	)
	if err != nil {
		t.Fatalf("scope overflow SQL: %v", err)
	}
	if _, err := pool.query(ctx, fixture.ProjectID, overflow, args); err == nil {
		t.Fatal("checked BIGINT overflow unexpectedly succeeded")
	}

	exact, args, err := scopedReadonlySQL(
		`SELECT 170141183460469231731687303715884105727::HUGEINT AS value FROM events LIMIT 1`,
		fixture.ProjectID,
		nil,
	)
	if err != nil {
		t.Fatalf("scope HUGEINT SQL: %v", err)
	}
	rows, err = pool.query(ctx, fixture.ProjectID, exact, args)
	if err != nil || rows[0]["value"] != "170141183460469231731687303715884105727" {
		t.Fatalf("exact HUGEINT rows=%#v err=%v", rows, err)
	}
}
