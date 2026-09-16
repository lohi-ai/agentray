package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDefaultAnalysisBoardsAreValidDeclarations(t *testing.T) {
	seenKeys := map[string]bool{}
	wantKeys := []string{BoardKeyAcquisition, BoardKeyMonetization, BoardKeyUsage}
	boards := DefaultAnalysisBoards()
	if len(boards) != len(wantKeys) {
		t.Fatalf("got %d boards, want %d", len(boards), len(wantKeys))
	}
	for i, board := range boards {
		if board.Key != wantKeys[i] {
			t.Errorf("board %d key = %q, want %q", i, board.Key, wantKeys[i])
		}
		if seenKeys[board.Key] {
			t.Errorf("board key %q declared twice", board.Key)
		}
		seenKeys[board.Key] = true
		if board.Name == "" || board.Description == "" {
			t.Errorf("board %q needs a name and a description", board.Key)
		}
		normalized, err := normalizeBoardDefinition(board.Definition)
		if err != nil {
			t.Fatalf("board %q refused: %v", board.Key, err)
		}
		if len(normalized.Sections) == 0 {
			t.Errorf("board %q has no sections", board.Key)
		}
		for _, section := range normalized.Sections {
			for _, tile := range section.Tiles {
				if tile.Kind != TileKindMetric {
					t.Errorf("board %q tile %q is %q; default boards declare catalog metrics only", board.Key, tile.Key, tile.Kind)
				}
				if tile.Title == "" {
					t.Errorf("board %q tile %q has no App Store Connect label", board.Key, tile.Key)
				}
				if _, ok := MetricCatalogEntry(tile.Metric); !ok {
					t.Errorf("board %q tile %q names metric %q, which is not in the catalog", board.Key, tile.Key, tile.Metric)
				}
			}
		}
	}
}

func TestDefaultAnalysisBoardsUseAppStoreConnectLabels(t *testing.T) {
	// The 2026-09-14 decision: adopt App Store Connect metric labels on the
	// analysis boards. Values stay AgentRay catalog metrics; the title is the
	// store-shaped name the destination shows.
	want := map[string]string{
		"first-time-downloads": "First-time downloads",
		"proceeds":             "Proceeds",
		"paying-users":         "Paying users",
		"proceeds-per-paying":  "Proceeds per paying user",
		"download-to-paid-d1":  "Download→paid D1",
		"download-to-paid-d7":  "Download→paid D7",
		"download-to-paid-d35": "Download→paid D35",
		"active-devices":       "Active devices",
		"sessions":             "Sessions",
		"sessions-per-device":  "Sessions per device",
		"sessions-daily":       "Sessions per day",
		"retention-d1":         "Average retention D1",
		"retention-d7":         "Average retention D7",
		"retention-d30":        "Average retention D30",
	}
	got := map[string]string{}
	for _, board := range DefaultAnalysisBoards() {
		for _, section := range board.Definition.Sections {
			for _, tile := range section.Tiles {
				got[tile.Key] = tile.Title
			}
		}
	}
	for key, label := range want {
		if got[key] != label {
			t.Errorf("tile %q title = %q, want App Store Connect label %q", key, got[key], label)
		}
	}
	// Apple-shaped metrics with no AgentRay implementation must not sneak in
	// as catalog tiles — that would be a number nobody computes.
	for _, forbidden := range []string{"redownloads", "impressions", "updates", "in-app-purchases", "crashes", "retention-d14", "retention-d28"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("tile %q is declared; it has no catalog metric and must stay an honest empty state", forbidden)
		}
	}
}

func TestEnsureDefaultBoardsIsIdempotent(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	if err := s.EnsureDefaultBoards(ctx, projectID); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	for _, key := range DefaultBoardKeys() {
		got, err := s.BoardContentByKey(ctx, projectID, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		if !got.HasDefinition || got.Board.BoardKey != key || got.Board.Revision != 1 {
			t.Fatalf("%s board = %+v", key, got.Board)
		}
	}

	// A second seed must not bump the revision or replace a board the owner
	// could have edited.
	if err := s.EnsureDefaultBoards(ctx, projectID); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	for _, key := range DefaultBoardKeys() {
		got, err := s.BoardContentByKey(ctx, projectID, key)
		if err != nil {
			t.Fatalf("re-get %s: %v", key, err)
		}
		if got.Board.Revision != 1 {
			t.Errorf("%s revision = %d after re-seed, want 1 (the seed overwrote an existing board)", key, got.Board.Revision)
		}
	}
}

func TestEnsureDefaultBoardsLeavesAnEditedBoardAlone(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	if err := s.EnsureDefaultBoards(ctx, projectID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	current, err := s.BoardContentByKey(ctx, projectID, BoardKeyAcquisition)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	empty := BoardDefinition{Version: BoardDefinitionVersion, Sections: []BoardSection{}}
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID:          current.Board.ID,
		Definition:       empty,
		ExpectedRevision: current.Board.Revision,
	}, "", ""); err != nil {
		t.Fatalf("owner emptied the board: %v", err)
	}

	if err := s.EnsureDefaultBoards(ctx, projectID); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got, err := s.BoardContentByKey(ctx, projectID, BoardKeyAcquisition)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if len(got.Definition.Sections) != 0 {
		t.Fatalf("re-seed restored sections = %+v; an edited board must stay edited", got.Definition.Sections)
	}
}

func TestMissingDefaultBoardIsNoRows(t *testing.T) {
	s := openConvTestStore(t)
	_, err := s.BoardContentByKey(context.Background(), "00000000-0000-0000-0000-000000000000", BoardKeyAcquisition)
	if err == nil || err != pgx.ErrNoRows {
		t.Fatalf("missing board err = %v, want pgx.ErrNoRows", err)
	}
}

func TestDefaultBoardDescriptionsStayHonest(t *testing.T) {
	for _, board := range DefaultAnalysisBoards() {
		blob := board.Name + " " + board.Description
		for _, section := range board.Definition.Sections {
			blob += " " + section.Description
		}
		if strings.Contains(strings.ToLower(blob), "app store connect source") {
			t.Errorf("board %q claims an App Store Connect source — labels only, values are AgentRay", board.Key)
		}
	}
}
