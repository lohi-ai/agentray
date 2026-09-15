package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// boards_e2e_test.go — the declarative board content model driven through the
// real operation registry (the same definitions REST /api/op, MCP, the CLI and
// the in-process agent share), against a live Postgres + DuckDB, with real
// events. It is the proof that "add a graph by declaration" produces a graph
// whose data can actually be read — and that the catalog refuses to describe a
// number nobody computed. Skips without a reachable database.

func seedBoardEvents(t *testing.T, s *storage.Store, projectID string, at time.Time) {
	t.Helper()
	events := []storage.Event{}
	for i, who := range []string{"reader-a", "reader-b", "reader-c"} {
		events = append(events, storage.Event{
			ProjectID:  projectID,
			EventID:    fmt.Sprintf("%s-0000-0000-0000-00000000000%d", projectID[:8], i),
			DistinctID: who,
			SessionID:  "session-" + who,
			EventName:  "user.pageview",
			EventType:  "user",
			Properties: `{"path":"/"}`,
			Timestamp:  at.UTC(),
			// A human product event: the qualification every people metric
			// requires, so a seeded event is counted rather than excluded.
			VisitorClass: "human",
			Platform:     storage.PlatformWeb,
		})
	}
	if err := s.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("seed events: %v", err)
	}
}

func TestBoardContentModelEndToEnd(t *testing.T) {
	s := openE2EStore(t)
	ctx := context.Background()
	reg := Registry()

	acct, err := s.CreateAccount(ctx, fmt.Sprintf("board-%d@test.local", time.Now().UnixNano()), "Board E2E", "password-1234", "board-ws", "board-proj")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	cc := opcore.CallContext{ProjectID: acct.Project.ID, Deps: &Deps{Repo: s}}

	// --- The catalog is the vocabulary a declaration is written against ---
	catalog := invoke(t, reg, cc, "list_metrics", `{}`)
	if catalog["metric_version"] != storage.OverviewMetricVersion {
		t.Fatalf("list_metrics metric_version = %v, want %s", catalog["metric_version"], storage.OverviewMetricVersion)
	}
	metrics, _ := catalog["metrics"].([]any)
	if len(metrics) != len(storage.MetricCatalog()) {
		t.Fatalf("list_metrics returned %d metrics, catalog declares %d", len(metrics), len(storage.MetricCatalog()))
	}
	catalogByKey := map[string]map[string]any{}
	for _, m := range metrics {
		row, _ := m.(map[string]any)
		catalogByKey[fmt.Sprint(row["key"])] = row
	}
	active := catalogByKey[storage.MetricActiveUsers]
	if active["unit"] != "people" || active["kind"] != storage.MetricKindValue || active["definition"] == "" {
		t.Fatalf("active_users catalog row = %v", active)
	}
	if _, ok := catalogByKey["crashes"]; ok {
		t.Fatal("the catalog advertises a crash metric no implementation computes")
	}

	// --- A project with no events reports no_data, never a zero ---
	reading := invoke(t, reg, cc, "read_metric", fmt.Sprintf(`{"metric":%q}`, storage.MetricActiveUsers))
	if reading["state"] != "no_data" {
		t.Fatalf("empty project active_users = %v, want no_data", reading)
	}
	if _, ok := reading["value"]; ok {
		t.Fatalf("a metric with no events served a value: %v", reading)
	}
	if fmt.Sprint(reading["definition"]) != fmt.Sprint(active["definition"]) {
		t.Fatalf("the reading and the catalog disagree: %v vs %v", reading["definition"], active["definition"])
	}

	// --- Real events, so the declared graph has something to read ---
	seedBoardEvents(t, s, acct.Project.ID, time.Now().UTC().Add(-25*time.Hour))

	// --- Declare a board: two metric tiles in one write ---
	declaration := fmt.Sprintf(`{
		"board_key":"app-overview",
		"name":"App overview",
		"definition":{"version":1,"sections":[
			{"key":"overview","title":"Overview","tiles":[
				{"key":"people","metric":%q,"display":"stat"},
			{"key":"people-trend","metric":%q,"display":"line","span":2,"params":{"period":"7d"}}
		]}
	]}}`,
		storage.MetricActiveUsers, storage.MetricActiveUsersDaily)
	board := invoke(t, reg, cc, "save_board", declaration)
	if board["has_definition"] != true {
		t.Fatalf("save_board = %v", board)
	}
	boardRow, _ := board["board"].(map[string]any)
	if boardRow["board_key"] != "app-overview" || num(boardRow, "revision") != 1 {
		t.Fatalf("declared board row = %v", boardRow)
	}
	resolved, _ := board["metrics"].([]any)
	if len(resolved) != 2 {
		t.Fatalf("resolved metrics = %v", board["metrics"])
	}
	if warnings, _ := board["warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("a fresh declaration warned: %v", warnings)
	}
	boardID := fmt.Sprint(boardRow["id"])

	// --- The declared graph reads its data ---
	reading = invoke(t, reg, cc, "read_metric", fmt.Sprintf(`{"metric":%q,"period":"7d"}`, storage.MetricActiveUsers))
	if reading["state"] != "ok" || num(reading, "value") != 3 {
		t.Fatalf("declared tile read = %v, want ok with 3 people", reading)
	}
	trend := invoke(t, reg, cc, "read_metric", fmt.Sprintf(`{"metric":%q,"period":"7d"}`, storage.MetricActiveUsersDaily))
	points, _ := trend["series"].([]any)
	if trend["state"] != "ok" || len(points) != 7 {
		t.Fatalf("declared trend read = %v, want seven daily points", trend)
	}
	// The platform filter the tile declared narrows the read the same way it
	// narrows the overview.
	webOnly := invoke(t, reg, cc, "read_metric", fmt.Sprintf(`{"metric":%q,"platform":"ios"}`, storage.MetricActiveUsers))
	if webOnly["state"] != "no_data" {
		t.Fatalf("ios-only read of web traffic = %v, want no_data", webOnly)
	}

	// --- Reading the board back, by key and by id ---
	byKey := invoke(t, reg, cc, "get_board", `{"board_key":"app-overview"}`)
	byKeyRow, _ := byKey["board"].(map[string]any)
	if fmt.Sprint(byKeyRow["id"]) != boardID || num(byKeyRow, "revision") != 1 {
		t.Fatalf("get_board by key = %v", byKeyRow)
	}
	byID := invoke(t, reg, cc, "get_board", fmt.Sprintf(`{"board_id":%q}`, boardID))
	byIDRow, _ := byID["board"].(map[string]any)
	if fmt.Sprint(byIDRow["id"]) != boardID {
		t.Fatalf("get_board by id = %v", byIDRow)
	}
	// The board is addressable from the board list the menu reads.
	listed := invoke(t, reg, cc, "list_dashboards", `{}`)
	foundKey := false
	for _, d := range listed["dashboards"].([]any) {
		if row, _ := d.(map[string]any); fmt.Sprint(row["board_key"]) == "app-overview" {
			foundKey = true
		}
	}
	if !foundKey {
		t.Fatalf("list_dashboards lost the board key: %v", listed)
	}

	// --- Extending the board is a declaration with the current revision ---
	extended := strings.Replace(declaration,
		`"name":"App overview"`,
		fmt.Sprintf(`"board_id":%q,"revision":1`, boardID), 1)
	extended = strings.Replace(extended, `"board_key":"app-overview",`, ``, 1)
	extended = strings.Replace(extended,
		`{"key":"people-trend"`,
		`{"key":"revenue","metric":"revenue","display":"stat"},{"key":"people-trend"`, 1)
	redeclared := invoke(t, reg, cc, "save_board", extended)
	redeclaredRow, _ := redeclared["board"].(map[string]any)
	if num(redeclaredRow, "revision") != 2 {
		t.Fatalf("redeclare = %v", redeclaredRow)
	}
	// Revenue is unconfigured until a trusted billing source exists: the tile
	// is declared, and the read says what is missing instead of drawing a zero.
	revenue := invoke(t, reg, cc, "read_metric", fmt.Sprintf(`{"metric":%q}`, storage.MetricRevenue))
	if revenue["state"] != "unconfigured" || fmt.Sprint(revenue["prerequisite"]) == "" {
		t.Fatalf("revenue read = %v, want unconfigured with a named prerequisite", revenue)
	}

	// --- Non-happy paths ---
	if err := invokeErr(t, reg, cc, "save_board", `{"board_key":"no-definition"}`); !strings.Contains(err.Error(), "definition") {
		t.Fatalf("missing definition err = %v, want a required-field error", err)
	}
	if err := invokeErr(t, reg, cc, "save_board", fmt.Sprintf(`{"board_id":%q,"definition":{"version":1,"sections":[]}}`, boardID)); !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("unfenced declaration err = %v, want a revision conflict", err)
	}
	if err := invokeErr(t, reg, cc, "save_board", fmt.Sprintf(`{"board_id":%q,"revision":1,"definition":{"version":1,"sections":[{"key":"s","title":"S","tiles":[{"key":"t","metric":"crashes","display":"stat"}]}]}}`, boardID)); !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("unknown metric err = %v, want a catalog refusal", err)
	}
	if err := invokeErr(t, reg, cc, "get_board", `{}`); !strings.Contains(err.Error(), "board_id or board_key") {
		t.Fatalf("unaddressed read err = %v, want an addressed-board error", err)
	}
	if err := invokeErr(t, reg, cc, "read_metric", `{"metric":"crashes"}`); !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("unknown metric read err = %v, want a catalog refusal", err)
	}

	// --- Access classes are the split the credential model claims ---
	capture := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredCapture}
	if reg.Authorize(capture, "save_board") || reg.Authorize(capture, "get_board") {
		t.Fatal("a capture credential reached the board surface")
	}
	reader := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredManagement, Grants: []opcore.Access{opcore.AccessAnalyticsRead}}
	if !reg.Authorize(reader, "get_board") || !reg.Authorize(reader, "read_metric") || !reg.Authorize(reader, "list_metrics") {
		t.Fatal("an analytics:read credential cannot read the board surface")
	}
	if reg.Authorize(reader, "save_board") {
		t.Fatal("an analytics:read credential declared a board")
	}

	// --- Project scope ---
	acct2, err := s.CreateAccount(ctx, fmt.Sprintf("board-b-%d@test.local", time.Now().UnixNano()), "Board E2E B", "password-1234", "board-ws-b", "board-proj-b")
	if err != nil {
		t.Fatalf("seed account B: %v", err)
	}
	ccB := opcore.CallContext{ProjectID: acct2.Project.ID, Deps: &Deps{Repo: s}}
	if err := invokeErr(t, reg, ccB, "get_board", fmt.Sprintf(`{"board_id":%q}`, boardID)); err == nil {
		t.Fatal("a foreign project read the board")
	}
	if err := invokeErr(t, reg, ccB, "save_board", fmt.Sprintf(`{"board_id":%q,"revision":2,"definition":{"version":1,"sections":[]}}`, boardID)); err == nil {
		t.Fatal("a foreign project declared content onto the board")
	}
}

// TestMetricTargetsEndToEnd drives the target contract through the shared
// registry: declare a target, read the metric, and the verdict the overview
// computed rides the reading — on_track, at_risk, off_track, and the named
// reasons a reading cannot be judged.
func TestMetricTargetsEndToEnd(t *testing.T) {
	s := openE2EStore(t)
	ctx := context.Background()
	reg := Registry()

	acct, err := s.CreateAccount(ctx, fmt.Sprintf("targets-%d@test.local", time.Now().UnixNano()), "Targets E2E", "password-1234", "targets-ws", "targets-proj")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	cc := opcore.CallContext{ProjectID: acct.Project.ID, Deps: &Deps{Repo: s}}
	// Three readers inside the previous complete local day — the "1d" window —
	// plus one today, so the partial window is a measured reading rather than
	// no_data (which serves no target at all).
	loc := time.UTC
	localNow := time.Now().In(loc)
	midnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	seedBoardEvents(t, s, acct.Project.ID, midnight.Add(-12*time.Hour))
	if err := s.InsertEvents(ctx, []storage.Event{{
		ProjectID: acct.Project.ID, EventID: fmt.Sprintf("%s-0000-0000-0000-000000000099", acct.Project.ID[:8]),
		DistinctID: "reader-today", SessionID: "session-today", EventName: "user.pageview", EventType: "user",
		Properties: `{"path":"/today"}`, Timestamp: midnight.Add(time.Hour).UTC(),
		VisitorClass: "human", Platform: storage.PlatformWeb,
	}}); err != nil {
		t.Fatalf("seed today event: %v", err)
	}

	// A target only judges windows that end after it took force, so the test
	// declares it effective before the window's end.
	effective := midnight.Add(-48 * time.Hour).Format(time.RFC3339)
	set := invoke(t, reg, cc, "set_metric_target",
		fmt.Sprintf(`{"metric":"active_users","direction":"gte","value":2,"period":"1d","effective_at":%q,"idempotency_key":"t-1"}`, effective))
	if num(set, "version") != 1 || set["cleared"] != false {
		t.Fatalf("set_metric_target = %v, want v1", set)
	}

	// 3 measured against ≥ 2: on track, citing v1.
	reading := invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"1d"}`)
	target, _ := reading["target"].(map[string]any)
	if reading["state"] != "ok" || num(reading, "value") != 3 {
		t.Fatalf("reading = %v, want ok 3", reading)
	}
	if target == nil || target["verdict"] != "on_track" || num(target, "version") != 1 || target["label"] != "≥ 2 daily" {
		t.Fatalf("target = %v, want on_track v1 \"≥ 2 daily\"", target)
	}

	// Raising the target appends v2; 3 of ≥ 3.3 sits inside the 10% band.
	invoke(t, reg, cc, "set_metric_target",
		fmt.Sprintf(`{"metric":"active_users","direction":"gte","value":3.3,"period":"1d","effective_at":%q}`, effective))
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"1d"}`)
	target, _ = reading["target"].(map[string]any)
	if target == nil || target["verdict"] != "at_risk" || num(target, "version") != 2 {
		t.Fatalf("at-risk read = %v, want at_risk v2", target)
	}

	// Well past the band: off track.
	invoke(t, reg, cc, "set_metric_target",
		fmt.Sprintf(`{"metric":"active_users","direction":"gte","value":10,"period":"1d","effective_at":%q}`, effective))
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"1d"}`)
	target, _ = reading["target"].(map[string]any)
	if target == nil || target["verdict"] != "off_track" || num(target, "version") != 3 {
		t.Fatalf("off-track read = %v, want off_track v3", target)
	}

	// A window the target's period does not match, and the partial "today",
	// carry the target with a named reason instead of a verdict.
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"7d"}`)
	target, _ = reading["target"].(map[string]any)
	if target == nil || target["verdict"] != nil || target["verdict_reason"] != "period_mismatch" {
		t.Fatalf("period-mismatched read = %v, want period_mismatch", target)
	}
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"today"}`)
	target, _ = reading["target"].(map[string]any)
	if target == nil || target["verdict"] != nil || target["verdict_reason"] != "partial_window" {
		t.Fatalf("partial read = %v, want partial_window", target)
	}

	// Clearing is a version, and it obeys the same in-force rule: effective
	// before the window's end, the window cites the tombstone and serves no
	// target. (A clear effective now would still leave v3 in force for the
	// complete window that already ended — history is not rewritten.)
	clearedAt := midnight.Add(-47 * time.Hour).Format(time.RFC3339)
	invoke(t, reg, cc, "set_metric_target", fmt.Sprintf(`{"metric":"active_users","clear":true,"effective_at":%q}`, clearedAt))
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"1d"}`)
	if _, present := reading["target"]; present {
		t.Fatalf("cleared read still serves a target: %v", reading["target"])
	}

	// A board tile can declare the same target — and get_board serves the
	// version the declaration wrote (v5, effective at write time).
	invoke(t, reg, cc, "save_board", `{"board_key":"targeted-board","name":"Targeted","definition":{"version":1,"sections":[{"key":"s","title":"S","tiles":[{"key":"people","metric":"active_users","display":"stat","target":{"direction":"gte","value":2,"period":"1d"}}]}]}}`)
	board := invoke(t, reg, cc, "get_board", `{"board_key":"targeted-board"}`)
	targets, _ := board["targets"].(map[string]any)
	declared, _ := targets["active_users"].(map[string]any)
	if declared == nil || num(declared, "version") != 5 || num(declared, "value") != 2 {
		t.Fatalf("board targets = %v, want active_users v5 = 2", targets)
	}
	// v5 took force at write time — after the window ended — so the read still
	// cites the tombstone: no target for a window that predates it.
	reading = invoke(t, reg, cc, "read_metric", `{"metric":"active_users","period":"1d"}`)
	if _, present := reading["target"]; present {
		t.Fatalf("a future-effective target judged an earlier window: %v", reading["target"])
	}

	// --- Non-happy paths ---
	if err := invokeErr(t, reg, cc, "set_metric_target", `{"metric":"crashes","direction":"gte","value":1,"period":"7d"}`); !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("unknown metric err = %v, want a catalog refusal", err)
	}
	if err := invokeErr(t, reg, cc, "set_metric_target", `{"metric":"revenue","direction":"gte","value":100,"period":"7d"}`); !strings.Contains(err.Error(), "currency") {
		t.Fatalf("currency-less revenue target err = %v, want a currency refusal", err)
	}
	if err := invokeErr(t, reg, cc, "set_metric_target", `{"metric":"active_users","direction":"gte","value":1,"period":"today"}`); !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial-period target err = %v, want a period refusal", err)
	}

	// --- Access classes: the write is dashboards:write, member-only ---
	reader := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredManagement, Grants: []opcore.Access{opcore.AccessAnalyticsRead}}
	if reg.Authorize(reader, "set_metric_target") {
		t.Fatal("an analytics:read credential set a metric target")
	}
	capture := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredCapture}
	if reg.Authorize(capture, "set_metric_target") {
		t.Fatal("a capture credential reached set_metric_target")
	}
}

// TestSaveBoardSchemaTeachesTheDocument: the advertised schema is what a model
// composes from, so the definition field must describe the document rather than
// saying "object".
func TestSaveBoardSchemaTeachesTheDocument(t *testing.T) {
	spec, ok := Registry().Get("save_board")
	if !ok {
		t.Fatal("save_board is not registered")
	}
	schema := spec.OpSchema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "definition" {
		t.Fatalf("save_board required = %v, want [definition]", schema["required"])
	}
	props, _ := schema["properties"].(map[string]any)
	definition, _ := props["definition"].(map[string]any)
	if definition["type"] != "object" {
		t.Fatalf("definition schema = %v", definition)
	}
	desc, _ := definition["description"].(string)
	for _, want := range []string{"sections", "tiles", "kind", "metric", "chart_id", "display", "span"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the definition description does not mention %q: %s", want, desc)
		}
	}
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema is not serializable for MCP: %v", err)
	}
}
