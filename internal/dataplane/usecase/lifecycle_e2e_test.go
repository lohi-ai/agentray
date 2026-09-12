package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// End-to-end lifecycle QA: drives the REAL operation registry (the same
// definitions REST /api/op, MCP, CLI, and the in-process agent all share)
// against a live Postgres + DuckDB. This is the adapter-neutral proof
// that the slice-2 contract works through opcore.Operation -> usecase.Repo ->
// storage, not just at the store layer. Skips without a reachable database.

func openE2EStore(t *testing.T) *storage.Store {
	t.Helper()
	pgURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if pgURL == "" {
		pgURL = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, config.Config{
		PostgresURL:          pgURL,
		DuckDBPath:           filepath.Join(t.TempDir(), "e2e.duckdb"),
		DefaultProjectName:   "e2e-default",
		DefaultProjectAPIKey: "e2e_default_key",
	})
	if err != nil {
		t.Skipf("no test database (%v) — set AGENTRAY_TEST_DATABASE_URL", err)
	}
	t.Cleanup(s.Close)
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// invoke runs one registered operation the way every adapter does: raw JSON
// args in, JSON result or error out.
func invoke(t *testing.T, reg *opcore.Registry, cc opcore.CallContext, name, args string) map[string]any {
	t.Helper()
	spec, ok := reg.Get(name)
	if !ok {
		t.Fatalf("operation %q not registered", name)
	}
	out, err := spec.OpInvoke(context.Background(), cc, args)
	if err != nil {
		t.Fatalf("%s(%s): %v", name, args, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%s: result not JSON: %v", name, err)
	}
	return m
}

func invokeErr(t *testing.T, reg *opcore.Registry, cc opcore.CallContext, name, args string) error {
	t.Helper()
	spec, ok := reg.Get(name)
	if !ok {
		t.Fatalf("operation %q not registered", name)
	}
	_, err := spec.OpInvoke(context.Background(), cc, args)
	if err == nil {
		t.Fatalf("%s(%s): expected error, got success", name, args)
	}
	return err
}

func num(m map[string]any, k string) float64 {
	v, _ := m[k].(float64)
	return v
}

// TestLifecycleOpsEndToEnd is the primary journey: dashboard + chart
// authoring through the shared registry with revision fences and reversible
// archive, then the source archive/resume contract — plus the non-happy
// paths (stale revision, idempotency conflict, missing args, cross-project).
func TestLifecycleOpsEndToEnd(t *testing.T) {
	s := openE2EStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "lifecycle-e2e-secret")
	ctx := context.Background()
	reg := Registry()

	acct, err := s.CreateAccount(ctx, fmt.Sprintf("e2e-%d@test.local", time.Now().UnixNano()), "E2E", "password-1234", "e2e-ws", "e2e-proj")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	cc := opcore.CallContext{ProjectID: acct.Project.ID, Deps: &Deps{Repo: s}}

	// --- Primary journey: board authoring through the registry ---
	dash := invoke(t, reg, cc, "create_dashboard", `{"name":"QA board"}`)
	dashID, _ := dash["id"].(string)
	if dashID == "" || num(dash, "revision") != 1 {
		t.Fatalf("create_dashboard = %v", dash)
	}
	c1 := invoke(t, reg, cc, "create_chart", fmt.Sprintf(`{"dashboard_id":%q,"name":"one","metric":"events"}`, dashID))
	c2 := invoke(t, reg, cc, "create_chart", fmt.Sprintf(`{"dashboard_id":%q,"name":"two","metric":"users"}`, dashID))
	c1ID, _ := c1["id"].(string)
	c2ID, _ := c2["id"].(string)

	listed := invoke(t, reg, cc, "list_charts", fmt.Sprintf(`{"dashboard_id":%q}`, dashID))
	charts, _ := listed["charts"].([]any)
	if len(charts) != 2 {
		t.Fatalf("list_charts = %v", listed)
	}

	// update_chart under the chart revision fence.
	upd := invoke(t, reg, cc, "update_chart", fmt.Sprintf(`{"chart_id":%q,"name":"one-renamed","kind":"bar","metric":"events","revision":1,"idempotency_key":"uq1"}`, c1ID))
	if num(upd, "revision") != 2 {
		t.Fatalf("update_chart = %v", upd)
	}
	// Identical replay returns the receipt — no second bump.
	replay := invoke(t, reg, cc, "update_chart", fmt.Sprintf(`{"chart_id":%q,"name":"one-renamed","kind":"bar","metric":"events","revision":1,"idempotency_key":"uq1"}`, c1ID))
	if num(replay, "revision") != 2 {
		t.Fatalf("update_chart replay = %v", replay)
	}

	// reorder_charts under the DASHBOARD revision fence.
	re := invoke(t, reg, cc, "reorder_charts", fmt.Sprintf(`{"dashboard_id":%q,"chart_ids":[%q,%q],"revision":1,"idempotency_key":"ro1"}`, dashID, c2ID, c1ID))
	if num(re, "revision") != 2 {
		t.Fatalf("reorder_charts = %v", re)
	}
	listed = invoke(t, reg, cc, "list_charts", fmt.Sprintf(`{"dashboard_id":%q}`, dashID))
	charts, _ = listed["charts"].([]any)
	first, _ := charts[0].(map[string]any)
	if first["id"] != c2ID {
		t.Fatalf("reorder did not apply: %v", listed)
	}

	// archive/unarchive chart — reversible, row kept.
	arch := invoke(t, reg, cc, "archive_chart", fmt.Sprintf(`{"chart_id":%q,"revision":2,"idempotency_key":"ac1"}`, c1ID))
	if arch["archived_at"] == nil {
		t.Fatalf("archive_chart = %v", arch)
	}
	listed = invoke(t, reg, cc, "list_charts", fmt.Sprintf(`{"dashboard_id":%q}`, dashID))
	if len(listed["charts"].([]any)) != 1 {
		t.Fatalf("archived chart still listed: %v", listed)
	}
	un := invoke(t, reg, cc, "unarchive_chart", fmt.Sprintf(`{"chart_id":%q,"revision":3,"idempotency_key":"uc1"}`, c1ID))
	if un["archived_at"] != nil {
		t.Fatalf("unarchive_chart = %v", un)
	}

	// --- Non-happy paths ---
	if err := invokeErr(t, reg, cc, "update_chart", fmt.Sprintf(`{"chart_id":%q,"name":"stale","revision":1}`, c1ID)); !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("stale revision err = %v, want revision conflict", err)
	}
	if err := invokeErr(t, reg, cc, "update_chart", fmt.Sprintf(`{"chart_id":%q,"name":"other","revision":1,"idempotency_key":"uq1"}`, c1ID)); !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("key reuse err = %v, want idempotency conflict", err)
	}
	if err := invokeErr(t, reg, cc, "update_chart", `{"name":"no-id"}`); !strings.Contains(err.Error(), "chart_id") {
		t.Fatalf("missing arg err = %v, want required-field error", err)
	}
	if err := invokeErr(t, reg, cc, "reorder_charts", fmt.Sprintf(`{"dashboard_id":%q,"chart_ids":[%q],"revision":1}`, dashID, c1ID)); !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("stale reorder err = %v, want revision conflict", err)
	}

	// Cross-project isolation: a second project's context cannot touch the
	// first project's chart.
	acct2, err := s.CreateAccount(ctx, fmt.Sprintf("e2e-b-%d@test.local", time.Now().UnixNano()), "E2E B", "password-1234", "e2e-ws-b", "e2e-proj-b")
	if err != nil {
		t.Fatalf("seed account B: %v", err)
	}
	ccB := opcore.CallContext{ProjectID: acct2.Project.ID, Deps: &Deps{Repo: s}}
	if err := invokeErr(t, reg, ccB, "archive_chart", fmt.Sprintf(`{"chart_id":%q,"revision":4}`, c1ID)); err == nil {
		t.Fatal("foreign project archived the chart")
	}

	// --- Source journey: credential -> create -> archive -> unarchive ---
	cred, err := s.CreateSourceCredential(ctx, acct.User.ID, acct.Project.ID, "e2e-pg", "postgres://u:p@h:5432/db")
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	src := invoke(t, reg, cc, "create_source", fmt.Sprintf(`{"name":"warehouse","kind":"postgres","credential_id":%q,"idempotency_key":"cs1"}`, cred.ID))
	srcID, _ := src["id"].(string)
	if srcID == "" || src["has_dsn"] != true {
		t.Fatalf("create_source = %v", src)
	}
	// A revoked/foreign credential cannot back a source.
	if err := invokeErr(t, reg, cc, "create_source", `{"name":"x","kind":"postgres","credential_id":"00000000-0000-0000-0000-000000000000"}`); !strings.Contains(err.Error(), "credential") {
		t.Fatalf("bad credential err = %v", err)
	}
	syncRow, err := s.CreateConnectorSync(ctx, acct.User.ID, acct.Project.ID, srcID, storage.ConnectorSyncInput{
		SourceTable: "orders", KeyColumn: "id", ScheduleCron: "*/5 * * * *", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	archSrc := invoke(t, reg, cc, "archive_source", fmt.Sprintf(`{"connector_id":%q,"revision":1,"idempotency_key":"as1"}`, srcID))
	if archSrc["archived_at"] == nil {
		t.Fatalf("archive_source = %v", archSrc)
	}
	// The sync was paused transactionally by the archive.
	st := invoke(t, reg, cc, "source_status", fmt.Sprintf(`{"sync_id":%q}`, syncRow.ID))
	syncs, _ := st["syncs"].([]any)
	syncMap, _ := syncs[0].(map[string]any)["sync"].(map[string]any)
	if syncMap["enabled"] != false || syncMap["disabled_by_archive"] != true {
		t.Fatalf("sync after archive = %v", syncMap)
	}
	// Resuming under an archived source is rejected.
	if err := invokeErr(t, reg, cc, "pause_source", fmt.Sprintf(`{"sync_id":%q,"paused":false,"revision":%d}`, syncRow.ID, int64(num(syncMap, "revision")))); !strings.Contains(err.Error(), "archived") {
		t.Fatalf("resume under archived source err = %v, want archived", err)
	}
	unSrc := invoke(t, reg, cc, "unarchive_source", fmt.Sprintf(`{"connector_id":%q,"revision":2,"idempotency_key":"us1"}`, srcID))
	if unSrc["archived_at"] != nil {
		t.Fatalf("unarchive_source = %v", unSrc)
	}
	st = invoke(t, reg, cc, "source_status", fmt.Sprintf(`{"sync_id":%q}`, syncRow.ID))
	syncs, _ = st["syncs"].([]any)
	syncMap, _ = syncs[0].(map[string]any)["sync"].(map[string]any)
	if syncMap["enabled"] != true || syncMap["disabled_by_archive"] != false {
		t.Fatalf("sync after unarchive = %v", syncMap)
	}
	// list_sources reflects the archive state.
	lst := invoke(t, reg, cc, "list_sources", `{"include_archived":true}`)
	if len(lst["sources"].([]any)) != 1 {
		t.Fatalf("list_sources = %v", lst)
	}
}
