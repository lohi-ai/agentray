package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
)

// Evaluator runs the saved alert rules on a cadence and fans firing/recovery
// edges out to channels. It is driven off the agent scheduler's minute tick
// (one more producer on the same clock), so alerting needs no second timer.
type Evaluator struct {
	store     *storage.Store
	deliverer *Deliverer
}

// NewEvaluator wires an evaluator over storage + a deliverer.
func NewEvaluator(store *storage.Store, deliverer *Deliverer) *Evaluator {
	return &Evaluator{store: store, deliverer: deliverer}
}

// Tick evaluates every enabled rule whose schedule matches now. Best-effort: a
// per-rule failure is isolated so one bad rule never stalls the rest. The claim
// step makes a rule single-writer across replicas.
func (e *Evaluator) Tick(ctx context.Context, now time.Time) {
	rules, err := e.store.DueAlertRules(ctx)
	if err != nil {
		return
	}
	for _, r := range rules {
		if !CronMatches(r.Schedule, now) {
			continue
		}
		// Re-claim window: allow re-evaluation if the last claim is older than 50s
		// (just under a tick) so a crashed evaluation retries next minute but a
		// healthy one isn't double-run within the same minute.
		claimed, err := e.store.ClaimAlertRuleForEval(ctx, r.ID, now.UTC(), now.Add(-50*time.Second).UTC())
		if err != nil || !claimed {
			continue
		}
		_ = e.evaluateRule(ctx, r)
	}
}

// evaluateRule computes the rule's metric, applies its condition, and delivers
// on the ok→firing edge (and records the firing→ok recovery). Delivery failures
// are recorded in the event payload but do not abort the edge transition — the
// state still advances so a flapping channel doesn't wedge the rule.
func (e *Evaluator) evaluateRule(ctx context.Context, r storage.AlertRule) error {
	series, err := e.metricSeries(ctx, r)
	if err != nil {
		return err
	}
	if len(series) == 0 {
		return nil
	}
	current := series[len(series)-1]
	firing, reason := applyCondition(r.Condition, series)

	newState := "ok"
	if firing {
		newState = "firing"
	}
	if newState == r.LastState {
		return nil // no edge; stay quiet (avoids re-notifying a sustained breach)
	}

	payload := map[string]any{"reason": reason, "value": current}
	if newState == "firing" {
		n := Notification{
			Title: fmt.Sprintf("🔴 Alert firing: %s", r.Name),
			Body:  reason,
			Level: "firing",
		}
		if delivErr := e.fanOut(ctx, r, n); delivErr != nil {
			payload["delivery_error"] = delivErr.Error()
		}
	} else {
		n := Notification{
			Title: fmt.Sprintf("🟢 Recovered: %s", r.Name),
			Body:  fmt.Sprintf("Metric back within threshold (value %.4g).", current),
			Level: "ok",
		}
		if delivErr := e.fanOut(ctx, r, n); delivErr != nil {
			payload["delivery_error"] = delivErr.Error()
		}
	}
	pb, _ := json.Marshal(payload)
	return e.store.RecordAlertEvent(ctx, r.ID, newState, current, pb)
}

// fanOut delivers n to every channel the rule references, aggregating errors so
// one dead channel doesn't hide the others.
func (e *Evaluator) fanOut(ctx context.Context, r storage.AlertRule, n Notification) error {
	if len(r.Channels) == 0 {
		return nil
	}
	channels, err := e.store.AlertChannelsByID(ctx, r.Channels)
	if err != nil {
		return err
	}
	var errs []string
	for _, ch := range channels {
		if derr := e.deliverer.Deliver(ctx, ch, n); derr != nil {
			errs = append(errs, ch.Name+": "+derr.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// applyCondition evaluates the threshold test against a metric series (last
// element = current value). Returns whether it fires and a human reason.
func applyCondition(c storage.AlertCondition, series []float64) (bool, string) {
	current := series[len(series)-1]
	switch c.Op {
	case "gt":
		if current > c.Value {
			return true, fmt.Sprintf("value %.4g exceeded threshold %.4g", current, c.Value)
		}
	case "lt":
		if current < c.Value {
			return true, fmt.Sprintf("value %.4g fell below threshold %.4g", current, c.Value)
		}
	case "z_score":
		return zScoreFires(c, series)
	}
	return false, ""
}

// zScoreFires flags the current value as anomalous if it is more than Value sigma
// from the baseline mean built from the prior buckets. MinEvents suppresses the
// test when the baseline sum is too small to be trusted (low-volume noise).
func zScoreFires(c storage.AlertCondition, series []float64) (bool, string) {
	if len(series) < 3 {
		return false, "" // need a baseline of at least two prior buckets
	}
	current := series[len(series)-1]
	baseline := series[:len(series)-1]
	if c.Window > 0 && len(baseline) > c.Window {
		baseline = baseline[len(baseline)-c.Window:]
	}
	var sum float64
	for _, v := range baseline {
		sum += v
	}
	if c.MinEvents > 0 && sum < float64(c.MinEvents) {
		return false, "" // too little volume to judge; stay quiet
	}
	mean := sum / float64(len(baseline))
	var variance float64
	for _, v := range baseline {
		variance += (v - mean) * (v - mean)
	}
	std := math.Sqrt(variance / float64(len(baseline)))
	if std == 0 {
		return false, "" // flat baseline; a spike would divide by zero, treat as quiet
	}
	sigma := c.Value
	if sigma <= 0 {
		sigma = 3
	}
	z := (current - mean) / std
	if math.Abs(z) >= sigma {
		return true, fmt.Sprintf("value %.4g is %.2fσ from baseline mean %.4g (threshold %.1fσ)", current, z, mean, sigma)
	}
	return false, ""
}

// metricSeries resolves a rule's source to a numeric series (the last element is
// the current value; earlier elements form the anomaly baseline). sql runs the
// project-scoped SQL verbatim; agent_ops maps a named metric to a canned query.
func (e *Evaluator) metricSeries(ctx context.Context, r storage.AlertRule) ([]float64, error) {
	var sqlText string
	switch r.SourceKind {
	case "sql":
		sqlText = r.SourceRef
	case "agent_ops":
		tmpl, ok := opsMetricSQL[strings.TrimSpace(r.SourceRef)]
		if !ok {
			return nil, fmt.Errorf("alerting: unknown agent_ops metric %q", r.SourceRef)
		}
		sqlText = tmpl
	default:
		return nil, fmt.Errorf("alerting: source_kind %q is not evaluable yet", r.SourceKind)
	}
	if strings.TrimSpace(sqlText) == "" {
		return nil, fmt.Errorf("alerting: rule %s has no source query", r.ID)
	}
	rows, err := e.store.RunSQL(ctx, r.ProjectID, sqlText)
	if err != nil {
		return nil, err
	}
	return extractSeries(rows), nil
}

// extractSeries pulls a numeric series from SQL rows: it prefers a column named
// "value", falling back to the first numeric column. Rows are taken in the order
// returned, so a query ordered by time bucket yields the series oldest→newest.
func extractSeries(rows []map[string]any) []float64 {
	if len(rows) == 0 {
		return nil
	}
	col := "value"
	if _, ok := rows[0][col]; !ok {
		col = firstNumericColumn(rows[0])
	}
	if col == "" {
		return nil
	}
	out := make([]float64, 0, len(rows))
	for _, row := range rows {
		if f, ok := toFloat(row[col]); ok {
			out = append(out, f)
		}
	}
	return out
}

func firstNumericColumn(row map[string]any) string {
	for k, v := range row {
		if _, ok := toFloat(v); ok {
			return k
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// opsMetricSQL maps agent_ops metric names to project-scoped SQL producing a
// time-bucketed series (oldest→newest), so both threshold and z_score rules can
// read the headline operational metrics without the user writing SQL. Each reads
// `FROM events` exactly once (RunSQL scopes it to the rule's project).
var opsMetricSQL = map[string]string{
	// Per-hour event volume over the last 24h — for volume drops (lt) or spikes (z_score).
	"event_volume_hourly": `SELECT toStartOfHour(timestamp) AS bucket, count() AS value
FROM events WHERE timestamp > now() - INTERVAL 24 HOUR GROUP BY bucket ORDER BY bucket`,
	// Per-hour error rate (%) over the last 24h.
	"error_rate_hourly": `SELECT toStartOfHour(timestamp) AS bucket,
100 * countIf(is_error = 1) / greatest(count(), 1) AS value
FROM events WHERE timestamp > now() - INTERVAL 24 HOUR GROUP BY bucket ORDER BY bucket`,
	// Minutes since the last event — an ingestion-gap watchdog (gt to alert on silence).
	"minutes_since_last_event": `SELECT dateDiff('minute', max(timestamp), now()) AS value FROM events`,
	// Per-hour p95 latency (ms) over the last 24h.
	"latency_p95_hourly": `SELECT toStartOfHour(timestamp) AS bucket,
quantile(0.95)(latency_ms) AS value
FROM events WHERE timestamp > now() - INTERVAL 24 HOUR AND latency_ms IS NOT NULL GROUP BY bucket ORDER BY bucket`,
}
