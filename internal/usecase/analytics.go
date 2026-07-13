package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/storage"
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
	opcore.Register(r, remember())
	opcore.Register(r, sendNotification())
	return r
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
		Name:    "explore_events",
		Summary: "Explore event-name breakdown and property coverage to find data-quality gaps.",
		Scope:   "data_quality",
		Handler: func(ctx context.Context, cc opcore.CallContext, in windowInput) (storage.EventExplorer, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.EventExplorer{}, err
			}
			return d.Repo.ExploreEvents(ctx, cc.ProjectID, recentFilter(in.Hours))
		},
	}
}

type runSQLInput struct {
	SQL string `json:"sql" desc:"a single read-only SELECT statement, ClickHouse dialect" required:"true"`
}

type runSQLOutput struct {
	Rows []map[string]any `json:"rows"`
}

func runSQL() opcore.Operation[runSQLInput, runSQLOutput] {
	return opcore.Operation[runSQLInput, runSQLOutput]{
		Name: "run_sql",
		Summary: "Run a read-only (SELECT-only) SQL query against the project's event store. " +
			"The store is ClickHouse: extract JSON properties with JSONExtractString(properties, 'key'), " +
			"not JSON_EXTRACT_STRING; the table is `events`. To count or retain unique users, use the " +
			"`canonical_id` column (identity-stitched: a visitor's anonymous events are folded onto the " +
			"user they later logged in as) — uniqExact(distinct_id) double-counts anyone who logged in. " +
			"Use raw `distinct_id` only for exact-match filters on a specific id. " +
			"For any user/acquisition/retention metric, exclude crawlers with " +
			"WHERE ifNull(visitor_class, 'human') = 'human' — search-bot and ai-platform rows are not people.",
		Scope: "data_quality",
		Handler: func(ctx context.Context, cc opcore.CallContext, in runSQLInput) (runSQLOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return runSQLOutput{}, err
			}
			rows, err := d.Repo.RunSQL(ctx, cc.ProjectID, normalizeSQL(in.SQL)) // read-only enforced in storage
			if err != nil {
				return runSQLOutput{}, err
			}
			return runSQLOutput{Rows: rows}, nil
		},
	}
}

// normalizeSQL translates the JSON helpers the model most often reaches for
// (MySQL/Postgres flavored) into the ClickHouse equivalent, so a query that is
// otherwise correct doesn't fail on dialect alone. The system prompt documents
// the right names; this is the safety net behind it.
func normalizeSQL(s string) string {
	repl := strings.NewReplacer(
		"JSON_EXTRACT_STRING(", "JSONExtractString(",
		"json_extract_string(", "JSONExtractString(",
	)
	return repl.Replace(s)
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

func listDashboards() opcore.Operation[noInput, listDashboardsOutput] {
	return opcore.Operation[noInput, listDashboardsOutput]{
		Name:    "list_dashboards",
		Summary: "List the project's existing dashboards (id + name) to pin charts to.",
		Scope:   "analyze_build",
		Handler: func(ctx context.Context, cc opcore.CallContext, _ noInput) (listDashboardsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listDashboardsOutput{}, err
			}
			boards, err := d.Repo.ListDashboards(ctx, cc.ProjectID)
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
		Name:    "create_dashboard",
		Summary: "Create a new dashboard to group charts. Returns the new dashboard id.",
		Scope:   "analyze_build",
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
	SQL         string `json:"sql" desc:"optional SELECT for a custom chart"`
	XField      string `json:"x_field"`
	YField      string `json:"y_field"`
}

func createChart() opcore.Operation[createChartInput, storage.Chart] {
	return opcore.Operation[createChartInput, storage.Chart]{
		Name:    "create_chart",
		Summary: "Create a chart on a dashboard. Provide either metric/event_name for a built-in chart or a SELECT sql for a custom one.",
		Scope:   "analyze_build",
		Handler: func(ctx context.Context, cc opcore.CallContext, in createChartInput) (storage.Chart, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Chart{}, err
			}
			return d.Repo.CreateChart(ctx, storage.Chart{
				DashboardID: in.DashboardID, ProjectID: cc.ProjectID, Name: in.Name, Kind: in.Kind,
				Metric: in.Metric, EventName: in.EventName, SQL: normalizeSQL(in.SQL), XField: in.XField, YField: in.YField,
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
		Name:    "send_notification",
		Summary: "Send a message to a configured alert channel (Slack/webhook/email) by name. Use to escalate something a human should see.",
		Scope:   "growth_suggest",
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
	Evidence    map[string]any `json:"evidence" desc:"references to charts/queries/numbers"`
	ImpactScore float64        `json:"impact_score" desc:"0-100 estimated impact"`
}

type submitRecOutput struct {
	RecommendationID string `json:"recommendation_id"`
	Status           string `json:"status"`
}

func submitRecommendation() opcore.Operation[submitRecInput, submitRecOutput] {
	return opcore.Operation[submitRecInput, submitRecOutput]{
		Name:     "submit_recommendation",
		Summary:  "Submit a final marketing/sales/growth recommendation with supporting evidence. Ends a scheduled/manual run.",
		Scope:    "growth_suggest",
		Terminal: true,
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
			id, err := d.Repo.CreateRecommendation(ctx, storage.AgentRecommendation{
				ProjectID: cc.ProjectID, RunID: cc.RunID, Category: in.Category, Title: in.Title,
				Rationale: in.Rationale, EvidenceJSON: ev, ImpactScore: in.ImpactScore,
			})
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
		Name:    "remember",
		Summary: "Persist a durable fact/learning/outcome to long-term memory for future runs.",
		Scope:   "growth_suggest",
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
			if err := d.Memory.Remember(ctx, agentcore.MemoryEntry{
				ScopeID: cc.ProjectID, Kind: kind, Content: in.Content, Tags: in.Tags,
				Confidence: 0.7, SourceRun: cc.RunID,
			}); err != nil {
				return rememberOutput{}, err
			}
			return rememberOutput{Remembered: true}, nil
		},
	}
}
