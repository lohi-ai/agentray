package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// boards.go — the declarative board content model.
//
// A board is a `dashboards` row. Its *content* is one document:
// sections, and inside each section the tiles that make up the board. A tile
// either declares a catalog metric (the server computes it, so the tile cannot
// describe a number nobody implemented) or places a saved chart (an existing
// `charts` row, whose query is the artifact). One declaration therefore
// produces a whole board — that is the point: an agent or a preset declares
// the board it wants, instead of issuing a sequence of create calls whose
// half-finished intermediate states are visible to whoever is watching.
//
// The declaration is validated before it is stored, and served back resolved:
// `BoardContent` carries the document, the catalog entries and the chart rows
// its tiles reference, and a warning for each reference that no longer
// resolves. A board still renders the way it always did for callers that read
// `charts` directly — a declared composition is additive to that surface, not a
// replacement of it — and `has_definition` says whether the document was
// actually declared.

// BoardDefinitionVersion is the schema version of the document this server
// writes. Version 0 means "unspecified" and is normalized to the current one;
// any other value is refused rather than guessed at.
const BoardDefinitionVersion = 1

// Tile kinds.
const (
	TileKindMetric = "metric"
	TileKindChart  = "chart"

	TileKindFunnel = "funnel"
)

// Bounds on one declaration. A board is rendered, not paginated, so an
// unbounded document is a denial-of-service on the renderer.
const (
	boardMaxSections        = 12
	boardMaxTilesPerSection = 24
	boardMaxSpan            = 3
	boardKeyMaxLen          = 64
	// funnelMaxSteps bounds one funnel tile's step list. The funnel query
	// chains one correlated subquery per step, so an unbounded list is an
	// unbounded query.
	funnelMaxSteps = 12
)

// ErrBoardDefinitionInvalid wraps every rejected declaration, so an adapter can
// answer 400 with the specific reason instead of a generic failure.
var ErrBoardDefinitionInvalid = errors.New("board definition is invalid")

// ErrBoardArchived rejects a declaration aimed at a soft-archived board: the
// archive is the reversible removal, and writing content into a board the owner
// took off the menu would be a silent resurrection.
var ErrBoardArchived = errors.New("board is archived — unarchive it before declaring content")

var boardKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// BoardTileParams are the knobs a metric tile declares. Empty means "whatever
// the reader selected" — the same meaning the value has on the overview read.
type BoardTileParams struct {
	Period   string `json:"period,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// BoardTile is one placement: a metric the server computes, a saved chart, or
// a funnel over named events.
type BoardTile struct {
	Key   string `json:"key"`
	Title string `json:"title,omitempty"`
	// Kind is "metric", "chart" or "funnel". Empty is inferred from which of
	// Metric / ChartID / Steps is set, and is an error when more than one or
	// none are.
	Kind string `json:"kind,omitempty"`
	// Display is required for metric tiles (stat/line/bar/area/table) and
	// must be empty for chart and funnel tiles — a chart is drawn the way its
	// own kind says and a funnel has one shape, so an override here would be
	// a second, quieter contract.
	Display string `json:"display,omitempty"`
	// Span is the grid width, 1..3; empty means 1.
	Span int `json:"span,omitempty"`
	// Metric is a catalog key (see list_metrics). Required for metric tiles.
	Metric string `json:"metric,omitempty"`
	// Target declares the project-scoped target for this metric (see
	// metric_targets.go). Declaring one appends a version to the metric's
	// target history; omitting it leaves the history untouched — a board
	// never clears a target, only set_metric_target does.
	Target *MetricTargetSpec `json:"target,omitempty"`
	// ChartID places a saved chart that already belongs to this board.
	ChartID string `json:"chart_id,omitempty"`
	// Steps is the funnel tile's ordered event names. Absent means "derive
	// the activation funnel from the project's event catalog" — resolved at
	// read time (see funnel_steps.go), never stored, so the tile tracks the
	// catalog as it grows. Declared means at least two non-empty names.
	Steps  []string         `json:"steps,omitempty"`
	Params *BoardTileParams `json:"params,omitempty"`
}

// BoardSection groups tiles under a heading.
type BoardSection struct {
	Key         string      `json:"key"`
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Tiles       []BoardTile `json:"tiles"`
}

// BoardDefinition is the stored content of one board.
type BoardDefinition struct {
	Version  int            `json:"version"`
	Sections []BoardSection `json:"sections"`
}

// BoardContent is a board served with everything its declaration references:
// the resolved catalog entries, the resolved chart rows, and a warning for any
// reference that no longer resolves. Warnings are served rather than logged
// because the reader of a board is the one who needs to know a tile is
// dangling.
type BoardContent struct {
	Board         Dashboard          `json:"board"`
	HasDefinition bool               `json:"has_definition"`
	Definition    BoardDefinition    `json:"definition"`
	Metrics       []MetricDefinition `json:"metrics"`
	Charts        []Chart            `json:"charts"`
	// Targets is the latest declared target version per metric the tiles
	// reference — including a cleared tombstone, so a declarer can read what
	// it would be restating. The in-force version for a read window is
	// resolved at read time, not here.
	Targets  map[string]MetricTarget `json:"targets"`
	Warnings []string                `json:"warnings"`
}

// BoardDefinitionWrite is one declaration: the document, the board it belongs
// to (by id, or by stable key which creates it when the key is new), and the
// revision the caller believes is current.
type BoardDefinitionWrite struct {
	BoardID     string
	BoardKey    string
	Name        *string
	Description *string
	Definition  BoardDefinition
	// ExpectedRevision fences the write. 0 means "this is a new board": if the
	// key already exists the declaration conflicts rather than overwriting a
	// board the caller has not read.
	ExpectedRevision int64
}

// unmarshalStrict refuses unknown JSON keys. A declaration is versioned, so a
// misspelled field (`sectons`, `tile`, `colour`) is a 400, not a silent drop
// that would store an empty document over the caller's intended content.
func unmarshalStrict(data []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("%w: %v", ErrBoardDefinitionInvalid, err)
	}
	return nil
}

func (d *BoardDefinition) UnmarshalJSON(b []byte) error {
	type alias BoardDefinition
	var a alias
	if err := unmarshalStrict(b, &a); err != nil {
		return err
	}
	*d = BoardDefinition(a)
	return nil
}

func (s *BoardSection) UnmarshalJSON(b []byte) error {
	type alias BoardSection
	var a alias
	if err := unmarshalStrict(b, &a); err != nil {
		return err
	}
	*s = BoardSection(a)
	return nil
}

func (t *BoardTile) UnmarshalJSON(b []byte) error {
	type alias BoardTile
	var a alias
	if err := unmarshalStrict(b, &a); err != nil {
		return err
	}
	*t = BoardTile(a)
	return nil
}

func (p *BoardTileParams) UnmarshalJSON(b []byte) error {
	type alias BoardTileParams
	var a alias
	if err := unmarshalStrict(b, &a); err != nil {
		return err
	}
	*p = BoardTileParams(a)
	return nil
}

// migrateBoards adds the declared-content columns to dashboards. They are
// nullable/defaulted and added with ADD COLUMN IF NOT EXISTS, so an existing
// board keeps rendering from its charts untouched.
func (s *Store) migrateBoards(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS board_key VARCHAR(64) NOT NULL DEFAULT ''`,
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS definition JSONB NOT NULL DEFAULT '{}'::jsonb`,
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS definition_updated_at TIMESTAMPTZ`,
		// A board key is how a declaration addresses its board across runs; it
		// is unique per project when set, and boards without one (every board
		// created before declarations existed) are unaffected.
		`CREATE UNIQUE INDEX IF NOT EXISTS dashboards_project_board_key_idx
ON dashboards (project_id, board_key) WHERE board_key <> ''`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// boardQuerier is the read surface the board content assembly needs: a row
// lookup for the board and a row set for its charts. Both *pgxpool.Pool and
// pgx.Tx satisfy it, so the same assembly runs inside the claim transaction
// (where a replayed declaration returns its first result) and outside it.
type boardQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const boardDefinitionColumns = dashboardColumns + `, definition`

func boardDefinitionScanDest(d *Dashboard, definition *[]byte) []any {
	dest := dashboardScanDest(d)
	return append(dest, definition)
}

// BoardContentForProject reads one board's declared content by id.
func (s *Store) BoardContentForProject(ctx context.Context, projectID, boardID string) (BoardContent, error) {
	return s.boardContent(ctx, s.pg, projectID, boardID, "")
}

// BoardContentByKey reads one board's declared content by its stable key.
func (s *Store) BoardContentByKey(ctx context.Context, projectID, boardKey string) (BoardContent, error) {
	return s.boardContent(ctx, s.pg, projectID, "", boardKey)
}

func (s *Store) boardContent(ctx context.Context, q boardQuerier, projectID, boardID, boardKey string) (BoardContent, error) {
	where := `project_id = $1 AND id = $2`
	arg := boardID
	if boardID == "" {
		where = `project_id = $1 AND board_key = $2 AND board_key <> ''`
		arg = boardKey
	}
	var board Dashboard
	var definition []byte
	err := q.QueryRow(ctx, `SELECT `+boardDefinitionColumns+` FROM dashboards WHERE `+where, projectID, arg).
		Scan(boardDefinitionScanDest(&board, &definition)...)
	if err != nil {
		return BoardContent{}, err
	}
	content, err := resolveBoardContent(ctx, q, board, definition)
	if err != nil {
		return BoardContent{}, err
	}
	// Funnel tiles that declared no steps derive them from the event catalog
	// here, outside any transaction — the catalog lives in DuckDB, which the
	// board querier cannot reach.
	s.resolveFunnelSteps(ctx, projectID, &content)
	return content, nil
}

// resolveBoardContent decodes the stored document and resolves everything the
// tiles reference. An undeclared board returns its (empty) definition with
// has_definition=false, so a caller can tell "no composition declared yet" from
// "declared empty" without inferring it from the section count.
func resolveBoardContent(ctx context.Context, q boardQuerier, board Dashboard, raw []byte) (BoardContent, error) {
	content := BoardContent{
		Board:         board,
		HasDefinition: board.HasDefinition(),
		Definition:    BoardDefinition{Version: BoardDefinitionVersion, Sections: []BoardSection{}},
		Metrics:       []MetricDefinition{},
		Charts:        []Chart{},
		Targets:       map[string]MetricTarget{},
		Warnings:      []string{},
	}
	definition, err := decodeBoardDefinition(raw)
	if err != nil {
		return BoardContent{}, err
	}
	content.Definition = definition
	if !content.HasDefinition {
		return content, nil
	}

	metricKeys := []string{}
	chartIDs := []string{}
	for _, section := range definition.Sections {
		for _, tile := range section.Tiles {
			switch tile.Kind {
			case TileKindMetric:
				metricKeys = append(metricKeys, tile.Metric)
			case TileKindChart:
				chartIDs = append(chartIDs, tile.ChartID)
			}
		}
	}

	catalog, err := metricDefinitionsByKeys(ctx, q, metricKeys)
	if err != nil {
		return BoardContent{}, err
	}
	charts, err := chartsByIDs(ctx, q, board.ProjectID, board.ID, chartIDs)
	if err != nil {
		return BoardContent{}, err
	}
	targets, err := latestMetricTargets(ctx, q, board.ProjectID, metricKeys)
	if err != nil {
		return BoardContent{}, err
	}
	content.Targets = targets

	metricsSeen := map[string]bool{}
	chartsSeen := map[string]bool{}
	for _, section := range definition.Sections {
		for _, tile := range section.Tiles {
			where := fmt.Sprintf("section %q tile %q", section.Key, tile.Key)
			switch tile.Kind {
			case TileKindMetric:
				def, ok := catalog[tile.Metric]
				if !ok {
					content.Warnings = append(content.Warnings, where+": metric "+tile.Metric+" is no longer in the catalog — the tile cannot be computed")
					continue
				}
				if !metricsSeen[tile.Metric] {
					metricsSeen[tile.Metric] = true
					content.Metrics = append(content.Metrics, def)
				}
			case TileKindChart:
				chart, ok := charts[tile.ChartID]
				if !ok {
					content.Warnings = append(content.Warnings, where+": chart "+tile.ChartID+" is not on this board — the tile has no query")
					continue
				}
				if chart.ArchivedAt != nil {
					content.Warnings = append(content.Warnings, where+": chart "+chart.Name+" is archived — the tile is dangling until it is unarchived")
				}
				if !chartsSeen[tile.ChartID] {
					chartsSeen[tile.ChartID] = true
					content.Charts = append(content.Charts, chart)
				}
			}
		}
	}
	return content, nil
}

// decodeBoardDefinition reads the stored JSON. An unwritten (`{}`, `null` or
// empty) document is the empty definition, not an error: every board created
// before this model existed has exactly that.
func decodeBoardDefinition(raw []byte) (BoardDefinition, error) {
	empty := BoardDefinition{Version: BoardDefinitionVersion, Sections: []BoardSection{}}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return empty, nil
	}
	var def BoardDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		return BoardDefinition{}, fmt.Errorf("stored board definition is unreadable: %w", err)
	}
	if def.Version == 0 {
		def.Version = BoardDefinitionVersion
	}
	if def.Sections == nil {
		def.Sections = []BoardSection{}
	}
	for i := range def.Sections {
		if def.Sections[i].Tiles == nil {
			def.Sections[i].Tiles = []BoardTile{}
		}
	}
	return def, nil
}

func metricDefinitionsByKeys(ctx context.Context, q boardQuerier, keys []string) (map[string]MetricDefinition, error) {
	out := map[string]MetricDefinition{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+metricDefinitionColumns+` FROM metric_definitions WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d MetricDefinition
		if err := rows.Scan(metricDefinitionScanDest(&d)...); err != nil {
			return nil, err
		}
		out[d.Key] = d
	}
	return out, rows.Err()
}

func chartsByIDs(ctx context.Context, q boardQuerier, projectID, boardID string, ids []string) (map[string]Chart, error) {
	out := map[string]Chart{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		`SELECT `+chartColumns+` FROM charts WHERE project_id = $1 AND dashboard_id = $2 AND id = ANY($3)`,
		projectID, boardID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Chart
		if err := rows.Scan(chartScanDest(&c)...); err != nil {
			return nil, err
		}
		out[c.ID] = c
	}
	return out, rows.Err()
}

// SaveBoardDefinition declares a board's content. The document replaces the
// board's content wholesale — that is what makes a declaration convergent: the
// same document declared twice leaves the same board, and a board with no
// declared content is declared empty rather than left ambiguous.
//
// The write is fenced by the board's revision and recorded under the
// idempotency key, so a retry returns the first result and a concurrent
// declarer conflicts instead of silently winning.
func (s *Store) SaveBoardDefinition(ctx context.Context, projectID string, in BoardDefinitionWrite, idemKey, requestHash string) (BoardContent, error) {
	raw, err := s.runIdempotent(ctx, projectID, "save_board", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			var content BoardContent
			err := withTx(ctx, q, func(tx pgx.Tx) error {
				c, err := saveBoardDefinition(ctx, tx, projectID, in)
				if err != nil {
					return err
				}
				content = c
				return nil
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(content)
		})
	if err != nil {
		return BoardContent{}, err
	}
	var content BoardContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return BoardContent{}, fmt.Errorf("stored board receipt is unreadable: %w", err)
	}
	// Resolve funnel steps after the transaction, the same way a read does:
	// the catalog lives in DuckDB, which the save transaction cannot reach —
	// and a replayed declaration returns its stored receipt, so resolution
	// must happen on the served copy or a replay would serve empty steps.
	s.resolveFunnelSteps(ctx, projectID, &content)
	return content, nil
}

func saveBoardDefinition(ctx context.Context, tx pgx.Tx, projectID string, in BoardDefinitionWrite) (BoardContent, error) {
	definition, err := normalizeBoardDefinition(in.Definition)
	if err != nil {
		return BoardContent{}, err
	}
	key := strings.TrimSpace(in.BoardKey)
	if key != "" && !boardKeyRe.MatchString(key) {
		return BoardContent{}, fmt.Errorf("%w: board_key %q must be %d characters or fewer, lower-case letters, digits, dash or underscore, starting with a letter or digit", ErrBoardDefinitionInvalid, key, boardKeyMaxLen)
	}
	if in.BoardID == "" && key == "" {
		return BoardContent{}, fmt.Errorf("%w: declare a board_id to update an existing board, or a board_key to create one", ErrBoardDefinitionInvalid)
	}

	var board Dashboard
	var stored []byte
	existing, err := lockBoard(ctx, tx, projectID, in.BoardID, key)
	switch {
	case err == nil:
		// The row is locked, so this verdict is final for the rest of the
		// transaction: an archived board is refused before the revision is
		// considered, because no revision can be declared against it until it
		// is unarchived.
		if existing.ArchivedAt != nil {
			return BoardContent{}, ErrBoardArchived
		}
		if in.ExpectedRevision <= 0 {
			return BoardContent{}, fmt.Errorf("%w: board %q already exists at revision %d — read it with get_board and declare with that revision", ErrRevisionConflict, boardLabel(existing), existing.Revision)
		}
		if err := checkChartReferences(ctx, tx, projectID, existing.ID, definition); err != nil {
			return BoardContent{}, err
		}
		board, stored, err = updateBoardDefinition(ctx, tx, projectID, existing, key, in, definition)
	case errors.Is(err, pgx.ErrNoRows):
		if in.BoardID != "" {
			return BoardContent{}, pgx.ErrNoRows
		}
		if in.ExpectedRevision > 0 {
			// The caller read a board that is gone: not found, not a
			// conflict — re-declaring cannot be resolved by re-reading.
			return BoardContent{}, pgx.ErrNoRows
		}
		// A brand-new board has no charts, so a chart tile could only refer to
		// a chart that does not exist. Say so instead of storing a dangling
		// board.
		if err := checkChartReferences(ctx, tx, projectID, "", definition); err != nil {
			return BoardContent{}, err
		}
		board, stored, err = insertBoardDefinition(ctx, tx, projectID, key, in, definition)
	default:
		return BoardContent{}, err
	}
	if err != nil {
		return BoardContent{}, err
	}
	// The board row is written; now append the target versions the tiles
	// declare, inside the same transaction — a refused target rolls the board
	// write back with it, so a declaration is never half-applied.
	if err := applyBoardTargets(ctx, tx, projectID, definition); err != nil {
		return BoardContent{}, err
	}
	return resolveBoardContent(ctx, tx, board, stored)
}

// applyBoardTargets appends one target version per metric the document
// declares a target for. A tile without a target declares nothing — the
// project-scoped history is never cleared by omission, so one board cannot
// silently remove a target another surface set.
func applyBoardTargets(ctx context.Context, tx pgx.Tx, projectID string, definition BoardDefinition) error {
	for _, section := range definition.Sections {
		for _, tile := range section.Tiles {
			if tile.Kind != TileKindMetric || tile.Target == nil {
				continue
			}
			if _, err := setMetricTarget(ctx, tx, projectID, MetricTargetWrite{Metric: tile.Metric, Spec: *tile.Target}); err != nil {
				return fmt.Errorf("section %q tile %q: %w", section.Key, tile.Key, err)
			}
		}
	}
	return nil
}

func boardLabel(d Dashboard) string {
	if d.BoardKey != "" {
		return d.BoardKey
	}
	return d.Name
}

// lockBoard finds the declaration's target, taking a row lock so two
// concurrent declarations of the same board cannot both pass their revision
// check. id wins over key when both are given, and a key that matches nothing
// is ErrNoRows, which the caller reads as "this is a create".
func lockBoard(ctx context.Context, tx pgx.Tx, projectID, id, key string) (Dashboard, error) {
	var board Dashboard
	var definition []byte
	where := `project_id = $1 AND id = $2`
	arg := id
	if id == "" {
		where = `project_id = $1 AND board_key = $2 AND board_key <> ''`
		arg = key
	}
	err := tx.QueryRow(ctx, `SELECT `+boardDefinitionColumns+` FROM dashboards WHERE `+where+` FOR UPDATE`, projectID, arg).
		Scan(boardDefinitionScanDest(&board, &definition)...)
	return board, err
}

func insertBoardDefinition(ctx context.Context, tx pgx.Tx, projectID, key string, in BoardDefinitionWrite, definition BoardDefinition) (Dashboard, []byte, error) {
	name := "Untitled dashboard"
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		name = strings.TrimSpace(*in.Name)
	}
	description := ""
	if in.Description != nil {
		description = *in.Description
	}
	payload, err := json.Marshal(definition)
	if err != nil {
		return Dashboard{}, nil, err
	}
	var board Dashboard
	var stored []byte
	err = tx.QueryRow(ctx, `
INSERT INTO dashboards (project_id, name, description, board_key, definition, definition_updated_at)
VALUES ($1, $2, $3, $4, $5, now())
RETURNING `+boardDefinitionColumns, projectID, name, description, key, payload).
		Scan(boardDefinitionScanDest(&board, &stored)...)
	if err != nil {
		// Two declarations can race between "the key is free" (the FOR UPDATE
		// found nothing to lock) and this insert; the unique index settles it
		// and a conflict is what the loser needs to hear, so it can re-read the
		// board the winner created instead of parsing a constraint name.
		if isUniqueViolation(err) {
			return Dashboard{}, nil, fmt.Errorf("%w: board key %q was just taken — read it with get_board and declare with its revision", ErrRevisionConflict, key)
		}
		return Dashboard{}, nil, err
	}
	return board, stored, nil
}

func updateBoardDefinition(ctx context.Context, tx pgx.Tx, projectID string, existing Dashboard, key string, in BoardDefinitionWrite, definition BoardDefinition) (Dashboard, []byte, error) {
	payload, err := json.Marshal(definition)
	if err != nil {
		return Dashboard{}, nil, err
	}
	var board Dashboard
	var stored []byte
	err = tx.QueryRow(ctx, `
UPDATE dashboards
SET name = CASE WHEN $3::text IS NULL THEN name
                WHEN $3 = '' THEN 'Untitled dashboard'
                ELSE $3 END,
    description = COALESCE($4::text, description),
    board_key = CASE WHEN $6::text = '' THEN board_key ELSE $6 END,
    definition = $5,
    definition_updated_at = now(),
    revision = revision + 1,
    updated_at = now()
WHERE project_id = $1 AND id = $2 AND revision = $7 AND archived_at IS NULL
RETURNING `+boardDefinitionColumns,
		projectID, existing.ID, in.Name, in.Description, payload, key, in.ExpectedRevision).
		Scan(boardDefinitionScanDest(&board, &stored)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the revision moved or the board was archived underneath
			// the declaration — both are "the board changed, re-read it".
			return Dashboard{}, nil, fmt.Errorf("%w: board %q changed under the declaration (expected revision %d)", ErrRevisionConflict, boardLabel(existing), in.ExpectedRevision)
		}
		// A declaration may name the board it updates; if that key belongs to
		// another board, the index refuses and the declarer has to choose.
		if isUniqueViolation(err) {
			return Dashboard{}, nil, fmt.Errorf("%w: board key %q belongs to another board", ErrRevisionConflict, key)
		}
		return Dashboard{}, nil, err
	}
	return board, stored, nil
}

// checkChartReferences rejects a chart tile whose chart is not on this board.
// The chart's own row is the artifact; a tile may place it, never invent it.
func checkChartReferences(ctx context.Context, tx pgx.Tx, projectID, boardID string, definition BoardDefinition) error {
	ids := []string{}
	for _, section := range definition.Sections {
		for _, tile := range section.Tiles {
			if tile.Kind == TileKindChart {
				ids = append(ids, tile.ChartID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if boardID == "" {
		return fmt.Errorf("%w: a chart tile must place a chart that already belongs to the board — create the board and its charts first, then declare them", ErrBoardDefinitionInvalid)
	}
	found := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT id::text FROM charts WHERE project_id = $1 AND dashboard_id = $2 AND id = ANY($3)`, projectID, boardID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, section := range definition.Sections {
		for _, tile := range section.Tiles {
			if tile.Kind == TileKindChart && !found[tile.ChartID] {
				return fmt.Errorf("%w: tile %q references chart %s, which is not on this board", ErrBoardDefinitionInvalid, tile.Key, tile.ChartID)
			}
		}
	}
	return nil
}

// normalizeBoardDefinition trims, defaults and validates a declaration. Every
// rejection names the offending section or tile and what to do about it — a
// declaration is composed by a model as often as by a person, and "invalid"
// without a reason costs a round trip.
func normalizeBoardDefinition(def BoardDefinition) (BoardDefinition, error) {
	if def.Version == 0 {
		def.Version = BoardDefinitionVersion
	}
	if def.Version != BoardDefinitionVersion {
		return BoardDefinition{}, fmt.Errorf("%w: definition version %d is not supported (this server declares version %d)", ErrBoardDefinitionInvalid, def.Version, BoardDefinitionVersion)
	}
	if def.Sections == nil {
		def.Sections = []BoardSection{}
	}
	if len(def.Sections) > boardMaxSections {
		return BoardDefinition{}, fmt.Errorf("%w: %d sections exceeds the maximum of %d", ErrBoardDefinitionInvalid, len(def.Sections), boardMaxSections)
	}
	sectionKeys := map[string]bool{}
	tileKeys := map[string]bool{}
	// One document may place the same metric twice, but it may not declare two
	// different targets for it: the history is project-scoped, so the second
	// write would overwrite the first inside one save.
	targetDecls := map[string]MetricTargetSpec{}
	for i := range def.Sections {
		section := &def.Sections[i]
		section.Key = strings.TrimSpace(section.Key)
		section.Title = strings.TrimSpace(section.Title)
		if !boardKeyRe.MatchString(section.Key) {
			return BoardDefinition{}, fmt.Errorf("%w: section %d has key %q; a key is 1-%d characters of lower-case letters, digits, dash or underscore, starting with a letter or digit", ErrBoardDefinitionInvalid, i+1, section.Key, boardKeyMaxLen)
		}
		if sectionKeys[section.Key] {
			return BoardDefinition{}, fmt.Errorf("%w: section key %q is declared twice", ErrBoardDefinitionInvalid, section.Key)
		}
		sectionKeys[section.Key] = true
		if section.Title == "" {
			return BoardDefinition{}, fmt.Errorf("%w: section %q has no title", ErrBoardDefinitionInvalid, section.Key)
		}
		if len(section.Tiles) > boardMaxTilesPerSection {
			return BoardDefinition{}, fmt.Errorf("%w: section %q declares %d tiles, more than the maximum of %d", ErrBoardDefinitionInvalid, section.Key, len(section.Tiles), boardMaxTilesPerSection)
		}
		for j := range section.Tiles {
			tile := &section.Tiles[j]
			if err := normalizeBoardTile(tile); err != nil {
				return BoardDefinition{}, fmt.Errorf("section %q: %w", section.Key, err)
			}
			if tileKeys[tile.Key] {
				return BoardDefinition{}, fmt.Errorf("%w: tile key %q is declared twice", ErrBoardDefinitionInvalid, tile.Key)
			}
			tileKeys[tile.Key] = true
			if tile.Kind == TileKindMetric && tile.Target != nil {
				if prev, ok := targetDecls[tile.Metric]; ok && prev != *tile.Target {
					return BoardDefinition{}, fmt.Errorf("%w: tiles declare two different targets for metric %q — a target is project-scoped, so one document can only declare it once", ErrBoardDefinitionInvalid, tile.Metric)
				}
				targetDecls[tile.Metric] = *tile.Target
			}
		}
	}
	return def, nil
}

func normalizeBoardTile(tile *BoardTile) error {
	tile.Key = strings.TrimSpace(tile.Key)
	tile.Title = strings.TrimSpace(tile.Title)
	tile.Kind = strings.ToLower(strings.TrimSpace(tile.Kind))
	tile.Display = strings.ToLower(strings.TrimSpace(tile.Display))
	tile.Metric = strings.TrimSpace(tile.Metric)
	tile.ChartID = strings.TrimSpace(tile.ChartID)
	for i := range tile.Steps {
		tile.Steps[i] = strings.TrimSpace(tile.Steps[i])
	}
	if !boardKeyRe.MatchString(tile.Key) {
		return fmt.Errorf("%w: tile key %q must be 1-%d characters of lower-case letters, digits, dash or underscore, starting with a letter or digit", ErrBoardDefinitionInvalid, tile.Key, boardKeyMaxLen)
	}
	declared := 0
	for _, set := range []bool{tile.Metric != "", tile.ChartID != "", len(tile.Steps) > 0} {
		if set {
			declared++
		}
	}
	if tile.Kind == "" {
		declared := 0
		for _, set := range []bool{tile.Metric != "", tile.ChartID != "", tile.Steps != nil} {
			if set {
				declared++
			}
		}
		switch {
		case declared > 1:
			return fmt.Errorf("%w: tile %q declares more than one of metric, chart_id and steps; a tile is one subject", ErrBoardDefinitionInvalid, tile.Key)
		case tile.Metric != "":
			tile.Kind = TileKindMetric
		case tile.ChartID != "":
			tile.Kind = TileKindChart
		case tile.Steps != nil:
			tile.Kind = TileKindFunnel
		default:
			return fmt.Errorf("%w: tile %q declares neither a metric nor a chart nor funnel steps", ErrBoardDefinitionInvalid, tile.Key)
		}
	}
	switch tile.Kind {
	case TileKindMetric:
		if tile.ChartID != "" {
			return fmt.Errorf("%w: tile %q is a metric tile and cannot also place a chart", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Steps != nil {
			return fmt.Errorf("%w: tile %q is a metric tile and cannot also declare funnel steps", ErrBoardDefinitionInvalid, tile.Key)
		}
		def, ok := MetricCatalogEntry(tile.Metric)
		if !ok {
			return fmt.Errorf("%w: tile %q references metric %q, which is not in the catalog (known metrics: %s)", ErrBoardDefinitionInvalid, tile.Key, tile.Metric, strings.Join(MetricKeys(), ", "))
		}
		if tile.Display == "" {
			tile.Display = def.DefaultDisplay()
		}
		if !def.AllowsDisplay(tile.Display) {
			return fmt.Errorf("%w: tile %q draws metric %q as %q; a %s metric can be drawn as %s", ErrBoardDefinitionInvalid, tile.Key, def.Key, tile.Display, def.Kind, strings.Join(def.Displays, ", "))
		}
		if tile.Params != nil {
			// Normalize what is validated: an accepted period with surrounding
			// whitespace was being stored verbatim, and the read that consumes
			// it validates the stored string against an anchored pattern — so
			// the board would declare a range it could never read.
			tile.Params.Period = strings.TrimSpace(tile.Params.Period)
			tile.Params.Platform = strings.ToLower(strings.TrimSpace(tile.Params.Platform))
			if err := validateTileParams(tile.Params); err != nil {
				return fmt.Errorf("tile %q: %w", tile.Key, err)
			}
		}
		if tile.Target != nil {
			spec, err := normalizeMetricTargetSpec(def, *tile.Target)
			if err != nil {
				return fmt.Errorf("tile %q: %w", tile.Key, err)
			}
			*tile.Target = spec
		}
	case TileKindChart:
		if tile.Metric != "" {
			return fmt.Errorf("%w: tile %q is a chart tile and cannot also reference a metric", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Steps != nil {
			return fmt.Errorf("%w: tile %q is a chart tile and cannot also declare funnel steps", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.ChartID == "" {
			return fmt.Errorf("%w: tile %q is a chart tile with no chart_id", ErrBoardDefinitionInvalid, tile.Key)
		}
		id, err := uuid.Parse(tile.ChartID)
		if err != nil {
			return fmt.Errorf("%w: tile %q has chart_id %q, which is not a chart id", ErrBoardDefinitionInvalid, tile.Key, tile.ChartID)
		}
		tile.ChartID = id.String()
		if tile.Display != "" {
			return fmt.Errorf("%w: tile %q sets display %q on a chart tile; a chart is drawn the way its own kind says", ErrBoardDefinitionInvalid, tile.Key, tile.Display)
		}
		if tile.Params != nil {
			return fmt.Errorf("%w: tile %q sets params on a chart tile; the chart's own query and the reader's range decide its window", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Target != nil {
			return fmt.Errorf("%w: tile %q sets a target on a chart tile; a target belongs to a catalog metric", ErrBoardDefinitionInvalid, tile.Key)
		}
	case TileKindFunnel:
		if tile.Metric != "" {
			return fmt.Errorf("%w: tile %q is a funnel tile and cannot also reference a metric", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.ChartID != "" {
			return fmt.Errorf("%w: tile %q is a funnel tile and cannot also place a chart", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Display != "" {
			return fmt.Errorf("%w: tile %q sets display %q on a funnel tile; a funnel has one shape", ErrBoardDefinitionInvalid, tile.Key, tile.Display)
		}
		if tile.Params != nil {
			return fmt.Errorf("%w: tile %q sets params on a funnel tile; the reader's range and platform decide its window", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Target != nil {
			return fmt.Errorf("%w: tile %q sets a target on a funnel tile; a target belongs to a catalog metric", ErrBoardDefinitionInvalid, tile.Key)
		}
		if tile.Steps != nil {
			// Declared steps are the funnel verbatim — every entry must name
			// an event, and a funnel needs at least two stages to say
			// anything. Absent (nil) means auto-resolve at read time.
			if len(tile.Steps) < 2 {
				return fmt.Errorf("%w: tile %q declares %d funnel step; a funnel needs at least 2 — omit steps to derive them from the event catalog", ErrBoardDefinitionInvalid, tile.Key, len(tile.Steps))
			}
			if len(tile.Steps) > funnelMaxSteps {
				return fmt.Errorf("%w: tile %q declares %d funnel steps, more than the maximum of %d", ErrBoardDefinitionInvalid, tile.Key, len(tile.Steps), funnelMaxSteps)
			}
			for i, step := range tile.Steps {
				if step == "" {
					return fmt.Errorf("%w: tile %q has an empty funnel step at position %d", ErrBoardDefinitionInvalid, tile.Key, i+1)
				}
			}
		}
	default:
		return fmt.Errorf("%w: tile %q has kind %q; a tile is %q, %q or %q", ErrBoardDefinitionInvalid, tile.Key, tile.Kind, TileKindMetric, TileKindChart, TileKindFunnel)
	}
	switch {
	case tile.Span == 0:
		tile.Span = 1
	case tile.Span < 1 || tile.Span > boardMaxSpan:
		return fmt.Errorf("%w: tile %q has span %d; a tile spans 1 to %d grid columns", ErrBoardDefinitionInvalid, tile.Key, tile.Span, boardMaxSpan)
	}
	return nil
}

// validateTileParams checks the already-normalized values a metric tile
// declared. The caller trims and lower-cases before calling, so what is
// validated here is exactly what gets stored.
func validateTileParams(params *BoardTileParams) error {
	if err := validPeriod(params.Period); err != nil {
		return fmt.Errorf("%w: period %q is not part of the range contract", ErrBoardDefinitionInvalid, params.Period)
	}
	switch params.Platform {
	case "", PlatformWeb, PlatformIOS, PlatformAndroid, PlatformServer, PlatformUnknown:
	default:
		return fmt.Errorf("%w: platform %q is unknown; use web, ios, android, server, unknown, or empty for every platform", ErrBoardDefinitionInvalid, params.Platform)
	}
	return nil
}
