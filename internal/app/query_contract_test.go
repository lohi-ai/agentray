package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

func seedAppQueryFixture(t *testing.T, s *storage.Store, projectID, otherProjectID string) querytest.Fixture {
	t.Helper()
	fixture, err := querytest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	events := make([]storage.Event, 0, len(fixture.Events))
	for _, input := range fixture.Events {
		at, err := time.Parse(time.RFC3339Nano, input.Timestamp)
		if err != nil {
			t.Fatalf("event timestamp: %v", err)
		}
		project := projectID
		if input.ProjectID == fixture.OtherProjectID {
			project = otherProjectID
		}
		events = append(events, storage.Event{
			ProjectID: project, EventID: input.EventID, DistinctID: input.DistinctID,
			SessionID: "query-contract", EventName: input.EventName, EventType: "user",
			Properties: input.Properties, Timestamp: at, VisitorClass: "human",
		})
	}
	if err := s.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	for _, alias := range fixture.Aliases {
		if err := s.CreateAlias(context.Background(), projectID, alias.AnonymousID, alias.CanonicalID); err != nil {
			t.Fatalf("seed alias: %v", err)
		}
	}
	type key struct{ project, connector, table string }
	groups := map[key][]connector.LandedRow{}
	for _, row := range fixture.ExternalRows {
		project := projectID
		if row.ProjectID == fixture.OtherProjectID {
			project = otherProjectID
		}
		k := key{project, row.ConnectorID, row.TableName}
		groups[k] = append(groups[k], connector.LandedRow{Key: row.RowKey, DataJSON: row.Data})
	}
	for k, rows := range groups {
		if err := s.InsertExternalRows(context.Background(), k.project, k.connector, k.table, rows, storage.AppliedMark{}); err != nil {
			t.Fatalf("seed external rows: %v", err)
		}
	}
	return fixture
}

func rowsJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var body struct {
		Rows []map[string]any `json:"rows"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode rows: %v; body=%s", err, raw)
	}
	encoded, err := json.Marshal(body.Rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestQueryContractAdaptersAndSavedArtifacts(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("query-contract-%d@test.local", stamp), "Owner", "password-123", "ws", "query")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateAccount(ctx, fmt.Sprintf("query-contract-foreign-%d@test.local", stamp), "Other", "password-123", "ws", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	fixture := seedAppQueryFixture(t, s, owner.Project.ID, foreign.Project.ID)
	contract := fixture.Queries[1] // connector-aware revenue query

	_, reader, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, "investigator", []string{"analytics:read", "sources:read"})
	if err != nil {
		t.Fatal(err)
	}
	_, author, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, "board-author", []string{"analytics:read", "dashboards:write"})
	if err != nil {
		t.Fatal(err)
	}
	e := mountServerRoutes(t, s)
	registerOpRoutes(e, s, nil, nil)
	registerMcpRoutes(e, s, nil, nil)
	body, _ := json.Marshal(map[string]string{"sql": contract.SQL})

	wantRaw, _ := json.Marshal(map[string]any{"rows": contract.Expected})
	want := rowsJSON(t, wantRaw)
	got := map[string]string{}
	legacy := postJSON(t, e, "/api/sql/run?project_id="+owner.Project.ID, string(body), map[string]string{"Authorization": "Bearer " + reader})
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy run_sql: %d %s", legacy.Code, legacy.Body.String())
	}
	got["legacy"] = rowsJSON(t, legacy.Body.Bytes())

	for name, invoke := range map[string]opInvoker{
		"op":      restInvoker(e, reader),
		"mcp":     mcpInvoker(e, reader),
		"runtime": runtimeInvoker(usecase.Registry(), opcore.CallContext{ProjectID: owner.Project.ID, Deps: &usecase.Deps{Repo: s}}),
	} {
		out := invoke(t, "run_sql", string(body))
		if out.class != "ok" {
			t.Fatalf("%s run_sql class = %s", name, out.class)
		}
		got[name] = rowsJSON(t, out.raw)
	}

	saved, err := s.CreateSavedQuery(ctx, owner.Project.ID, "Canonical revenue", contract.SQL, true)
	if err != nil {
		t.Fatal(err)
	}
	savedRec := postJSON(t, e, "/api/saved-queries/"+saved.ID+"/run?project_id="+owner.Project.ID, `{}`, map[string]string{"Authorization": "Bearer " + reader})
	if savedRec.Code != http.StatusOK {
		t.Fatalf("saved query: %d %s", savedRec.Code, savedRec.Body.String())
	}
	var savedEnvelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(savedRec.Body.Bytes(), &savedEnvelope); err != nil {
		t.Fatal(err)
	}
	got["saved_query"] = rowsJSON(t, savedEnvelope.Result)
	for adapter, rows := range got {
		if rows != want {
			t.Errorf("%s rows differ\n got: %s\nwant: %s", adapter, rows, want)
		}
	}

	dashboardRec := postJSON(t, e, "/api/dashboards?project_id="+owner.Project.ID, `{"name":"Canonical evidence","description":"query contract"}`, map[string]string{"Authorization": "Bearer " + author})
	if dashboardRec.Code != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", dashboardRec.Code, dashboardRec.Body.String())
	}
	var dashboard struct {
		Dashboard storage.Dashboard `json:"dashboard"`
	}
	if err := json.Unmarshal(dashboardRec.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	chartInput, _ := json.Marshal(map[string]any{
		"name": "Revenue", "kind": "line", "sql": contract.SQL,
		"x_field": "date", "y_field": "value", "col_span": 2,
	})
	chartRec := postJSON(t, e, "/api/dashboards/"+dashboard.Dashboard.ID+"/charts?project_id="+owner.Project.ID, string(chartInput), map[string]string{"Authorization": "Bearer " + author})
	if chartRec.Code != http.StatusCreated {
		t.Fatalf("create chart: %d %s", chartRec.Code, chartRec.Body.String())
	}
	var chart struct {
		Chart storage.Chart `json:"chart"`
	}
	if err := json.Unmarshal(chartRec.Body.Bytes(), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Chart.SQL != contract.SQL || chart.Chart.XField != "date" || chart.Chart.YField != "value" {
		t.Fatalf("saved chart changed query contract: %+v", chart.Chart)
	}
}
