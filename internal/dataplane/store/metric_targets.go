package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// metric_targets.go — the project-scoped, append-only history of declared
// metric targets.
//
// A target is what a metric is *supposed* to reach: a direction, a value, and
// the complete-day window it is judged over ("activation ≥ 40% weekly"). It is
// declared per project — never on the system-owned catalog row — so two
// projects reading the same metric can hold different ambitions, and the
// Overview headline has exactly one source of truth for "is this good".
//
// History is append-only: changing or clearing a target appends a new
// version, so a verdict can cite the version that was in force for the window
// it judges instead of silently re-scoring the past. The version in force for
// a read is the highest version whose effective_at is at or before the
// window's end — a cleared row in force means no target, and a version
// declared for a future effective_at does not judge windows that end earlier.

// Target directions. "gte" reads "at least", "lte" "at most" — the two
// comparisons a scalar target can express.
const (
	TargetDirectionGTE = "gte"
	TargetDirectionLTE = "lte"
)

// Verdicts a measured, compatible, complete window earns. An empty verdict is
// not "unknown": it means the target could not judge this reading, and
// VerdictReason says why.
const (
	TargetVerdictOnTrack  = "on_track"
	TargetVerdictAtRisk   = "at_risk"
	TargetVerdictOffTrack = "off_track"
)

// Why a served target produced no verdict. Served beside the target so a
// consumer (the findings detector, a tile) can tell "not judged" from "no
// target" without re-deriving the rule.
const (
	TargetReasonMetricUnavailable = "metric_unavailable" // state is not ok
	TargetReasonPartialWindow     = "partial_window"     // "today" is still accumulating
	TargetReasonPeriodMismatch    = "period_mismatch"    // target period ≠ read window
	TargetReasonCurrencyMismatch  = "currency_mismatch"  // revenue target vs headline currency
)

// targetAtRiskBand is the share of the target value inside which a failing
// measurement reads "at risk" rather than "off track". It is a fixed, declared
// margin — not a trend inference — so a verdict never depends on a previous
// window the read may not have.
const targetAtRiskBand = 0.10

// ErrMetricTargetInvalid wraps every rejected target declaration, so an
// adapter answers 400 with the specific reason instead of a generic failure.
var ErrMetricTargetInvalid = errors.New("metric target is invalid")

// MetricTarget is one version in a metric's target history. Cleared rows are
// tombstones: they carry no spec, and being in force means "no target".
type MetricTarget struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Metric      string    `json:"metric"`
	Version     int64     `json:"version"`
	Cleared     bool      `json:"cleared"`
	Direction   string    `json:"direction,omitempty"`
	Value       float64   `json:"value,omitempty"`
	PeriodDays  int       `json:"period_days,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	EffectiveAt time.Time `json:"effective_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// MetricTargetSpec is the declared shape of a target — the fields a board
// tile's `target` object and the set_metric_target operation share. A nil spec
// on a tile declares nothing; a cleared write declares "no target from here".
type MetricTargetSpec struct {
	Direction string  `json:"direction"`
	Value     float64 `json:"value"`
	// Period is the complete-day window the target is judged over, in the
	// range contract's own vocabulary: "Nd" (1-90). "today" and "" are not
	// target periods — a verdict needs a complete window.
	Period string `json:"period"`
	// Currency is required when the metric's unit is "currency" and refused
	// otherwise: a revenue target names the currency it judges because the
	// metric never sums currencies.
	Currency string `json:"currency,omitempty"`
	// EffectiveAt is when this version takes force; empty means "at write
	// time". A future date schedules the target without re-scoring windows
	// that end before it.
	EffectiveAt string `json:"effective_at,omitempty"`
}

// MetricTargetWrite is one set_metric_target call: the metric, the spec (or
// Clear), and the idempotency plumbing the operation layer passes through.
type MetricTargetWrite struct {
	Metric  string
	Spec    MetricTargetSpec
	Clear   bool
}

// MetricTargetView is the served form: the version in force for the read
// window, its rendered label, and — only when the reading is a measured,
// compatible, complete window — the verdict.
type MetricTargetView struct {
	Version     int64     `json:"version"`
	Direction   string    `json:"direction"`
	Value       float64   `json:"value"`
	PeriodDays  int       `json:"period_days"`
	Currency    string    `json:"currency,omitempty"`
	EffectiveAt time.Time `json:"effective_at"`
	// Label is the rendered target ("≥ 40% weekly") so every surface prints
	// the same words instead of re-deriving them.
	Label string `json:"label"`
	// Verdict is empty unless the target could judge this reading; then it is
	// on_track | at_risk | off_track. VerdictReason names why it is empty.
	Verdict       string `json:"verdict,omitempty"`
	VerdictReason string `json:"verdict_reason,omitempty"`
}

// migrateMetricTargets creates the append-only history table. New table, no
// rewrite of anything existing.
func (s *Store) migrateMetricTargets(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS metric_targets (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	metric VARCHAR(64) NOT NULL,
	version BIGINT NOT NULL,
	cleared BOOLEAN NOT NULL DEFAULT FALSE,
	direction VARCHAR(8) NOT NULL DEFAULT '',
	value DOUBLE PRECISION NOT NULL DEFAULT 0,
	period_days INT NOT NULL DEFAULT 0,
	currency VARCHAR(8) NOT NULL DEFAULT '',
	effective_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (project_id, metric, version)
)`,
		// The in-force read: latest version per metric effective at or before
		// a window end.
		`CREATE INDEX IF NOT EXISTS idx_metric_targets_lookup ON metric_targets (project_id, metric, effective_at, version)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

const metricTargetColumns = `id::text, project_id::text, metric, version, cleared, direction, value, period_days, currency, effective_at, created_at`

func metricTargetScanDest(t *MetricTarget) []any {
	return []any{&t.ID, &t.ProjectID, &t.Metric, &t.Version, &t.Cleared, &t.Direction, &t.Value, &t.PeriodDays, &t.Currency, &t.EffectiveAt, &t.CreatedAt}
}

// normalizeMetricTargetSpec validates a declared spec against the catalog
// entry it targets. The catalog is the authority on the metric's unit, so a
// percent target is bounded 0-100 and a currency target must name its
// currency — the two units whose values are meaningless without that context.
func normalizeMetricTargetSpec(def MetricDefinition, spec MetricTargetSpec) (MetricTargetSpec, error) {
	spec.Direction = strings.ToLower(strings.TrimSpace(spec.Direction))
	spec.Period = strings.TrimSpace(spec.Period)
	spec.Currency = strings.ToUpper(strings.TrimSpace(spec.Currency))
	spec.EffectiveAt = strings.TrimSpace(spec.EffectiveAt)

	if def.Kind != MetricKindValue {
		return spec, fmt.Errorf("%w: metric %q is a %s metric — only a single-number metric can carry a target", ErrMetricTargetInvalid, def.Key, def.Kind)
	}
	switch spec.Direction {
	case TargetDirectionGTE, TargetDirectionLTE:
	default:
		return spec, fmt.Errorf("%w: direction %q is not a target direction; use %q (at least) or %q (at most)", ErrMetricTargetInvalid, spec.Direction, TargetDirectionGTE, TargetDirectionLTE)
	}
	if math.IsNaN(spec.Value) || math.IsInf(spec.Value, 0) || spec.Value < 0 {
		return spec, fmt.Errorf("%w: value %v is not a finite, non-negative target", ErrMetricTargetInvalid, spec.Value)
	}
	if def.Unit == "percent" && spec.Value > 100 {
		return spec, fmt.Errorf("%w: metric %q is a percent — a target of %v exceeds 100", ErrMetricTargetInvalid, def.Key, spec.Value)
	}
	if spec.Period == "" || spec.Period == "today" {
		return spec, fmt.Errorf("%w: period %q is not a target period — declare a complete-day window \"Nd\" (1-%d); \"today\" is partial and cannot be judged", ErrMetricTargetInvalid, spec.Period, overviewMaxDays)
	}
	if err := validPeriod(spec.Period); err != nil {
		return spec, fmt.Errorf("%w: %v", ErrMetricTargetInvalid, err)
	}
	if def.Unit == "currency" {
		if spec.Currency == "" {
			return spec, fmt.Errorf("%w: metric %q is per-currency — the target must name the currency it judges", ErrMetricTargetInvalid, def.Key)
		}
	} else if spec.Currency != "" {
		return spec, fmt.Errorf("%w: metric %q is measured in %s, not currency — drop the currency field", ErrMetricTargetInvalid, def.Key, def.Unit)
	}
	if spec.EffectiveAt != "" {
		if _, err := time.Parse(time.RFC3339, spec.EffectiveAt); err != nil {
			return spec, fmt.Errorf("%w: effective_at %q is not an RFC3339 instant", ErrMetricTargetInvalid, spec.EffectiveAt)
		}
	}
	return spec, nil
}

// sameMetricTarget reports whether a write restates the latest version —
// the dedup that keeps a repeated identical board save from appending a
// duplicate version. EffectiveAt is excluded when the write leaves it unset:
// "the same target from now" restates "the same target from whenever it
// started", not a new schedule.
func sameMetricTarget(latest *MetricTarget, in MetricTargetWrite) bool {
	if latest == nil {
		return false
	}
	if in.Clear {
		return latest.Cleared
	}
	if latest.Cleared {
		return false
	}
	if latest.Direction != in.Spec.Direction ||
		latest.Value != in.Spec.Value ||
		latest.PeriodDays != mustPeriodDays(in.Spec.Period) ||
		latest.Currency != in.Spec.Currency {
		return false
	}
	if in.Spec.EffectiveAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, in.Spec.EffectiveAt)
	return err == nil && latest.EffectiveAt.Equal(at)
}

func mustPeriodDays(period string) int {
	m := overviewPeriodRe.FindStringSubmatch(period)
	if m == nil {
		return 0
	}
	days, _ := strconv.Atoi(m[1])
	return days
}

// setMetricTarget appends one version inside an existing transaction. The
// advisory lock serializes version allocation per (project, metric) so two
// concurrent declarers cannot both claim the same next version.
func setMetricTarget(ctx context.Context, tx pgx.Tx, projectID string, in MetricTargetWrite) (MetricTarget, error) {
	metric := strings.TrimSpace(in.Metric)
	def, ok := MetricCatalogEntry(metric)
	if !ok {
		return MetricTarget{}, fmt.Errorf("%w: metric %q is not in the catalog (known metrics: %s)", ErrMetricTargetInvalid, metric, strings.Join(MetricKeys(), ", "))
	}
	spec := in.Spec
	if !in.Clear {
		var err error
		spec, err = normalizeMetricTargetSpec(def, in.Spec)
		if err != nil {
			return MetricTarget{}, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, projectID, metric); err != nil {
		return MetricTarget{}, err
	}
	var latest *MetricTarget
	var row MetricTarget
	err := tx.QueryRow(ctx, `
SELECT `+metricTargetColumns+` FROM metric_targets
WHERE project_id = $1 AND metric = $2
ORDER BY version DESC LIMIT 1`, projectID, metric).Scan(metricTargetScanDest(&row)...)
	switch {
	case err == nil:
		latest = &row
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return MetricTarget{}, err
	}
	if sameMetricTarget(latest, in) {
		return *latest, nil
	}
	version := int64(1)
	if latest != nil {
		version = latest.Version + 1
	}
	effectiveAt := time.Now().UTC()
	if spec.EffectiveAt != "" {
		effectiveAt, _ = time.Parse(time.RFC3339, spec.EffectiveAt)
	}
	var stored MetricTarget
	err = tx.QueryRow(ctx, `
INSERT INTO metric_targets (project_id, metric, version, cleared, direction, value, period_days, currency, effective_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING `+metricTargetColumns,
		projectID, metric, version, in.Clear, spec.Direction, spec.Value, mustPeriodDays(spec.Period), spec.Currency, effectiveAt).
		Scan(metricTargetScanDest(&stored)...)
	if err != nil {
		return MetricTarget{}, err
	}
	return stored, nil
}

// latestMetricTargets reads the newest version per metric — the declaration
// state a board read serves, as opposed to the in-force version a read window
// resolves. Tombstones are included: "cleared" is a fact a declarer needs.
func latestMetricTargets(ctx context.Context, q boardQuerier, projectID string, metrics []string) (map[string]MetricTarget, error) {
	out := map[string]MetricTarget{}
	if len(metrics) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT ON (metric) `+metricTargetColumns+`
FROM metric_targets
WHERE project_id = $1 AND metric = ANY($2)
ORDER BY metric, version DESC`, projectID, metrics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t MetricTarget
		if err := rows.Scan(metricTargetScanDest(&t)...); err != nil {
			return nil, err
		}
		out[t.Metric] = t
	}
	return out, rows.Err()
}

// SetMetricTargetIdempotent appends a target version under an idempotency
// claim: a retry returns the first receipt, and a repeated identical
// declaration returns the latest version without appending a duplicate.
func (s *Store) SetMetricTargetIdempotent(ctx context.Context, projectID string, in MetricTargetWrite, idemKey, requestHash string) (MetricTarget, error) {
	raw, err := s.runIdempotent(ctx, projectID, "set_metric_target", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			var target MetricTarget
			err := withTx(ctx, q, func(tx pgx.Tx) error {
				t, err := setMetricTarget(ctx, tx, projectID, in)
				if err != nil {
					return err
				}
				target = t
				return nil
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(target)
		})
	if err != nil {
		return MetricTarget{}, err
	}
	var target MetricTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return MetricTarget{}, fmt.Errorf("stored target receipt is unreadable: %w", err)
	}
	return target, nil
}

// metricTargetsInForce reads the version in force per metric at instant `at`:
// the highest version whose effective_at is not after it. A cleared row in
// force means the metric has no target — it is still returned so a caller can
// distinguish "cleared" from "never declared" when it needs to.
func metricTargetsInForce(ctx context.Context, q boardQuerier, projectID string, at time.Time) (map[string]MetricTarget, error) {
	out := map[string]MetricTarget{}
	rows, err := q.Query(ctx, `
SELECT DISTINCT ON (metric) `+metricTargetColumns+`
FROM metric_targets
WHERE project_id = $1 AND effective_at <= $2
ORDER BY metric, version DESC`, projectID, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t MetricTarget
		if err := rows.Scan(metricTargetScanDest(&t)...); err != nil {
			return nil, err
		}
		out[t.Metric] = t
	}
	return out, rows.Err()
}

// metricTargetLabel renders the target the way the surfaces print it:
// "≥ 40% weekly", "≤ 500 daily", "≥ 50,000 VND monthly". The value is on the
// unit's own scale — percent targets are 0-100, currency targets are the
// smallest unit the money metrics serve.
func metricTargetLabel(t MetricTarget, unit string) string {
	dir := "≥"
	if t.Direction == TargetDirectionLTE {
		dir = "≤"
	}
	value := formatTargetValue(t.Value)
	switch unit {
	case "percent":
		value += "%"
	case "currency":
		value += " " + t.Currency
	}
	period := fmt.Sprintf("per %dd", t.PeriodDays)
	switch t.PeriodDays {
	case 1:
		period = "daily"
	case 7:
		period = "weekly"
	case 30:
		period = "monthly"
	}
	return dir + " " + value + " " + period
}

func formatTargetValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		// Thousands separators keep a large count or money target legible.
		s := fmt.Sprintf("%d", int64(v))
		for i := len(s) - 3; i > 0; i -= 3 {
			s = s[:i] + "," + s[i:]
		}
		return s
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// judgeMetricTarget computes the verdict for one measured value, or the reason
// none is possible. measured is on the unit's own scale (percent metrics pass
// their 0-100 rate; revenue passes the signed net in the headline currency).
func judgeMetricTarget(t MetricTarget, unit string, r OverviewRange, measured float64, currency string) MetricTargetView {
	view := MetricTargetView{
		Version:     t.Version,
		Direction:   t.Direction,
		Value:       t.Value,
		PeriodDays:  t.PeriodDays,
		Currency:    t.Currency,
		EffectiveAt: t.EffectiveAt,
		Label:       metricTargetLabel(t, unit),
	}
	switch {
	case !r.CompleteDays:
		view.VerdictReason = TargetReasonPartialWindow
	case t.PeriodDays != r.Days:
		view.VerdictReason = TargetReasonPeriodMismatch
	case unit == "currency" && t.Currency != currency:
		view.VerdictReason = TargetReasonCurrencyMismatch
	default:
		met := measured >= t.Value
		if t.Direction == TargetDirectionLTE {
			met = measured <= t.Value
		}
		if met {
			view.Verdict = TargetVerdictOnTrack
			break
		}
		// Within the band of the target on the failing side, the metric is
		// close enough to read "at risk" rather than "off track".
		margin := t.Value * targetAtRiskBand
		near := measured >= t.Value-margin
		if t.Direction == TargetDirectionLTE {
			near = measured <= t.Value+margin
		}
		if near {
			view.Verdict = TargetVerdictAtRisk
		} else {
			view.Verdict = TargetVerdictOffTrack
		}
	}
	return view
}
