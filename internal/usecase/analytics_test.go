package usecase

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// fakeRepo records what it receives so handler behavior (SQL normalization, title
// derivation) can be asserted end-to-end through the operation.
type fakeRepo struct {
	Repo
	gotSQL          string
	gotRec          storage.AgentRecommendation
	gotInsightType  string
	gotSteps        []string
	gotInsightEvent string
}

func (f *fakeRepo) RunSQL(_ context.Context, _ string, sqlText string) ([]map[string]any, error) {
	f.gotSQL = sqlText
	return []map[string]any{{"ok": 1}}, nil
}

func (f *fakeRepo) RunInsight(_ context.Context, _, insightType, _ string, steps []string, filter storage.EventFilter) (storage.InsightResult, error) {
	f.gotInsightType = insightType
	f.gotSteps = steps
	f.gotInsightEvent = filter.EventName
	return storage.InsightResult{Type: insightType}, nil
}

// run_funnel and run_retention must delegate to RunInsight with the right type,
// so agent/MCP/REST/CLI all get funnels and retention from the one engine.
func TestFunnelRetentionOpsDelegate(t *testing.T) {
	reg := Registry()
	deps := &opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: &fakeRepo{}}}

	funnel, _ := reg.Get("run_funnel")
	repo := deps.Deps.(*Deps).Repo.(*fakeRepo)
	if _, err := funnel.OpInvoke(context.Background(), *deps, `{"steps":["a","b"]}`); err != nil {
		t.Fatalf("run_funnel invoke: %v", err)
	}
	if repo.gotInsightType != "funnel" || len(repo.gotSteps) != 2 {
		t.Fatalf("run_funnel delegated wrong: type=%q steps=%v", repo.gotInsightType, repo.gotSteps)
	}

	retention, _ := reg.Get("run_retention")
	if _, err := retention.OpInvoke(context.Background(), *deps, `{"event":"purchase"}`); err != nil {
		t.Fatalf("run_retention invoke: %v", err)
	}
	if repo.gotInsightType != "retention" || repo.gotInsightEvent != "purchase" {
		t.Fatalf("run_retention delegated wrong: type=%q event=%q", repo.gotInsightType, repo.gotInsightEvent)
	}
}

func TestRegistryHasEveryOperation(t *testing.T) {
	reg := Registry()
	want := []string{
		"activity_summary", "recent_events", "persons", "explore_events", "run_sql",
		"run_insight", "run_funnel", "run_retention", "list_dashboards", "create_dashboard", "create_chart",
		"submit_recommendation", "remember", "send_notification",
	}
	for _, name := range want {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing operation %q", name)
		}
	}
	if got := len(reg.Specs()); got != len(want) {
		t.Errorf("registry size = %d, want %d", got, len(want))
	}
}

func TestRunSQLSchemaMarksSQLRequired(t *testing.T) {
	reg := Registry()
	spec, _ := reg.Get("run_sql")
	schema := spec.OpSchema()
	req, _ := schema["required"].([]string)
	if len(req) != 1 || req[0] != "sql" {
		t.Fatalf("run_sql required = %v, want [sql]", schema["required"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["sql"]; !ok {
		t.Errorf("run_sql schema missing sql property: %v", props)
	}
}

func TestSubmitRecommendationIsTerminal(t *testing.T) {
	terminal := opcore.TerminalNames(Registry())
	if !terminal["submit_recommendation"] {
		t.Error("submit_recommendation should be a terminal operation")
	}
	if terminal["run_sql"] {
		t.Error("run_sql should not be terminal")
	}
}

func TestRunSQLNormalizesClickHouseDialect(t *testing.T) {
	repo := &fakeRepo{}
	reg := Registry()
	spec, _ := reg.Get("run_sql")
	cc := opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}

	in, _ := json.Marshal(map[string]string{
		"sql": "SELECT JSON_EXTRACT_STRING(properties, 'path') FROM events",
	})
	if _, err := spec.OpInvoke(context.Background(), cc, string(in)); err != nil {
		t.Fatalf("OpInvoke: %v", err)
	}
	want := "SELECT JSONExtractString(properties, 'path') FROM events"
	if repo.gotSQL != want {
		t.Errorf("normalized SQL = %q, want %q", repo.gotSQL, want)
	}
}

func (f *fakeRepo) CreateRecommendation(_ context.Context, rec storage.AgentRecommendation) (string, error) {
	f.gotRec = rec
	return "rec-1", nil
}

// A missing required field is rejected uniformly by opcore (not by a per-handler
// check) with an error that names the field, so the model can self-correct.
func TestMissingRequiredFieldNamesTheField(t *testing.T) {
	repo := &fakeRepo{}
	spec, _ := Registry().Get("submit_recommendation")
	cc := opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}
	_, err := spec.OpInvoke(context.Background(), cc, `{"category":"growth","rationale":"x"}`)
	if err == nil {
		t.Fatal("expected a missing-required-field error")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error should name the missing field: %v", err)
	}
	if (repo.gotRec != storage.AgentRecommendation{}) {
		t.Error("handler must not run when validation fails")
	}
}

// A valid call passes validation and reaches the handler.
func TestSubmitRecommendationWithTitleSucceeds(t *testing.T) {
	repo := &fakeRepo{}
	spec, _ := Registry().Get("submit_recommendation")
	cc := opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}
	out, err := spec.OpInvoke(context.Background(), cc, `{"category":"growth","title":"Boost homepage retention"}`)
	if err != nil {
		t.Fatalf("OpInvoke: %v", err)
	}
	if repo.gotRec.Title != "Boost homepage retention" || !strings.Contains(out, "rec-1") {
		t.Errorf("unexpected result: title=%q out=%s", repo.gotRec.Title, out)
	}
}

// compile-time guard: *storage.Store must satisfy Repo (the inversion that keeps
// the agent off infra). A drift in either signature breaks the build here.
var _ Repo = (*storage.Store)(nil)
