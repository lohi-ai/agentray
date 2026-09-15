package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// jsonMarshal renders a value to a compact JSON string (used to store the
// recommendation evidence blob).
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Registry builds the single operation registry shared by every adapter. The
// agent (opcore.Tools), the REST surface (opcore.MountHTTP), and the CLI
// (command metadata) all read from this one definition, so a capability is
// written — and fixed — exactly once.
func Registry() *opcore.Registry {
	r := opcore.NewRegistry()
	opcore.Register(r, activitySummary())
	opcore.Register(r, recentEvents())
	opcore.Register(r, persons())
	opcore.Register(r, exploreEvents())
	opcore.Register(r, runSQL())
	opcore.Register(r, runInsight())
	opcore.Register(r, runFunnel())
	opcore.Register(r, runRetention())
	opcore.Register(r, listDashboards())
	opcore.Register(r, createDashboard())
	opcore.Register(r, createChart())
	opcore.Register(r, submitRecommendation())
	opcore.Register(r, proposeTest())
	opcore.Register(r, testStatus())
	opcore.Register(r, listTests())
	opcore.Register(r, remember())
	opcore.Register(r, sendNotification())
	opcore.Register(r, overview())
	opcore.Register(r, updateTest())
	opcore.Register(r, recordOutcome())
	opcore.Register(r, abandonTest())
	opcore.Register(r, listFindings())
	opcore.Register(r, datasetPreview())
	opcore.Register(r, verifySDK())
	opcore.Register(r, updateDashboard())
	opcore.Register(r, archiveDashboard())
	opcore.Register(r, unarchiveDashboard())
	opcore.Register(r, listCharts())
	opcore.Register(r, updateChart())
	opcore.Register(r, archiveChart())
	opcore.Register(r, unarchiveChart())
	opcore.Register(r, reorderCharts())
	opcore.Register(r, listMetrics())
	opcore.Register(r, readMetric())
	opcore.Register(r, getBoard())
	opcore.Register(r, saveBoard())
	opcore.Register(r, setMetricTarget())
	opcore.Register(r, testSource())
	opcore.Register(r, previewSource())
	opcore.Register(r, listSources())
	opcore.Register(r, createSource())
	opcore.Register(r, updateSource())
	opcore.Register(r, archiveSource())
	opcore.Register(r, unarchiveSource())
	opcore.Register(r, pauseSource())
	opcore.Register(r, runSource())
	opcore.Register(r, sourceStatus())
	opcore.Register(r, cancelSourceRun())
	opcore.Register(r, runFindingsScan())
	opcore.Register(r, watchFunnel())
	opcore.Register(r, listFunnelWatches())
	r.SetLegacyAllowlist(legacyOperationAllowlist)
	r.SetErrorClassifier(classifyOpError)
	r.SetErrorMapper(MapOpError)
	return r
}

// legacyOperationAllowlist is the Option A contract: the exact set of
// operations a pre-split project key could invoke when the credential split
// shipped, plus verify_sdk — the one approved grandfathered addition (it is a
// bounded recent-event read, a subset of what the key already reached through
// recent_events). Every operation registered after this list was frozen is
// denied to legacy keys by construction: the list is a constant, not a
// snapshot of the registry, so adding an operation never expands it.
//
// Existing grants this preserves (all 17 pre-split operations):
//
//	analytics reads — activity_summary, recent_events, persons,
//	  explore_events, run_sql, run_insight, run_funnel, run_retention,
//	  list_dashboards, test_status, list_tests
//	dashboard writes — create_dashboard, create_chart
//	growth writes — submit_recommendation, propose_test, remember,
//	  send_notification
//	grandfathered — verify_sdk
var legacyOperationAllowlist = []string{
	"activity_summary", "recent_events", "persons", "explore_events",
	"run_sql", "run_insight", "run_funnel", "run_retention",
	"list_dashboards", "create_dashboard", "create_chart",
	"submit_recommendation", "propose_test", "test_status", "list_tests",
	"remember", "send_notification",
	"verify_sdk",
}

// --- Read operations (monitor / data_quality) ---

type windowInput struct {
	Hours int `json:"hours" desc:"look-back window in hours (default 24)"`
}

func activitySummary() opcore.Operation[windowInput, storage.ActivitySummary] {
	return opcore.Operation[windowInput, storage.ActivitySummary]{
		Name:    "activity_summary",
		Summary: "Summarize event volume, errors, latency and cost over a recent window.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in windowInput) (storage.ActivitySummary, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.ActivitySummary{}, err
			}
			return d.Repo.ActivitySummary(ctx, cc.ProjectID, recentFilter(in.Hours))
		},
	}
}

type recentEventsInput struct {
	Limit int `json:"limit" desc:"max events 1-200 (default 50)"`
}

type recentEventsOutput struct {
	Events []storage.Event `json:"events"`
}

func recentEvents() opcore.Operation[recentEventsInput, recentEventsOutput] {
	return opcore.Operation[recentEventsInput, recentEventsOutput]{
		Name:    "recent_events",
		Summary: "List the most recent raw events for the project.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in recentEventsInput) (recentEventsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return recentEventsOutput{}, err
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 50
			}
			if limit > 200 {
				limit = 200
			}
			events, err := d.Repo.RecentEvents(ctx, cc.ProjectID, limit)
			if err != nil {
				return recentEventsOutput{}, err
			}
			return recentEventsOutput{Events: events}, nil
		},
	}
}

func persons() opcore.Operation[windowInput, storage.PersonsSummary] {
	return opcore.Operation[windowInput, storage.PersonsSummary]{
		Name:    "persons",
		Summary: "Summarize persons (identified + anonymous) over a recent window.",
		Scope:   "data_quality",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in windowInput) (storage.PersonsSummary, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.PersonsSummary{}, err
			}
			return d.Repo.Persons(ctx, cc.ProjectID, recentFilter(in.Hours))
		},
	}
}

func exploreEvents() opcore.Operation[windowInput, storage.EventExplorer] {
	return opcore.Operation[windowInput, storage.EventExplorer]{
		Name: "explore_events",
		Summary: "Read the project's event catalog and a sample of raw events. " +
			"`schema` is one row per event name over the window — event_name, event_type, events (volume), " +
			"people (identity-stitched, crawlers excluded), first_seen/last_seen, and property_keys — so you do " +
			"NOT need SQL to find out what is tracked, what properties exist, how much data there is, or when it " +
			"starts and stops. Read `schema` first and query only what it cannot answer. " +
			"`events` is a raw sample for seeing an actual payload; it is truncated, so never count from it. " +
			"Both cover the same window (`hours`, default recent) — widen `hours` rather than concluding an event " +
			"is missing or a product is new.",
		Scope:  "data_quality",
		Access: opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in windowInput) (storage.EventExplorer, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.EventExplorer{}, err
			}
			filter := recentFilter(in.Hours)
			// A tool result is truncated to 24KB before it reaches the model, and
			// recentFilter's 200 raw events is several times that — so this payload
			// always arrived cut off mid-JSON, which is why an agent would sample the
			// events again in SQL rather than trust what it had been handed. The
			// catalog carries the shape now, so the sample only has to show what a
			// real payload looks like: keep it small enough that both survive whole.
			filter.Limit = exploreSampleSize
			return d.Repo.ExploreEvents(ctx, cc.ProjectID, filter)
		},
	}
}

// exploreSampleSize is how many raw events ride along with the catalog. Sized to
// fit under the tool-result cap with the catalog intact, not to be a sample
// anything is counted from — explore_events says so, and the numbers come from
// the catalog or from SQL.
const exploreSampleSize = 20

type runSQLInput struct {
	SQL string `json:"sql" desc:"a single read-only SELECT statement, DuckDB dialect" required:"true"`
}

type runSQLOutput struct {
	Rows []map[string]any `json:"rows"`
}

func runSQL() opcore.Operation[runSQLInput, runSQLOutput] {
	return opcore.Operation[runSQLInput, runSQLOutput]{
		Name: "run_sql",
		Summary: "Run a read-only (SELECT-only) SQL query against the project's event store. " +
			"The store is DuckDB: extract JSON properties with json_extract_string(properties, '$.key'), " +
			"not JSON_EXTRACT; the table is `events`. To count or retain unique users, use the " +
			"`canonical_id` column (identity-stitched: a visitor's anonymous events are folded onto the " +
			"user they later logged in as) — count(DISTINCT distinct_id) double-counts anyone who logged in. " +
			"Use raw `distinct_id` only for exact-match filters on a specific id. " +
			"For any user/acquisition/retention metric, exclude crawlers with " +
			"WHERE coalesce(visitor_class, 'human') = 'human' — search-bot and ai-platform rows are not people. " +
			"A project may ship more than one app: the `platform` column says which one an event came from " +
			"('web', 'ios', 'android', 'server'; '' when undetermined). Split by it before comparing " +
			"platforms — do NOT read platform out of properties, and never state a product-wide rate as if " +
			"it described one app when more than one platform is present. " +
			"Synced external data (data connectors) lives in `external_rows`: filter by table_name (the source " +
			"table, e.g. 'public.users' shortened to 'users' when in public), read fields with " +
			"json_extract_string(data, '$.column') (json_extract for numbers); row_key is the source row's " +
			"key and synced_at the landing time. Rows are already deduplicated per (table_name, row_key) and " +
			"each row is CURRENT state, not history — a re-sync replaces the row, it does not append. " +
			"Rows the source marked deleted (the sync's soft-delete column) are already excluded; rows the " +
			"source hard-deleted without a mark are NOT — a count here can overstate the source. " +
			"To combine the two tables, put events on the FROM side and join external_rows onto it " +
			"(FROM events e JOIN external_rows x ON ...): events may appear exactly once and only after FROM, " +
			"external_rows only after FROM or JOIN — comma joins, quoted table names, and JOIN events are rejected.",
		Scope:  "data_quality",
		Access: opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in runSQLInput) (runSQLOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return runSQLOutput{}, err
			}
			rows, err := d.Repo.RunSQL(ctx, cc.ProjectID, in.SQL) // read-only enforced in storage
			if err != nil {
				return runSQLOutput{}, err
			}
			return runSQLOutput{Rows: rows}, nil
		},
	}
}

// --- Authoring operations (analyze_build) ---

type runInsightInput struct {
	Type   string   `json:"type" desc:"timeseries | funnel | retention (default timeseries)"`
	Metric string   `json:"metric" desc:"metric, e.g. events | users | sessions"`
	Steps  []string `json:"steps" desc:"event names for a funnel"`
	Hours  int      `json:"hours" desc:"look-back window in hours (default 24)"`
}

func runInsight() opcore.Operation[runInsightInput, storage.InsightResult] {
	return opcore.Operation[runInsightInput, storage.InsightResult]{
		Name:    "run_insight",
		Summary: "Run an insight (timeseries | funnel | retention) and return its computed series/rows.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in runInsightInput) (storage.InsightResult, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.InsightResult{}, err
			}
			if in.Type == "" {
				in.Type = "timeseries"
			}
			return d.Repo.RunInsight(ctx, cc.ProjectID, in.Type, in.Metric, in.Steps, recentFilter(in.Hours))
		},
	}
}

type runFunnelInput struct {
	Steps []string `json:"steps" desc:"ordered event names, first → last step" required:"true"`
	Hours int      `json:"hours" desc:"look-back window in hours (default 24)"`
}

// runFunnel is a first-class funnel tool. It delegates to the same RunInsight
// engine as run_insight, but advertises the funnel contract (an ordered steps
// list) directly so a model discovers and calls it without having to know the
// run_insight `type` convention.
func runFunnel() opcore.Operation[runFunnelInput, storage.InsightResult] {
	return opcore.Operation[runFunnelInput, storage.InsightResult]{
		Name:    "run_funnel",
		Summary: "Compute step-by-step conversion through an ordered list of events over a recent window.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in runFunnelInput) (storage.InsightResult, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.InsightResult{}, err
			}
			return d.Repo.RunInsight(ctx, cc.ProjectID, "funnel", "users", in.Steps, recentFilter(in.Hours))
		},
	}
}

type runRetentionInput struct {
	Event string `json:"event" desc:"the returning event that defines retention (default user.pageview)"`
	Hours int    `json:"hours" desc:"look-back window in hours (default 24)"`
}

// runRetention is a first-class retention tool over the same engine.
func runRetention() opcore.Operation[runRetentionInput, storage.InsightResult] {
	return opcore.Operation[runRetentionInput, storage.InsightResult]{
		Name:    "run_retention",
		Summary: "Compute cohort retention for a returning event over a recent window.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in runRetentionInput) (storage.InsightResult, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.InsightResult{}, err
			}
			filter := recentFilter(in.Hours)
			filter.EventName = in.Event // RunInsight reads the returning event from here
			return d.Repo.RunInsight(ctx, cc.ProjectID, "retention", "users", nil, filter)
		},
	}
}

type noInput struct{}

type listDashboardsOutput struct {
	Dashboards []storage.Dashboard `json:"dashboards"`
}

type listDashboardsInput struct {
	// IncludeArchived is the explicit archived view the soft-archive contract
	// requires — default lists stay active-only.
	IncludeArchived bool `json:"include_archived" desc:"include archived dashboards (default false)"`
}

func listDashboards() opcore.Operation[listDashboardsInput, listDashboardsOutput] {
	return opcore.Operation[listDashboardsInput, listDashboardsOutput]{
		Name:    "list_dashboards",
		Summary: "List the project's existing dashboards (id + name) to pin charts to. Pass include_archived for the archived view.",
		Scope:   "analyze_build",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in listDashboardsInput) (listDashboardsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listDashboardsOutput{}, err
			}
			boards, err := d.Repo.ListDashboardsFiltered(ctx, cc.ProjectID, in.IncludeArchived)
			if err != nil {
				return listDashboardsOutput{}, err
			}
			return listDashboardsOutput{Dashboards: boards}, nil
		},
	}
}

type createDashboardInput struct {
	Name        string `json:"name" desc:"dashboard name" required:"true"`
	Description string `json:"description" desc:"optional description"`
}

func createDashboard() opcore.Operation[createDashboardInput, storage.Dashboard] {
	return opcore.Operation[createDashboardInput, storage.Dashboard]{
		Name:           "create_dashboard",
		Summary:        "Create a new dashboard to group charts. Returns the new dashboard id.",
		Scope:          "analyze_build",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in createDashboardInput) (storage.Dashboard, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Dashboard{}, err
			}
			return d.Repo.CreateDashboard(ctx, cc.ProjectID, in.Name, in.Description)
		},
	}
}

type createChartInput struct {
	DashboardID string `json:"dashboard_id" desc:"target dashboard id (from list_dashboards/create_dashboard)" required:"true"`
	Name        string `json:"name" desc:"chart name" required:"true"`
	Kind        string `json:"kind" desc:"line | bar | area | number | table (default line)"`
	Metric      string `json:"metric" desc:"built-in metric, e.g. events | users"`
	EventName   string `json:"event_name"`
	EventType   string `json:"event_type"`
	SQL         string `json:"sql" desc:"optional SELECT for a custom chart"`
	XField      string `json:"x_field"`
	YField      string `json:"y_field"`
	ColSpan     int    `json:"col_span" desc:"grid columns 1-3 (default 1)"`
}

func createChart() opcore.Operation[createChartInput, storage.Chart] {
	return opcore.Operation[createChartInput, storage.Chart]{
		Name:           "create_chart",
		Summary:        "Create a chart on a dashboard. Provide either metric/event_name for a built-in chart or a SELECT sql for a custom one.",
		Scope:          "analyze_build",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in createChartInput) (storage.Chart, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Chart{}, err
			}
			return d.Repo.CreateChart(ctx, storage.Chart{
				DashboardID: in.DashboardID, ProjectID: cc.ProjectID, Name: in.Name, Kind: in.Kind,
				Metric: in.Metric, EventName: in.EventName, EventType: in.EventType,
				SQL: in.SQL, XField: in.XField, YField: in.YField, ColSpan: in.ColSpan,
			})
		},
	}
}

// --- Notification operation (send_notification, growth_suggest scope) ---

type sendNotificationInput struct {
	Channel string `json:"channel" desc:"name of a configured alert channel in this workspace" required:"true"`
	Title   string `json:"title" desc:"short notification title" required:"true"`
	Body    string `json:"body" desc:"notification body / details"`
}

type sendNotificationOutput struct {
	Channel string `json:"channel"`
	Status  string `json:"status"`
}

// sendNotification posts a message to a saved alert channel by name. It reuses the
// same channels the alerting worker delivers on, so an agent that spots something
// worth a human's attention can push it to Slack/webhook/email without a bespoke
// integration. Delivery goes through the platform Notifier (SSRF-guarded,
// secret-resolving); a build without a Notifier wired returns a clear error.
func sendNotification() opcore.Operation[sendNotificationInput, sendNotificationOutput] {
	return opcore.Operation[sendNotificationInput, sendNotificationOutput]{
		Name:           "send_notification",
		Summary:        "Send a message to a configured alert channel (Slack/webhook/email) by name. Use to escalate something a human should see.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessGrowthWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in sendNotificationInput) (sendNotificationOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return sendNotificationOutput{}, err
			}
			if d.Notifier == nil {
				return sendNotificationOutput{}, fmt.Errorf("send_notification: notification delivery is not configured on this server")
			}
			wsID, err := d.Repo.WorkspaceIDForProject(ctx, cc.ProjectID)
			if err != nil {
				return sendNotificationOutput{}, fmt.Errorf("send_notification: resolving workspace: %w", err)
			}
			ch, err := d.Repo.WorkspaceChannelByName(ctx, wsID, in.Channel)
			if err != nil {
				return sendNotificationOutput{}, fmt.Errorf("send_notification: no channel named %q in this workspace", in.Channel)
			}
			if err := d.Notifier.Notify(ctx, ch, in.Title, in.Body); err != nil {
				return sendNotificationOutput{}, err
			}
			return sendNotificationOutput{Channel: ch.Name, Status: "sent"}, nil
		},
	}
}

// --- Growth operations (growth_suggest) ---

type submitRecInput struct {
	Category    string         `json:"category" desc:"marketing | sales | growth | product | data"`
	Title       string         `json:"title" desc:"short recommendation title" required:"true"`
	Rationale   string         `json:"rationale" desc:"why, grounded in the data you saw"`
	Evidence    map[string]any `json:"evidence" desc:"typed envelope object: {query_ref, metric_version, dataset_version, range, filters, timezone, watermark, warnings} — cite the actual data window"`
	ImpactScore float64        `json:"impact_score" desc:"0-100 estimated impact"`
	// IdempotencyKey makes a retried submit replay the stored receipt instead
	// of folding into (or duplicating) a finding twice.
	IdempotencyKey string `json:"idempotency_key"`
}

type submitRecOutput struct {
	RecommendationID string `json:"recommendation_id"`
	Status           string `json:"status"`
}

func submitRecommendation() opcore.Operation[submitRecInput, submitRecOutput] {
	return opcore.Operation[submitRecInput, submitRecOutput]{
		Name:           "submit_recommendation",
		Summary:        "Submit a final marketing/sales/growth recommendation with supporting evidence. Ends a scheduled/manual run.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Terminal:       true,
		Handler: func(ctx context.Context, cc opcore.CallContext, in submitRecInput) (submitRecOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return submitRecOutput{}, err
			}
			// title is enforced as required by opcore before the handler runs.
			ev := "{}"
			if len(in.Evidence) > 0 {
				if b, err := jsonMarshal(in.Evidence); err == nil {
					ev = b
				}
			}
			rec := storage.AgentRecommendation{
				ProjectID: cc.ProjectID, RunID: cc.RunID, Category: in.Category, Title: in.Title,
				Rationale: in.Rationale, EvidenceJSON: ev, ImpactScore: in.ImpactScore,
			}
			var id string
			if in.IdempotencyKey != "" {
				hash, herr := requestHash(in)
				if herr != nil {
					return submitRecOutput{}, herr
				}
				id, err = d.Repo.CreateRecommendationIdempotent(ctx, rec, strings.TrimSpace(in.IdempotencyKey), hash)
			} else {
				id, err = d.Repo.CreateRecommendation(ctx, rec)
			}
			if err != nil {
				return submitRecOutput{}, err
			}
			return submitRecOutput{RecommendationID: id, Status: "open"}, nil
		},
	}
}

type rememberInput struct {
	Kind    string   `json:"kind" desc:"fact | learning | outcome (default fact)"`
	Content string   `json:"content" desc:"the durable fact to persist" required:"true"`
	Tags    []string `json:"tags"`
}

type rememberOutput struct {
	Remembered bool `json:"remembered"`
}

func remember() opcore.Operation[rememberInput, rememberOutput] {
	return opcore.Operation[rememberInput, rememberOutput]{
		Name:           "remember",
		Summary:        "Persist a durable fact/learning/outcome to long-term memory for future runs.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessGrowthWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in rememberInput) (rememberOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return rememberOutput{}, err
			}
			if d.Memory == nil {
				return rememberOutput{}, fmt.Errorf("memory is not configured for this project")
			}
			kind := agentcore.MemoryKind(in.Kind)
			if kind != agentcore.MemoryFact && kind != agentcore.MemoryLearning && kind != agentcore.MemoryOutcome {
				kind = agentcore.MemoryFact
			}
			// The agent's own scope, not the project's: recall reads the agent
			// scope, so a write filed under the project is a write the agent can
			// never read back. MemoryScope falls back to the project for callers
			// with no agent, which is exactly what the default agent resolves to.
			if err := d.Memory.Remember(ctx, agentcore.MemoryEntry{
				ScopeID: cc.MemoryScope(), Kind: kind, Content: in.Content, Tags: in.Tags,
				Confidence: 0.7, SourceRun: cc.RunID,
			}); err != nil {
				return rememberOutput{}, err
			}
			return rememberOutput{Remembered: true}, nil
		},
	}
}
