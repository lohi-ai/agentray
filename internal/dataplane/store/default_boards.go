package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// Default analysis boards. Each is a 007 declaration addressed by board_key,
// seeded onto every project so Acquisition / Monetization / Usage are real
// destinations rather than Coming soon affordances.
//
// Tile titles follow App Store Connect analytics labels (decision 2026-09-14).
// Values stay AgentRay's: the tile names a catalog metric the overview read
// already computes. Apple-shaped metrics with no AgentRay implementation are
// not declared here — the catalog refuses a metric nobody computes — and the
// analysis pages render those as named empty states instead of inventing a
// figure.
const (
	BoardKeyAcquisition  = "acquisition"
	BoardKeyMonetization = "monetization"
	BoardKeyUsage        = "usage"
)

// DefaultAnalysisBoard is one seeded analysis destination: the stable key the
// menu addresses it by, the name shown in list_dashboards, and the document
// get_board serves.
type DefaultAnalysisBoard struct {
	Key         string
	Name        string
	Description string
	Definition  BoardDefinition
}

func defaultTile(key, metric, display, title string, span int) BoardTile {
	return BoardTile{
		Key:     key,
		Kind:    TileKindMetric,
		Metric:  metric,
		Display: display,
		Title:   title,
		Span:    span,
	}
}

// DefaultAnalysisBoards is the declaration the seed writes. Order is the
// analysis menu order: Acquisition, Monetization, Usage.
func DefaultAnalysisBoards() []DefaultAnalysisBoard {
	return []DefaultAnalysisBoard{
		{
			Key:         BoardKeyAcquisition,
			Name:        "Acquisition",
			Description: "How people arrive. Labels follow App Store Connect; values are AgentRay first-observed people and attributed pageviews — not store downloads or impressions.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{{
					Key:         "kpis",
					Title:       "Acquisition",
					Description: "First-time downloads is AgentRay new people (first observed qualifying activity). Ranked pages and sources stay visible, including Direct / unknown.",
					Tiles: []BoardTile{
						defaultTile("first-time-downloads", MetricNewUsers, DisplayStat, "First-time downloads", 1),
						defaultTile("top-pages", MetricTopPages, DisplayBar, "Top pages", 2),
						defaultTile("top-sources", MetricTopSources, DisplayBar, "Top acquisition sources", 2),
					},
				}},
			},
		},
		{
			Key:         BoardKeyMonetization,
			Name:        "Monetization",
			Description: "Proceeds is AgentRay net revenue from a trusted billing source. Paying users, proceeds per paying user and download→paid cohorts compute from the same deduplicated money grid; in-app purchase counts are Not available until a purchase metric exists.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{{
					Key:         "kpis",
					Title:       "Monetization",
					Description: "Proceeds uses AgentRay's deduplicated net-money read. Unlike currencies are never summed; proceeds per paying person stays inside the headline currency.",
					Tiles: []BoardTile{
						defaultTile("proceeds", MetricRevenue, DisplayStat, "Proceeds", 1),
						defaultTile("paying-users", MetricPayingUsers, DisplayStat, "Paying users", 1),
						defaultTile("proceeds-per-paying", MetricProceedsPerPaying, DisplayStat, "Proceeds per paying user", 1),
						defaultTile("download-to-paid-d1", MetricDownloadToPaidD1, DisplayStat, "Download→paid D1", 1),
						defaultTile("download-to-paid-d7", MetricDownloadToPaidD7, DisplayStat, "Download→paid D7", 1),
						defaultTile("download-to-paid-d35", MetricDownloadToPaidD35, DisplayStat, "Download→paid D35", 1),
					},
				}},
			},
		},
		{
			Key:         BoardKeyUsage,
			Name:        "Usage",
			Description: "Active devices, sessions and retention from AgentRay qualifying activity. Average retention D14 and crashes are Not available — AgentRay serves D1/D7/D30 and has no crash contract.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{
					{
						Key:         "kpis",
						Title:       "Usage",
						Description: "Active devices is AgentRay active people. Sessions per person is App Store Connect's sessions-per-device read. Retention points are mature lifetime first-activity cohorts; immature cohorts render Not ready, never 0%.",
						Tiles: []BoardTile{
							defaultTile("active-devices", MetricActiveUsers, DisplayStat, "Active devices", 1),
							defaultTile("sessions", MetricSessions, DisplayStat, "Sessions", 1),
							defaultTile("sessions-per-device", MetricSessionsPerUser, DisplayStat, "Sessions per device", 1),
							defaultTile("retention-d1", MetricRetentionD1, DisplayStat, "Average retention D1", 1),
							defaultTile("retention-d7", MetricRetentionD7, DisplayStat, "Average retention D7", 1),
							defaultTile("retention-d30", MetricRetentionD30, DisplayStat, "Average retention D30", 1),
						},
					},
					{
						Key:         "trend",
						Title:       "Daily activity",
						Description: "Daily distinct people and sessions. Daily counts are never summed into the period total.",
						Tiles: []BoardTile{
							defaultTile("active-devices-daily", MetricActiveUsersDaily, DisplayArea, "Active devices per day", 3),
							defaultTile("sessions-daily", MetricSessionsDaily, DisplayArea, "Sessions per day", 3),
						},
					},
					{
						Key:         "funnel",
						Title:       "Where do new users drop off?",
						Description: "Step-by-step conversion over the selected range. No declared steps — the funnel is derived from the project's event catalog at read time.",
						Tiles: []BoardTile{
							{Key: "activation-funnel", Kind: TileKindFunnel, Title: "Activation funnel", Span: 3},
						},
					},
				},
			},
		},
	}
}

// previousDefaultBoards is the declaration the seed shipped before the v6
// metrics existed, verbatim. It is the repair's "is this still the seeded
// board?" test: a stored definition that no longer matches it byte-for-byte
// (as JSONB — order and whitespace aside) is a board somebody edited, and a
// board somebody edited is never rewritten by boot.
func previousDefaultBoards() map[string]BoardDefinition {
	return map[string]BoardDefinition{
		BoardKeyMonetization: {
			Version: BoardDefinitionVersion,
			Sections: []BoardSection{{
				Key:         "kpis",
				Title:       "Monetization",
				Description: "Proceeds uses AgentRay's deduplicated net-money read. Unlike currencies are never summed.",
				Tiles: []BoardTile{
					defaultTile("proceeds", MetricRevenue, DisplayStat, "Proceeds", 1),
				},
			}},
		},
		BoardKeyUsage: {
			Version: BoardDefinitionVersion,
			Sections: []BoardSection{
				{
					Key:         "kpis",
					Title:       "Usage",
					Description: "Active devices is AgentRay active people. Retention points are mature lifetime first-activity cohorts; immature cohorts render Not ready, never 0%.",
					Tiles: []BoardTile{
						defaultTile("active-devices", MetricActiveUsers, DisplayStat, "Active devices", 1),
						defaultTile("sessions", MetricSessions, DisplayStat, "Sessions", 1),
						defaultTile("retention-d1", MetricRetentionD1, DisplayStat, "Average retention D1", 1),
						defaultTile("retention-d7", MetricRetentionD7, DisplayStat, "Average retention D7", 1),
						defaultTile("retention-d30", MetricRetentionD30, DisplayStat, "Average retention D30", 1),
					},
				},
				{
					Key:         "trend",
					Title:       "Active devices per day",
					Description: "Daily distinct people. Daily counts are never summed into the period total.",
					Tiles: []BoardTile{
						defaultTile("active-devices-daily", MetricActiveUsersDaily, DisplayArea, "Active devices per day", 3),
					},
				},
			},
		},
	}
}

// RepairDefaultBoards upgrades the seeded analysis boards on projects that
// already have them. The seed only ever writes an absent key, so a project
// created before a metric existed keeps a Monetization board that says
// "Not available" forever — this is the other half of the contract.
//
// The predicate is an exact JSONB match on the previously-shipped definition:
// it can only ever touch a row that IS the untouched seed. A board the owner
// or an agent edited — one tile moved, one title changed — stops matching and
// is left alone, which is the same guarantee repairSeededCharts makes for
// seeded queries. Idempotent: after the rewrite the row holds the fresh
// definition, which matches nothing in previousDefaultBoards.
//
// Definition and description move together (the seeded description is where
// the stale "Not available" claim lives); name is deliberately not written —
// a rename is a customization worth keeping even on an otherwise-untouched
// board. Returns rows repaired and the projects that own them.
func (s *Store) RepairDefaultBoards(ctx context.Context) (rows int64, projects int64, err error) {
	previous := previousDefaultBoards()
	current := make(map[string]DefaultAnalysisBoard, 3)
	for _, board := range DefaultAnalysisBoards() {
		current[board.Key] = board
	}
	var totalRows, totalProjects int64
	for key, staleDef := range previous {
		board, ok := current[key]
		if !ok {
			continue
		}
		staleNorm, err := normalizeBoardDefinition(staleDef)
		if err != nil {
			return totalRows, totalProjects, fmt.Errorf("repair %s board: %w", key, err)
		}
		staleJSON, err := json.Marshal(staleNorm)
		if err != nil {
			return totalRows, totalProjects, fmt.Errorf("repair %s board: %w", key, err)
		}
		freshNorm, err := normalizeBoardDefinition(board.Definition)
		if err != nil {
			return totalRows, totalProjects, fmt.Errorf("repair %s board: %w", key, err)
		}
		freshJSON, err := json.Marshal(freshNorm)
		if err != nil {
			return totalRows, totalProjects, fmt.Errorf("repair %s board: %w", key, err)
		}
		var repaired, touched int64
		err = s.pg.QueryRow(ctx, `
WITH repaired AS (
	UPDATE dashboards
	SET definition = $2::jsonb, description = $3, definition_updated_at = now()
	WHERE board_key = $1 AND definition = $4::jsonb
	RETURNING project_id
)
SELECT count(*), count(DISTINCT project_id) FROM repaired`,
			key, freshJSON, board.Description, staleJSON).Scan(&repaired, &touched)
		if err != nil {
			return totalRows, totalProjects, fmt.Errorf("repair %s board: %w", key, err)
		}
		totalRows += repaired
		totalProjects += touched
	}
	return totalRows, totalProjects, nil
}

// EnsureDefaultBoards writes the three analysis boards onto a project when
// their keys are absent. An existing board with the same key is left alone —
// a later declaration by the owner or an agent must not be overwritten by
// boot. The insert is set-level (one statement per key) so a busy test
// database cannot turn three declarations into a per-project timeout.
func (s *Store) EnsureDefaultBoards(ctx context.Context, projectID string) error {
	return seedDefaultBoards(ctx, s.pg, projectID)
}

// EnsureDefaultBoardsForAll seeds the analysis boards onto every project that
// does not already have them. Called at boot after the board columns exist, so
// projects created before this slice gain the destinations without a second
// signup.
func (s *Store) EnsureDefaultBoardsForAll(ctx context.Context) error {
	return seedDefaultBoards(ctx, s.pg, "")
}

func seedDefaultBoards(ctx context.Context, q pgQuerier, projectID string) error {
	for _, board := range DefaultAnalysisBoards() {
		normalized, err := normalizeBoardDefinition(board.Definition)
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
		payload, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
		if projectID != "" {
			_, err = q.Exec(ctx, `
INSERT INTO dashboards (project_id, name, description, board_key, definition, definition_updated_at)
SELECT p.id, $1::text, $2::text, $3::varchar, $4::jsonb, now()
FROM projects p
WHERE p.id = $5::uuid
  AND NOT EXISTS (
    SELECT 1 FROM dashboards d WHERE d.project_id = p.id AND d.board_key = $3::varchar
  )
ON CONFLICT (project_id, board_key) WHERE board_key <> '' DO NOTHING`, board.Name, board.Description, board.Key, payload, projectID)
		} else {
			_, err = q.Exec(ctx, `
INSERT INTO dashboards (project_id, name, description, board_key, definition, definition_updated_at)
SELECT p.id, $1::text, $2::text, $3::varchar, $4::jsonb, now()
FROM projects p
WHERE NOT EXISTS (
  SELECT 1 FROM dashboards d WHERE d.project_id = p.id AND d.board_key = $3::varchar
)
ON CONFLICT (project_id, board_key) WHERE board_key <> '' DO NOTHING`, board.Name, board.Description, board.Key, payload)
		}
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
	}
	return nil
}

// DefaultBoardKeys is the set the analysis menu addresses, in menu order.
func DefaultBoardKeys() []string {
	boards := DefaultAnalysisBoards()
	keys := make([]string, len(boards))
	for i, board := range boards {
		keys[i] = board.Key
	}
	return keys
}
