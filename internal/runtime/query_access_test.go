package agentruntime

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

func TestQueryAccessRuntimeScopeMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes Scopes
		keep   []string
		drop   []string
	}{
		{"monitor", Scopes{Monitor: true}, []string{ToolActivitySummary, ToolSourceStatus}, []string{ToolRunSQL, ToolCreateChart, ToolCreateSource}},
		{"data quality", Scopes{DataQuality: true}, []string{ToolRunSQL, ToolDatasetPreview, ToolListSources}, []string{ToolCreateChart, ToolCreateSource, ToolSendNotification}},
		{"analysis builder", Scopes{AnalyzeBuild: true}, []string{ToolRunSQL, ToolCreateChart, ToolCreateSource}, []string{ToolSendNotification, ToolSubmitRec}},
		{"growth", Scopes{GrowthSuggest: true}, []string{ToolSubmitRec, ToolRemember, ToolSendNotification}, []string{ToolRunSQL, ToolCreateChart, ToolCreateSource}},
		{"none", Scopes{}, nil, []string{ToolRunSQL, ToolActivitySummary, ToolCreateChart, ToolCreateSource, ToolSendNotification}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := ScopeToolNames(tc.scopes)
			for _, name := range tc.keep {
				if !slices.Contains(names, name) {
					t.Errorf("missing granted tool %q from %v", name, names)
				}
			}
			for _, name := range tc.drop {
				if slices.Contains(names, name) {
					t.Errorf("unexpected tool %q in %v", name, names)
				}
			}
		})
	}
}

func TestQueryAccessReadOnlyRemovesEveryMutationAndEscape(t *testing.T) {
	p := BuildParams{
		Scopes:    allScopes,
		ReadOnly:  true,
		Memory:    newScopedMemory(),
		HTTPTool:  stubTool{name: "http_request"},
		Tools:     []agentcore.Tool{stubTool{name: "run_shell"}},
		Subagents: &subagent.Plugin{},
	}
	names := permittedToolNames(p)
	for _, name := range []string{
		ToolCreateSource, ToolUpdateSource, ToolRunSource,
		ToolCreateDashboard, ToolCreateChart, ToolSaveBoard,
		ToolSubmitRec, ToolProposeTest, ToolRemember, ToolSendNotification,
		"memory_edit", "learn", "http_request", "run_shell", subagent.ToolSpawnSubagent,
	} {
		if slices.Contains(names, name) {
			t.Errorf("read-only runtime retained mutation/escape %q", name)
		}
	}
	for _, name := range []string{ToolRunSQL, ToolSourceStatus, ToolListSources, ToolListDashboards, ToolListFindings} {
		if !slices.Contains(names, name) {
			t.Errorf("read-only runtime lost read %q", name)
		}
	}
}

type deterministicQueryRepo struct {
	usecase.Repo
	project string
	sql     string
}

func (r *deterministicQueryRepo) RunSQL(_ context.Context, projectID, sql string) ([]map[string]any, error) {
	r.project, r.sql = projectID, sql
	return []map[string]any{{"date": "2026-09-01", "value": int64(2)}}, nil
}

func TestQueryAccessExecutesThroughRealRegistryTool(t *testing.T) {
	repo := &deterministicQueryRepo{}
	reg := usecase.Registry()
	cc := opcore.CallContext{ProjectID: "project-1", Deps: &usecase.Deps{Repo: repo}}
	var tool agentcore.Tool
	for _, candidate := range opcore.Tools(reg, cc) {
		if candidate.Name() == ToolRunSQL {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("real operation registry did not expose run_sql")
	}
	out, err := tool.Run(context.Background(), `{"sql":"SELECT count(*) AS value FROM events"}`)
	if err != nil {
		t.Fatalf("run registry tool: %v", err)
	}
	if repo.project != "project-1" || repo.sql != "SELECT count(*) AS value FROM events" {
		t.Fatalf("repo call project=%q sql=%q", repo.project, repo.sql)
	}
	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode tool output: %v; body=%s", err, out)
	}
	if len(result.Rows) != 1 || result.Rows[0]["value"] != float64(2) {
		t.Fatalf("tool output = %#v", result.Rows)
	}
}
