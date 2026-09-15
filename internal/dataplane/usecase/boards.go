package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// boards.go — the declarative board content model's operations.
//
// Four operations, one definition each, projected by opcore onto the in-process
// agent tool, REST (/api/op/<name>), the CLI, and MCP:
//
//	list_metrics — the metric catalog: what a tile may reference and what each
//	               metric means, in what unit, with which prerequisite.
//	read_metric  — one catalog metric computed over a range, with its evidence.
//	get_board    — a board's declared content, resolved: catalog entries and
//	               chart rows for its tiles, plus a warning per dangling
//	               reference.
//	save_board   — declare a board's content in one write: sections, and the
//	               tiles inside them.
//
// The declaration is the whole point: an agent that has been asked to "add a
// graph" reads the catalog, reads the board, and declares the document it wants
// in one revision-fenced write — instead of a sequence of creates that leave a
// half-built board visible in between. A tile declares a catalog metric (the
// server computes it, so the graph cannot describe a number nobody implemented)
// or places a saved chart (an artifact that already exists on the board).

type listMetricsInput struct{}

type listMetricsOutput struct {
	MetricVersion string                     `json:"metric_version"`
	Metrics       []storage.MetricDefinition `json:"metrics"`
}

// listMetrics serves the catalog. Access is analytics:read — the catalog is
// metadata about metrics, not project data — and it is the vocabulary every
// board declaration is validated against, so an authoring agent reads it first.
func listMetrics() opcore.Operation[listMetricsInput, listMetricsOutput] {
	return opcore.Operation[listMetricsInput, listMetricsOutput]{
		Name:    "list_metrics",
		Summary: "List the metric catalog: the metrics a board tile may reference, with each one's key, label, unit, kind, definition, required instrumentation, and the displays it can be drawn as.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, _ listMetricsInput) (listMetricsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listMetricsOutput{}, err
			}
			metrics, err := d.Repo.ListMetricDefinitions(ctx)
			if err != nil {
				return listMetricsOutput{}, err
			}
			return listMetricsOutput{MetricVersion: storage.OverviewMetricVersion, Metrics: metrics}, nil
		},
	}
}

type readMetricInput struct {
	Metric   string `json:"metric" required:"true" desc:"catalog metric key, e.g. active_users — see list_metrics for the whole set"`
	Period   string `json:"period" desc:"project-local complete-day window: \"7d\" (default), \"Nd\" 1-90, or \"today\" (partial)"`
	Platform string `json:"platform" desc:"web | ios | android | server | unknown; empty = all platforms"`
}

// readMetric computes one catalog metric. It runs the same deterministic
// overview read the overview surface runs and projects one metric out of it —
// one implementation, so a board tile and the overview tile can never disagree
// about the same number.
func readMetric() opcore.Operation[readMetricInput, storage.MetricReading] {
	return opcore.Operation[readMetricInput, storage.MetricReading]{
		Name:    "read_metric",
		Summary: "Read one catalog metric (value, series, ranking or rate) over a range, with its definition, state, and the range/timezone/freshness evidence behind it.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in readMetricInput) (storage.MetricReading, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.MetricReading{}, err
			}
			key := strings.TrimSpace(in.Metric)
			def, err := d.Repo.MetricDefinitionByKey(ctx, key)
			if err != nil {
				return storage.MetricReading{}, err
			}
			res, err := d.Repo.Overview(ctx, cc.ProjectID, in.Period, in.Platform, time.Now())
			if err != nil {
				return storage.MetricReading{}, err
			}
			return storage.MetricReadingFor(def, res)
		},
	}
}

type getBoardInput struct {
	BoardID  string `json:"board_id" desc:"board id from list_dashboards"`
	BoardKey string `json:"board_key" desc:"stable board key, for a board a declaration created"`
}

// getBoard serves a board's declared content. It answers by id or by key —
// whichever the caller has — because a preset declares by key and a person
// clicks a board with an id.
func getBoard() opcore.Operation[getBoardInput, storage.BoardContent] {
	return opcore.Operation[getBoardInput, storage.BoardContent]{
		Name:    "get_board",
		Summary: "Read a board's declared content by id or key: its sections and tiles, the catalog entries and charts those tiles reference, and the current revision needed to declare it again.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in getBoardInput) (storage.BoardContent, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.BoardContent{}, err
			}
			boardID := strings.TrimSpace(in.BoardID)
			boardKey := strings.TrimSpace(in.BoardKey)
			switch {
			case boardID != "":
				return d.Repo.BoardContentForProject(ctx, cc.ProjectID, boardID)
			case boardKey != "":
				return d.Repo.BoardContentByKey(ctx, cc.ProjectID, boardKey)
			default:
				return storage.BoardContent{}, fmt.Errorf("board_id or board_key is required — list_dashboards returns the ids")
			}
		},
	}
}

type saveBoardInput struct {
	BoardID     string  `json:"board_id" desc:"board to declare; omit when declaring a board by board_key"`
	BoardKey    string  `json:"board_key" desc:"stable key for this board; creates the board when the key is new, and is how a preset or an agent addresses it on the next run"`
	Name        *string `json:"name" desc:"board name — required when the declaration creates the board; omit to keep the current name"`
	Description *string `json:"description" desc:"board description; omit to keep, \"\" to clear"`
	// The document is described field by field so a model composing it has the
	// whole vocabulary in the schema: sections hold tiles; a tile is either a
	// catalog metric or a saved chart of this board.
	Definition     storage.BoardDefinition `json:"definition" required:"true" desc:"the board content: {version, sections:[{key, title, description, tiles:[{key, title, kind, display, span, metric, chart_id, params}]}]}. A metric tile sets kind=metric, metric=<catalog key from list_metrics>, display=<stat|line|bar|area|table>, span=1-3, and optional params {period, platform}. A chart tile sets kind=chart and chart_id=<a chart already on this board>. Tile and section keys are unique, lower-case, 1-64 characters."`
	Revision       int64                   `json:"revision" desc:"the board's current revision from get_board; omit when creating, required to update — a stale revision conflicts instead of overwriting"`
	IdempotencyKey string                  `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

// saveBoard declares a board's content in one write. The document replaces the
// board's content, which is what makes the declaration convergent: the same
// document declared twice leaves the same board.
func saveBoard() opcore.Operation[saveBoardInput, storage.BoardContent] {
	return opcore.Operation[saveBoardInput, storage.BoardContent]{
		Name:           "save_board",
		Summary:        "Declare a board's content: sections and the tiles inside them. Replaces the board's declared content in one revision-fenced write and returns the resolved board.",
		Scope:          "analyze_build",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in saveBoardInput) (storage.BoardContent, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.BoardContent{}, err
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.BoardContent{}, err
			}
			write := storage.BoardDefinitionWrite{
				BoardID:          strings.TrimSpace(in.BoardID),
				BoardKey:         strings.TrimSpace(in.BoardKey),
				Name:             in.Name,
				Description:      in.Description,
				Definition:       in.Definition,
				ExpectedRevision: in.Revision,
			}
			return d.Repo.SaveBoardDefinition(ctx, cc.ProjectID, write, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

type setMetricTargetInput struct {
	Metric string `json:"metric" required:"true" desc:"catalog metric key — see list_metrics; only single-number (value) metrics carry a target"`
	// The spec mirrors a board tile's target object: direction, value, the
	// complete-day window it is judged over, and — for the per-currency
	// revenue metric — the currency it names.
	Direction string  `json:"direction" desc:"gte (at least) | lte (at most)"`
	Value     float64 `json:"value" desc:"target value on the metric's own scale — percent metrics take 0-100, revenue takes the smallest unit of the named currency"`
	Period    string  `json:"period" desc:"complete-day window the target is judged over: \"Nd\" (1-90), e.g. \"7d\" for weekly"`
	Currency  string  `json:"currency" desc:"required when the metric is revenue; refused otherwise"`
	// EffectiveAt schedules the version; empty takes force at write time. A
	// window cites the highest version effective at or before its end.
	EffectiveAt    string `json:"effective_at" desc:"RFC3339 instant this version takes force; empty = now"`
	Clear          bool   `json:"clear" desc:"append a cleared version — the metric has no target from here"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

// setMetricTarget appends one version to a metric's project-scoped target
// history. It is the only way to clear a target — a board tile that omits
// `target` declares nothing — and the direct path when no board save is
// otherwise happening.
func setMetricTarget() opcore.Operation[setMetricTargetInput, storage.MetricTarget] {
	return opcore.Operation[setMetricTargetInput, storage.MetricTarget]{
		Name:           "set_metric_target",
		Summary:        "Declare or clear a metric's target: appends a version to the project-scoped target history (value + direction + period, e.g. activation ≥ 40% weekly). The verdict a read serves cites the version in force for its window.",
		Scope:          "analyze_build",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in setMetricTargetInput) (storage.MetricTarget, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.MetricTarget{}, err
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.MetricTarget{}, err
			}
			write := storage.MetricTargetWrite{
				Metric: strings.TrimSpace(in.Metric),
				Clear:  in.Clear,
				Spec: storage.MetricTargetSpec{
					Direction:   in.Direction,
					Value:       in.Value,
					Period:      in.Period,
					Currency:    in.Currency,
					EffectiveAt: in.EffectiveAt,
				},
			}
			return d.Repo.SetMetricTargetIdempotent(ctx, cc.ProjectID, write, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}
