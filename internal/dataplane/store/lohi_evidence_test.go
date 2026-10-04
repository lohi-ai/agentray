package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
	"github.com/lohi-ai/agentray/internal/workloads"
)

func seedLohiEvidenceFixture(t *testing.T, d *DuckDB) querytest.LohiEvidenceFixture {
	t.Helper()
	fixture, err := querytest.LohiEvidenceV1()
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
			SessionID: "lohi-evidence-v1", EventName: input.EventName, EventType: "user",
			Properties: input.Properties, Timestamp: at, VisitorClass: "human",
			InsertID: input.InsertID, Platform: input.Platform, UTMSource: input.UTMSource,
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

func TestLohiEvidenceV1RecipesExecuteAndRepeatExactly(t *testing.T) {
	d := openTestDuckDB(t)
	fixture := seedLohiEvidenceFixture(t, d)
	pool, ctx := newTestSandboxPool(t, d, nil)
	recipes := workloads.LohiEvidenceRecipes()
	if len(recipes) != 11 {
		t.Fatalf("recipe count = %d, want 11", len(recipes))
	}

	for n := 1; n <= 11; n++ {
		ref := fmt.Sprintf("R%02d", n)
		sqlText := recipes[ref]
		t.Run(ref, func(t *testing.T) {
			query, args, err := scopedReadonlySQL(sqlText, fixture.ProjectID, nil)
			if err != nil {
				t.Fatalf("scope %s: %v", ref, err)
			}
			rows, err := pool.query(ctx, fixture.ProjectID, query, args)
			if err != nil {
				t.Fatalf("execute %s: %v\n%s", ref, err, query)
			}
			again, err := pool.query(ctx, fixture.ProjectID, query, args)
			if err != nil {
				t.Fatalf("repeat %s: %v", ref, err)
			}
			if got, want := canonicalRows(t, again), canonicalRows(t, rows); got != want {
				t.Fatalf("%s changed on repeat\nfirst: %s\nagain: %s", ref, want, got)
			}
			if len(rows) == 0 {
				t.Fatalf("%s returned no rows", ref)
			}
			for i, row := range rows {
				for _, column := range []string{"date", "series", "value", "unit", "sample_size", "state", "reason"} {
					if _, ok := row[column]; !ok {
						t.Fatalf("%s row %d missing %s: %#v", ref, i, column, row)
					}
				}
				if value, ok := row["value"].(float64); ok && (math.IsNaN(value) || math.IsInf(value, 0)) {
					t.Fatalf("%s row %d returned non-finite value %v", ref, i, value)
				}
			}
			for _, assertion := range fixture.Assertions[ref] {
				assertLohiRow(t, rows, assertion)
			}
		})
	}
}

func assertLohiRow(t *testing.T, rows []map[string]any, want querytest.LohiAssertion) {
	t.Helper()
	for _, row := range rows {
		if fmt.Sprint(row["date"]) != want.Date || fmt.Sprint(row["series"]) != want.Series {
			continue
		}
		if want.Value != nil && !sameFixtureNumber(row["value"], want.Value) {
			t.Fatalf("%s/%s value = %v (%T), want %v", want.Date, want.Series, row["value"], row["value"], want.Value)
		}
		if want.State != "" && fmt.Sprint(row["state"]) != want.State {
			t.Fatalf("%s/%s state = %v, want %s", want.Date, want.Series, row["state"], want.State)
		}
		if want.ReasonLike != "" && !strings.Contains(fmt.Sprint(row["reason"]), want.ReasonLike) {
			t.Fatalf("%s/%s reason = %v, want substring %q", want.Date, want.Series, row["reason"], want.ReasonLike)
		}
		if want.Eligible != nil && fmt.Sprint(row["eligible"]) != fmt.Sprint(want.Eligible) {
			t.Fatalf("%s/%s eligible = %v, want %v", want.Date, want.Series, row["eligible"], want.Eligible)
		}
		if want.Converted != nil && fmt.Sprint(row["converted"]) != fmt.Sprint(want.Converted) {
			t.Fatalf("%s/%s converted = %v, want %v", want.Date, want.Series, row["converted"], want.Converted)
		}
		return
	}
	encoded, _ := json.Marshal(rows)
	t.Fatalf("missing asserted row %s/%s in %s", want.Date, want.Series, encoded)
}

func sameFixtureNumber(got, want any) bool {
	gotNumber, gotErr := strconv.ParseFloat(fmt.Sprint(got), 64)
	wantNumber, wantErr := strconv.ParseFloat(fmt.Sprint(want), 64)
	return gotErr == nil && wantErr == nil && gotNumber == wantNumber
}

func TestLohiEvidenceInstallsOnStockDataAnalystWithoutOverwrite(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	pack := workloads.MustBySlug("data-analyst")
	preset := AgentPreset{
		Slug: pack.Slug, Name: pack.Name, Tagline: pack.Tagline, Description: pack.Description,
		Category: string(pack.Category), Icon: pack.Icon, SoulMD: pack.SoulMD, AgentsMD: pack.AgentsMD,
		Scopes: pack.Scopes,
	}
	for _, skill := range pack.Skills {
		preset.Skills = append(preset.Skills, AgentPresetSkill(skill))
	}
	SetPackCatalog(func() []AgentPreset { return []AgentPreset{preset} }, func(slug string) (AgentPreset, bool) {
		return preset, slug == preset.Slug
	})
	t.Cleanup(func() { SetPackCatalog(nil, nil) })

	agent, err := s.InstallAgentPreset(ctx, userID, projectID, "data-analyst")
	if err != nil {
		t.Fatalf("install data analyst: %v", err)
	}
	skills, err := s.ListAgentSkills(ctx, userID, projectID, agent.ID)
	if err != nil {
		t.Fatalf("list installed skills: %v", err)
	}
	var installed AgentSkill
	for _, skill := range skills {
		if skill.Name == workloads.LohiEvidenceVersion {
			installed = skill
		}
	}
	if installed.ID == "" || installed.Body != workloads.LohiEvidenceSkill().Body {
		t.Fatalf("portable skill was not installed verbatim: id=%q body_equal=%v", installed.ID, installed.Body == workloads.LohiEvidenceSkill().Body)
	}

	installed.Body += "\n\nOperator note: preserve this local edit."
	if _, err := s.UpsertAgentSkill(ctx, userID, projectID, agent.ID, installed); err != nil {
		t.Fatalf("edit installed skill: %v", err)
	}
	reinstalled, err := s.InstallAgentPreset(ctx, userID, projectID, "data-analyst")
	if err != nil {
		t.Fatalf("reinstall data analyst: %v", err)
	}
	if reinstalled.ID != agent.ID {
		t.Fatalf("reinstall created agent %s, want existing %s", reinstalled.ID, agent.ID)
	}
	skills, err = s.ListAgentSkills(ctx, userID, projectID, agent.ID)
	if err != nil {
		t.Fatalf("list reinstalled skills: %v", err)
	}
	for _, skill := range skills {
		if skill.Name == workloads.LohiEvidenceVersion && !strings.Contains(skill.Body, "preserve this local edit") {
			t.Fatal("reinstall overwrote the operator-edited Lohi skill")
		}
	}
}
