package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// boards_test.go — live tests for the declarative board content model and the
// served metric catalog. Needs the compose Postgres; skips without one.

func TestBoardDefinitionJSONRejectsUnknownFields(t *testing.T) {
	cases := []string{
		`{"version":1,"sectons":[]}`,
		`{"version":1,"sections":[{"key":"s","title":"S","tile":[]}]}`,
		`{"version":1,"sections":[{"key":"s","title":"S","tiles":[{"key":"t","metric":"active_users","colour":"red"}]}]}`,
		`{"version":1,"sections":[{"key":"s","title":"S","tiles":[{"key":"t","metric":"active_users","params":{"period":"7d","foo":1}}]}]}`,
	}
	for _, raw := range cases {
		var def BoardDefinition
		if err := json.Unmarshal([]byte(raw), &def); err == nil {
			t.Errorf("accepted unknown field: %s", raw)
		} else if !errors.Is(err, ErrBoardDefinitionInvalid) {
			t.Errorf("%s: err = %v, want ErrBoardDefinitionInvalid", raw, err)
		}
	}
	var ok BoardDefinition
	if err := json.Unmarshal([]byte(`{"version":1,"sections":[{"key":"s","title":"S","tiles":[{"key":"t","metric":"active_users","display":"stat","params":{"period":"7d","platform":"web"}},{"key":"f","kind":"funnel","steps":["a","b"]}]}]}`), &ok); err != nil {
		t.Fatalf("known fields refused: %v", err)
	}
	if len(ok.Sections) != 1 || len(ok.Sections[0].Tiles) != 2 || ok.Sections[0].Tiles[0].Metric != "active_users" {
		t.Fatalf("known-fields decode = %+v", ok)
	}
	if ok.Sections[0].Tiles[1].Kind != TileKindFunnel || len(ok.Sections[0].Tiles[1].Steps) != 2 {
		t.Fatalf("funnel tile decode = %+v", ok.Sections[0].Tiles[1])
	}
}
func metricTile(key, metric, display string, span int) BoardTile {
	return BoardTile{Key: key, Kind: TileKindMetric, Metric: metric, Display: display, Span: span}
}

func twoTileBoard() BoardDefinition {
	return BoardDefinition{
		Version: BoardDefinitionVersion,
		Sections: []BoardSection{{
			Key: "overview", Title: "Overview", Description: "headline",
			Tiles: []BoardTile{
				metricTile("people", MetricActiveUsers, DisplayStat, 1),
				{
					Key: "people-trend", Kind: TileKindMetric, Metric: MetricActiveUsersDaily,
					Display: DisplayLine, Span: 2,
					Params: &BoardTileParams{Period: "30d", Platform: PlatformWeb},
				},
			},
		}},
	}
}

func TestMetricCatalogIsServedFromPostgres(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()

	served, err := s.ListMetricDefinitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	declared := MetricCatalog()
	if len(served) != len(declared) {
		t.Fatalf("served %d metrics, declared %d", len(served), len(declared))
	}
	for i, got := range served {
		want := declared[i]
		if got.Key != want.Key || got.Label != want.Label || got.Unit != want.Unit || got.Kind != want.Kind ||
			got.Group != want.Group || got.Definition != want.Definition || got.Prerequisite != want.Prerequisite ||
			got.MetricVersion != want.MetricVersion || got.SortOrder != want.SortOrder || !got.IsSystem {
			t.Errorf("catalog row %d drifted: got %+v want %+v", i, got, want)
		}
		if len(got.Displays) != len(want.Displays) || len(got.Params) != len(want.Params) {
			t.Errorf("catalog row %q displays/params = %v / %v, want %v / %v", want.Key, got.Displays, got.Params, want.Displays, want.Params)
		}
	}

	entry, err := s.MetricDefinitionByKey(ctx, MetricSessions)
	if err != nil || entry.Key != MetricSessions {
		t.Fatalf("sessions lookup = %+v, %v", entry, err)
	}
	_, err = s.MetricDefinitionByKey(ctx, "no_such_metric")
	if !errors.Is(err, ErrMetricUnknown) {
		t.Fatalf("unknown metric err = %v, want ErrMetricUnknown", err)
	}
	if !strings.Contains(err.Error(), MetricActiveUsers) {
		t.Errorf("the refusal must list the keys that do exist: %v", err)
	}
}

// TestCatalogRefreshDropsAMetricTheCodeNoLongerDeclares: the table is a
// projection of the declaration, so a boot removes rows the code dropped
// instead of serving a metric nothing computes.
func TestCatalogRefreshDropsAMetricTheCodeNoLongerDeclares(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	if _, err := s.pg.Exec(ctx, `INSERT INTO metric_definitions (key, metric_version, label, unit, kind, metric_group, definition)
VALUES ('legacy_metric', $1, 'Legacy', 'events', 'value', 'overview', 'a metric the code no longer declares')`, OverviewMetricVersion); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	if err := s.migrateMetricCatalog(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := s.MetricDefinitionByKey(ctx, "legacy_metric"); !errors.Is(err, ErrMetricUnknown) {
		t.Fatalf("stale catalog row survived a refresh: %v", err)
	}
}

func TestBoardDeclarationRoundTrip(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	name := "App overview"
	content, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "app-overview", Name: &name, Definition: twoTileBoard(),
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if content.Board.Revision != 1 || content.Board.BoardKey != "app-overview" || content.Board.Name != name {
		t.Fatalf("declared board = %+v", content.Board)
	}
	if !content.HasDefinition || len(content.Definition.Sections) != 1 || len(content.Definition.Sections[0].Tiles) != 2 {
		t.Fatalf("declared content = %+v", content)
	}
	// The resolved payload carries the catalog entries the tiles reference, in
	// first-reference order and without duplicates.
	if len(content.Metrics) != 2 || content.Metrics[0].Key != MetricActiveUsers || content.Metrics[1].Key != MetricActiveUsersDaily {
		t.Fatalf("resolved metrics = %+v", content.Metrics)
	}
	if len(content.Warnings) != 0 {
		t.Fatalf("fresh declaration warned: %v", content.Warnings)
	}

	byKey, err := s.BoardContentByKey(ctx, projectID, "app-overview")
	if err != nil {
		t.Fatalf("read by key: %v", err)
	}
	if byKey.Board.ID != content.Board.ID || byKey.Board.Revision != 1 {
		t.Fatalf("read by key = %+v, want the declared board", byKey.Board)
	}
	byID, err := s.BoardContentForProject(ctx, projectID, content.Board.ID)
	if err != nil || byID.Board.ID != content.Board.ID {
		t.Fatalf("read by id = %+v, %v", byID.Board, err)
	}

	// Declaring again with the current revision replaces the content.
	updated := twoTileBoard()
	updated.Sections = append(updated.Sections, BoardSection{Key: "money", Title: "Money", Tiles: []BoardTile{
		metricTile("revenue", MetricRevenue, DisplayStat, 1),
	}})
	content, err = s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: updated, ExpectedRevision: 1,
	}, "", "")
	if err != nil {
		t.Fatalf("redeclare: %v", err)
	}
	if content.Board.Revision != 2 || len(content.Definition.Sections) != 2 {
		t.Fatalf("redeclared board = rev %d sections %d", content.Board.Revision, len(content.Definition.Sections))
	}
	if len(content.Metrics) != 3 || content.Metrics[2].Key != MetricRevenue {
		t.Fatalf("resolved metrics after redeclare = %+v", content.Metrics)
	}

	// Declaring empty content is a declaration, not an omission.
	content, err = s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: BoardDefinition{}, ExpectedRevision: 2,
	}, "", "")
	if err != nil {
		t.Fatalf("declare empty: %v", err)
	}
	if !content.HasDefinition || len(content.Definition.Sections) != 0 || content.Definition.Version != BoardDefinitionVersion {
		t.Fatalf("empty declaration = %+v", content)
	}

	// A tile's parameters are stored as they were validated: what a read
	// accepts a caller may hand back verbatim, so an accepted period is
	// persisted trimmed and an accepted platform lower-cased.
	padded := BoardDefinition{Version: BoardDefinitionVersion, Sections: []BoardSection{{
		Key: "overview", Title: "Overview", Tiles: []BoardTile{{
			Key: "padded", Kind: TileKindMetric, Metric: MetricActiveUsers, Display: DisplayStat,
			Params: &BoardTileParams{Period: "  7d  ", Platform: "WEB"},
		}},
	}}}
	content, err = s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: padded, ExpectedRevision: 3,
	}, "", "")
	if err != nil {
		t.Fatalf("declare padded params: %v", err)
	}
	stored := content.Definition.Sections[0].Tiles[0].Params
	if stored == nil || stored.Period != "7d" || stored.Platform != PlatformWeb {
		t.Fatalf("stored params = %+v, want them normalized", stored)
	}
	if err := validPeriod(stored.Period); err != nil {
		t.Fatalf("a stored period is unreadable: %v", err)
	}
}

func TestBoardDeclarationFencesAndIdempotency(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	name := "Fences"
	board, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "fences", Name: &name, Definition: twoTileBoard(),
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}

	// A stale revision conflicts and leaves the row alone.
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: BoardDefinition{}, ExpectedRevision: 7,
	}, "", ""); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale declaration err = %v, want ErrRevisionConflict", err)
	}
	cur, err := s.BoardContentForProject(ctx, projectID, board.Board.ID)
	if err != nil || cur.Board.Revision != 1 || len(cur.Definition.Sections) != 1 {
		t.Fatalf("conflict changed the board: %+v (%v)", cur, err)
	}

	// A create intent (no revision) against an existing key conflicts instead
	// of overwriting a board the declarer has not read.
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "fences", Definition: BoardDefinition{},
	}, "", ""); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("create-over-existing err = %v, want ErrRevisionConflict", err)
	}

	// A retry under the same idempotency key returns the first result and does
	// not bump the revision twice.
	first, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: twoTileBoard(), ExpectedRevision: 1,
	}, "declare-1", "hash-1")
	if err != nil {
		t.Fatalf("first declare: %v", err)
	}
	replay, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: twoTileBoard(), ExpectedRevision: 1,
	}, "declare-1", "hash-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first.Board.Revision != 2 || replay.Board.Revision != 2 {
		t.Fatalf("replay bumped the revision: first=%d replay=%d", first.Board.Revision, replay.Board.Revision)
	}
	// The same key with a different payload is a conflict, not a replay.
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: BoardDefinition{}, ExpectedRevision: 2,
	}, "declare-1", "hash-2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reuse err = %v, want ErrIdempotencyConflict", err)
	}

	// A declaration cannot take a key that belongs to another board.
	other := "Other"
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "other", Name: &other, Definition: BoardDefinition{},
	}, "", ""); err != nil {
		t.Fatalf("declare second board: %v", err)
	}
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, BoardKey: "other", Definition: BoardDefinition{}, ExpectedRevision: replay.Board.Revision,
	}, "", ""); !errors.Is(err, ErrRevisionConflict) || !strings.Contains(err.Error(), "another board") {
		t.Fatalf("key takeover err = %v, want a conflict naming the other board", err)
	}
}

func TestBoardDeclarationValidation(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	name := "Validation"
	board, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "validation", Name: &name, Definition: BoardDefinition{},
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	// A chart from another project's board is not this board's chart.
	_, otherProject := seedConvProject(t, s)
	otherBoard, err := s.CreateDashboard(ctx, otherProject, "Other", "")
	if err != nil {
		t.Fatalf("other board: %v", err)
	}
	otherChart, err := s.CreateChart(ctx, Chart{DashboardID: otherBoard.ID, ProjectID: otherProject, Name: "foreign", Kind: "line"})
	if err != nil {
		t.Fatalf("foreign chart: %v", err)
	}

	cases := []struct {
		name    string
		def     BoardDefinition
		wantErr string
	}{
		{
			name: "unknown metric",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				metricTile("t", "crashes", DisplayStat, 1),
			}}}},
			wantErr: "not in the catalog",
		},
		{
			name: "display the metric kind cannot carry",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				metricTile("t", MetricActiveUsers, DisplayTable, 1),
			}}}},
			wantErr: "can be drawn as stat",
		},
		{
			name: "span outside the grid",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				metricTile("t", MetricActiveUsers, DisplayStat, 4),
			}}}},
			wantErr: "spans 1 to 3",
		},
		{
			name: "duplicate tile key",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				metricTile("t", MetricActiveUsers, DisplayStat, 1),
				metricTile("t", MetricSessions, DisplayStat, 1),
			}}}},
			wantErr: "declared twice",
		},
		{
			name: "duplicate section key",
			def: BoardDefinition{Sections: []BoardSection{
				{Key: "s", Title: "S", Tiles: []BoardTile{metricTile("a", MetricActiveUsers, DisplayStat, 1)}},
				{Key: "s", Title: "S again", Tiles: []BoardTile{metricTile("b", MetricSessions, DisplayStat, 1)}},
			}},
			wantErr: "declared twice",
		},
		{
			name: "section without a title",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Tiles: []BoardTile{
				metricTile("t", MetricActiveUsers, DisplayStat, 1),
			}}}},
			wantErr: "no title",
		},
		{
			name:    "unsupported document version",
			def:     BoardDefinition{Version: 99, Sections: []BoardSection{}},
			wantErr: "not supported",
		},
		{
			name: "chart tile carrying a display",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindChart, ChartID: uuid.NewString(), Display: DisplayLine},
			}}}},
			wantErr: "chart tile",
		},
		{
			name: "chart tile with a foreign chart",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindChart, ChartID: otherChart.ID},
			}}}},
			wantErr: "not on this board",
		},
		{
			name: "tile that declares nothing",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t"},
			}}}},
			wantErr: "neither a metric nor a chart",
		},
		{
			name: "tile that declares both",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Metric: MetricActiveUsers, ChartID: otherChart.ID},
			}}}},
			wantErr: "more than one of metric, chart_id and steps",
		},
		{
			name: "funnel tile with one step",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"user.pageview"}},
			}}}},
			wantErr: "at least 2",
		},
		{
			name: "funnel tile with an empty step",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"user.pageview", "  "}},
			}}}},
			wantErr: "empty funnel step",
		},
		{
			name: "funnel tile carrying a metric",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Metric: MetricActiveUsers, Steps: []string{"a", "b"}},
			}}}},
			wantErr: "funnel tile",
		},
		{
			name: "funnel tile carrying a display",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"a", "b"}, Display: DisplayBar},
			}}}},
			wantErr: "funnel tile",
		},
		{
			name: "funnel tile carrying params",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"a", "b"}, Params: &BoardTileParams{Period: "7d"}},
			}}}},
			wantErr: "funnel tile",
		},
		{
			name: "funnel tile carrying a target",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"a", "b"}, Target: &MetricTargetSpec{Direction: "gte", Value: 1, Period: "7d"}},
			}}}},
			wantErr: "funnel tile",
		},
		{
			name: "metric tile carrying steps",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Metric: MetricActiveUsers, Steps: []string{"a", "b"}},
			}}}},
			wantErr: "more than one of metric, chart_id and steps",
		},
		{
			name: "chart tile carrying steps",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindChart, ChartID: uuid.NewString(), Steps: []string{"a", "b"}},
			}}}},
			wantErr: "cannot also declare funnel steps",
		},
		{
			name: "funnel tile over the step bound",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindFunnel, Steps: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m"}},
			}}}},
			wantErr: "maximum",
		},
		{
			name: "chart_id that is not a uuid",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Kind: TileKindChart, ChartID: "not-a-uuid"},
			}}}},
			wantErr: "not a chart id",
		},
		{
			name: "param outside the range contract",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Metric: MetricActiveUsers, Params: &BoardTileParams{Period: "week"}},
			}}}},
			wantErr: "range contract",
		},
		{
			name: "unknown platform",
			def: BoardDefinition{Sections: []BoardSection{{Key: "s", Title: "S", Tiles: []BoardTile{
				{Key: "t", Metric: MetricActiveUsers, Params: &BoardTileParams{Platform: "smart-fridge"}},
			}}}},
			wantErr: "unknown",
		},
	}
	for _, tc := range cases {
		if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
			BoardID: board.Board.ID, Definition: tc.def, ExpectedRevision: 1,
		}, "", ""); !errors.Is(err, ErrBoardDefinitionInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want invalid mentioning %q", tc.name, err, tc.wantErr)
		}
	}

	// Nothing was stored by any of the refused declarations.
	cur, err := s.BoardContentForProject(ctx, projectID, board.Board.ID)
	if err != nil || cur.Board.Revision != 1 || len(cur.Definition.Sections) != 0 {
		t.Fatalf("a refused declaration changed the board: %+v (%v)", cur, err)
	}
}

// TestChartTilePlacesAnExistingChartAndWarnsWhenItLeaves walks the one path a
// board has to a saved chart: the chart exists on the board, a tile places it,
// and the tile reports itself as dangling once the chart is archived.
func TestChartTilePlacesAnExistingChartAndWarnsWhenItLeaves(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	board, err := s.CreateDashboard(ctx, projectID, "Legacy board", "")
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	chart, err := s.CreateChart(ctx, Chart{DashboardID: board.ID, ProjectID: projectID, Name: "guest vs identified", Kind: "line", Metric: "users"})
	if err != nil {
		t.Fatalf("create chart: %v", err)
	}

	content, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.ID, ExpectedRevision: board.Revision,
		Definition: BoardDefinition{Sections: []BoardSection{{Key: "legacy", Title: "Legacy", Tiles: []BoardTile{
			{Key: "chart-1", Kind: TileKindChart, ChartID: strings.ToUpper(chart.ID)},
			metricTile("people", MetricActiveUsers, DisplayStat, 1),
		}}}},
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if len(content.Charts) != 1 || content.Charts[0].ID != chart.ID {
		t.Fatalf("resolved charts = %+v", content.Charts)
	}
	if len(content.Warnings) != 0 {
		t.Fatalf("warnings = %v", content.Warnings)
	}
	if content.Definition.Sections[0].Tiles[0].ChartID != chart.ID {
		t.Fatalf("stored chart_id = %q, want the canonical form %q", content.Definition.Sections[0].Tiles[0].ChartID, chart.ID)
	}

	if _, err := s.ArchiveChartIdempotent(ctx, projectID, chart.ID, chart.Revision, "", ""); err != nil {
		t.Fatalf("archive chart: %v", err)
	}
	content, err = s.BoardContentForProject(ctx, projectID, board.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(content.Warnings) != 1 || !strings.Contains(content.Warnings[0], "archived") {
		t.Fatalf("archived chart warning = %v", content.Warnings)
	}

	// A metric the catalog no longer serves is a warning too, not a silent
	// gap: the row is deleted underneath the board.
	t.Cleanup(func() { _ = s.migrateMetricCatalog(ctx) })
	if _, err := s.pg.Exec(ctx, `DELETE FROM metric_definitions WHERE key = $1`, MetricActiveUsers); err != nil {
		t.Fatalf("drop metric: %v", err)
	}
	content, err = s.BoardContentForProject(ctx, projectID, board.ID)
	if err != nil {
		t.Fatalf("read after metric drop: %v", err)
	}
	found := false
	for _, warning := range content.Warnings {
		if strings.Contains(warning, MetricActiveUsers) && strings.Contains(warning, "no longer in the catalog") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped metric warning = %v", content.Warnings)
	}
	if len(content.Metrics) != 0 {
		t.Fatalf("a dropped metric was still resolved: %+v", content.Metrics)
	}
}

// TestUndeclaredBoardKeepsRenderingFromItsCharts: every board created before
// declarations existed serves an empty definition and its chart list is
// untouched — the declarative model is additive.
func TestUndeclaredBoardKeepsRenderingFromItsCharts(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	board, err := s.CreateDashboard(ctx, projectID, "Undeclared", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateChart(ctx, Chart{DashboardID: board.ID, ProjectID: projectID, Name: "one", Kind: "line", Metric: "events"}); err != nil {
		t.Fatalf("create chart: %v", err)
	}

	content, err := s.BoardContentForProject(ctx, projectID, board.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if content.HasDefinition || len(content.Definition.Sections) != 0 || content.Board.BoardKey != "" {
		t.Fatalf("undeclared board = %+v", content)
	}
	if content.Definition.Version != BoardDefinitionVersion {
		t.Errorf("empty definition version = %d", content.Definition.Version)
	}
	charts, err := s.ListChartsFiltered(ctx, projectID, board.ID, false)
	if err != nil || len(charts) != 1 {
		t.Fatalf("charts = %+v (%v)", charts, err)
	}
	if _, err := s.BoardContentByKey(ctx, projectID, "never-declared"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown key err = %v, want ErrNoRows", err)
	}
}

// TestFunnelTileKindInferenceAndRoundTrip: steps alone infer the funnel kind,
// declared steps are stored verbatim, and a funnel tile that declares none is
// served with steps resolved at read time — or a warning when the event
// catalog is unreachable (the test store has no DuckDB).
func TestFunnelTileKindInferenceAndRoundTrip(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	name := "Funnels"
	content, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "funnels", Name: &name,
		Definition: BoardDefinition{Sections: []BoardSection{{
			Key: "f", Title: "F", Tiles: []BoardTile{
				// No kind: steps alone infer funnel.
				{Key: "declared", Steps: []string{"user.pageview", "user.signup"}},
				// No steps: kind must be explicit — nothing else to infer from.
				{Key: "derived", Kind: TileKindFunnel},
			},
		}}},
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	tiles := content.Definition.Sections[0].Tiles
	if tiles[0].Kind != TileKindFunnel {
		t.Fatalf("steps did not infer the funnel kind: %+v", tiles[0])
	}
	if len(tiles[0].Steps) != 2 || tiles[0].Steps[0] != "user.pageview" {
		t.Fatalf("declared steps = %v, want them stored verbatim", tiles[0].Steps)
	}
	// No DuckDB in this store: the derived tile keeps steps unset and the read
	// says why, instead of failing the whole board.
	if tiles[1].Steps != nil {
		t.Fatalf("derived steps = %v, want unresolved without a catalog", tiles[1].Steps)
	}
	foundWarning := false
	for _, w := range content.Warnings {
		if strings.Contains(w, "funnel steps could not be derived") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("no derivation warning on the receipt: %v", content.Warnings)
	}

	// A read resolves the same way — and the stored document still has no
	// steps, so a later read re-derives rather than serving a frozen list.
	read, err := s.BoardContentForProject(ctx, projectID, content.Board.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Definition.Sections[0].Tiles[1].Steps != nil {
		t.Fatalf("stored steps = %v, want the derivation to stay read-time", read.Definition.Sections[0].Tiles[1].Steps)
	}
}

// TestFunnelStepNamesMirrorsTheWebHeuristic pins the Go port of
// web/lib/ia.ts's funnelStepNames: busiest event per matched stage in stage
// order, the activation_event override, the onboarding_verified exclusion,
// and the default contract under two matched stages.
func TestFunnelStepNamesMirrorsTheWebHeuristic(t *testing.T) {
	entry := func(name string) EventCatalogEntry { return EventCatalogEntry{EventName: name} }

	// Volume-descending catalog: the first match per stage is the busiest.
	catalog := []EventCatalogEntry{
		entry("page_view"), entry("app_open"), // both visit — page_view wins
		entry("sign_up"),                      // signup
		entry("onboarding_completed"),         // activation
		entry("checkout"),                     // revenue
		entry("onboarding_verified"),          // receipt, never a step
	}
	got := funnelStepNames(catalog, "")
	want := []string{"page_view", "sign_up", "onboarding_completed", "checkout"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}

	// A configured activation event claims the activation stage by exact
	// match and disables the regex there: onboarding_completed no longer
	// qualifies, and the unconfigured name does.
	got = funnelStepNames(catalog, "my_aha_moment")
	want = []string{"page_view", "sign_up", "checkout"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps with unconfigured activation = %v, want %v", got, want)
	}
	got = funnelStepNames(append(catalog, entry("my_aha_moment")), "my_aha_moment")
	want = []string{"page_view", "sign_up", "my_aha_moment", "checkout"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps with configured activation = %v, want %v", got, want)
	}

	// Fewer than two matched stages serves the default contract.
	got = funnelStepNames([]EventCatalogEntry{entry("page_view")}, "")
	if strings.Join(got, ",") != strings.Join(defaultFunnelSteps, ",") {
		t.Fatalf("thin catalog steps = %v, want %v", got, defaultFunnelSteps)
	}
	got = funnelStepNames(nil, "")
	if strings.Join(got, ",") != strings.Join(defaultFunnelSteps, ",") {
		t.Fatalf("empty catalog steps = %v, want %v", got, defaultFunnelSteps)
	}

	// A name matching several stages is claimed by the last one — the web's
	// `claimed` loop, not the first match.
	if stage := funnelStageOfName("subscription_started", ""); stage != 4 {
		t.Fatalf("subscription_started stage = %d, want 4 (revenue)", stage)
	}
}

func TestBoardDeclarationRefusesAnArchivedBoard(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	name := "Archived"
	board, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "archived", Name: &name, Definition: twoTileBoard(),
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := s.ArchiveDashboard(ctx, projectID, board.Board.ID, board.Board.Revision); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: BoardDefinition{},
	}, "", ""); !errors.Is(err, ErrBoardArchived) {
		t.Fatalf("archived declaration err = %v, want ErrBoardArchived", err)
	}
	// The board key stays resolvable after archive, so an audit can read what
	// the board declared.
	if _, err := s.BoardContentByKey(ctx, projectID, "archived"); err != nil {
		t.Fatalf("read archived board by key: %v", err)
	}
}

func TestBoardDeclarationIsProjectScoped(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	_, otherProject := seedConvProject(t, s)

	name := "Scoped"
	board, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "scoped", Name: &name, Definition: twoTileBoard(),
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := s.BoardContentForProject(ctx, otherProject, board.Board.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project read err = %v, want ErrNoRows", err)
	}
	if _, err := s.BoardContentByKey(ctx, otherProject, "scoped"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project key read err = %v, want ErrNoRows", err)
	}
	// The other project can declare the same key without colliding.
	other, err := s.SaveBoardDefinition(ctx, otherProject, BoardDefinitionWrite{
		BoardKey: "scoped", Name: &name, Definition: BoardDefinition{},
	}, "", "")
	if err != nil || other.Board.ID == board.Board.ID {
		t.Fatalf("second project declare = %+v (%v)", other.Board, err)
	}
	// A declaration cannot move a board to another project's revision fence.
	if _, err := s.SaveBoardDefinition(ctx, otherProject, BoardDefinitionWrite{
		BoardID: board.Board.ID, Definition: BoardDefinition{}, ExpectedRevision: 1,
	}, "", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign board declaration err = %v, want ErrNoRows", err)
	}
}
