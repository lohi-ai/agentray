package usecase

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

type queryMetaRepo struct{ fakeRepo }

func (r *queryMetaRepo) RunSQLWithMeta(_ context.Context, _, sqlText string) ([]map[string]any, storage.QueryMeta, error) {
	r.gotSQL = sqlText
	return []map[string]any{{"ok": 1}}, storage.QueryMeta{QueryRef: "q1", QueryDigest: "digest", ExecutedAt: time.Now().UTC(), ResultCompleteness: storage.ResultComplete}, nil
}

func TestReadinessContractConnectorWithoutSyncsKeepsEmptyArray(t *testing.T) {
	repo := &readinessStatusRepo{}
	out, err := sourceStatus().Handler(context.Background(), opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}, sourceStatusInput{ConnectorID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"syncs":[]`) {
		t.Fatalf("zero-sync connector lost its empty array: %s", body)
	}
}

func TestQueryEvidenceContractPreservesRowsAndAddsMeta(t *testing.T) {
	repo := &queryMetaRepo{}
	out, err := runSQL().Handler(context.Background(), opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}, runSQLInput{SQL: "select * from events"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 1 || out.Rows[0]["ok"] != 1 || out.Meta.QueryRef != "q1" || repo.gotSQL == "" {
		t.Fatalf("output = %+v", out)
	}
}

type readinessStatusRepo struct {
	batchStatusRepo
	readiness map[string]*storage.Readiness
}

func (r *readinessStatusRepo) SourceReadiness(context.Context, string, []storage.ReadinessSource) (map[string]*storage.Readiness, error) {
	return r.readiness, nil
}

func TestReadinessContractBulkSourceStatus(t *testing.T) {
	reason := "coverage_hole"
	repo := &readinessStatusRepo{batchStatusRepo: batchStatusRepo{syncs: []storage.ConnectorSync{{ID: "s1", ConnectorID: "c1", SourceTable: "orders", KeyColumn: "id"}}},
		readiness: map[string]*storage.Readiness{"s1": {State: storage.ReadinessIncomplete, Reason: &reason}}}
	out, err := sourceStatus().Handler(context.Background(), opcore.CallContext{ProjectID: "p1", Deps: &Deps{Repo: repo}}, sourceStatusInput{ConnectorID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Syncs) != 1 || out.Syncs[0].Readiness == nil || out.Syncs[0].Readiness.State != storage.ReadinessIncomplete {
		t.Fatalf("output = %+v", out)
	}
}
