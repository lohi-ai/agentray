package findings

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// finding is one detector firing before it becomes a recommendation row.
// dedupeKey is the stable condition identity createRecommendation folds on.
type finding struct {
	dedupeKey string
	category  string
	title     string
	rationale string
	impact    float64
	evidence  evidenceEnvelope
}

// evidenceEnvelope is the typed provenance contract the findings UI renders
// (web/modules/plans/lib/plans.ts evidenceParts): query_ref, metric_version,
// range, timezone, watermark, warnings. The engine writes the envelope itself
// — submit_recommendation never validates it, and a finding without
// provenance is a claim nobody can check.
type evidenceEnvelope struct {
	QueryRef      map[string]any `json:"query_ref"`
	MetricVersion string         `json:"metric_version,omitempty"`
	Range         string         `json:"range"`
	Timezone      string         `json:"timezone,omitempty"`
	Watermark     string         `json:"watermark,omitempty"`
	Detector      string         `json:"detector"`
	Observed      map[string]any `json:"observed"`
}

func (f finding) evidenceJSON() string {
	b, err := json.Marshal(f.evidence)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// rangeLabel renders the compared windows the way the requirement asks for
// them: both windows, named.
func rangeLabel(cur, prior storage.OverviewRange) string {
	return fmt.Sprintf("%s → %s vs %s → %s",
		cur.From.Format("2006-01-02"), cur.To.Format("2006-01-02"),
		prior.From.Format("2006-01-02"), prior.To.Format("2006-01-02"))
}

// watermark is the freshest receipt timestamp the overview observed — the
// "as of" a reader checks the finding against.
func watermark(res storage.OverviewResult) string {
	if res.DataStatus.LastReceivedAt != nil {
		return res.DataStatus.LastReceivedAt.UTC().Format(time.RFC3339)
	}
	return ""
}

// wowMetrics are the catalog value metrics the WoW detector reads. Count
// metrics only — rates and breakdowns don't carry a comparable Previous.
var wowMetrics = []string{storage.MetricActiveUsers, storage.MetricNewUsers, storage.MetricSessions}

// detectWoWDeltas flags a headline count metric that moved more than
// WoWDeltaThreshold vs the prior window, past the count floor.
func detectWoWDeltas(cur storage.OverviewResult) []finding {
	out := []finding{}
	for _, key := range wowMetrics {
		def, ok := storage.MetricCatalogEntry(key)
		if !ok {
			continue
		}
		reading, err := storage.MetricReadingFor(def, cur)
		if err != nil || reading.State != storage.OverviewStateOK || reading.Value == nil || reading.Previous == nil {
			continue
		}
		v, p := *reading.Value, *reading.Previous
		if v < WoWDeltaMinCount && p < WoWDeltaMinCount {
			continue
		}
		var delta float64
		if p == 0 {
			delta = 1 // 0 → N is a 100%+ rise by definition
		} else {
			delta = (float64(v) - float64(p)) / float64(p)
		}
		if delta < WoWDeltaThreshold && delta > -WoWDeltaThreshold {
			continue
		}
		dir := "up"
		if delta < 0 {
			dir = "down"
		}
		out = append(out, finding{
			dedupeKey: "wow:" + key,
			category:  "growth",
			title:     fmt.Sprintf("%s %s %.0f%% week over week", reading.Label, dir, abs(delta)*100),
			rationale: fmt.Sprintf(
				"%s moved from %d to %d %s (%+.0f%%) between the prior 7-day window and this one. Check the trend on the overview and the sources that drove the change.",
				reading.Label, p, v, reading.Unit, delta*100),
			impact: impactFor(abs(delta) * 100),
			evidence: evidenceEnvelope{
				QueryRef:      map[string]any{"kind": "metric", "id_or_definition": key, "version": reading.MetricVersion},
				MetricVersion: reading.MetricVersion,
				Range:         rangeLabel(cur.Context.Range, cur.Context.PreviousRange),
				Timezone:      cur.Context.Timezone,
				Watermark:     watermark(cur),
				Detector:      "wow_delta",
				Observed:      map[string]any{"metric": key, "value": v, "previous": p, "delta_pct": delta * 100},
			},
		})
	}
	return out
}

// detectOffTrack flags a metric whose declared target judged this window
// off_track — the 003 verdict surface consumed as a detector input. at_risk
// stays silent: inside the band is the target working as designed, not a
// finding.
func detectOffTrack(cur storage.OverviewResult) []finding {
	out := []finding{}
	for _, def := range storage.MetricCatalog() {
		reading, err := storage.MetricReadingFor(def, cur)
		if err != nil || reading.Target == nil || reading.Target.Verdict != storage.TargetVerdictOffTrack {
			continue
		}
		out = append(out, finding{
			dedupeKey: "target:" + def.Key,
			category:  "growth",
			title:     fmt.Sprintf("%s is off track against its target", reading.Label),
			rationale: fmt.Sprintf(
				"%s read %s against target %q for this window — the declared target judged the reading off_track. Review the metric on the overview and decide whether the plan or the target needs to move.",
				reading.Label, readingValue(reading), reading.Target.Label),
			impact: 70,
			evidence: evidenceEnvelope{
				QueryRef:      map[string]any{"kind": "metric", "id_or_definition": def.Key, "version": reading.MetricVersion},
				MetricVersion: reading.MetricVersion,
				Range:         rangeLabel(cur.Context.Range, cur.Context.PreviousRange),
				Timezone:      cur.Context.Timezone,
				Watermark:     watermark(cur),
				Detector:      "target_off_track",
				Observed: map[string]any{
					"metric": def.Key, "verdict": reading.Target.Verdict,
					"target": reading.Target.Label, "target_version": reading.Target.Version,
				},
			},
		})
	}
	return out
}

// readingValue renders a reading's measured value for prose — the count for
// value metrics, the percent for retention rates.
func readingValue(r storage.MetricReading) string {
	if r.Value != nil {
		return fmt.Sprintf("%d %s", *r.Value, r.Unit)
	}
	if r.Rate != nil {
		return fmt.Sprintf("%.1f%%", *r.Rate)
	}
	return "an unmeasurable value"
}

// detectSourceShift flags a material change in the referrer-channel mix: the
// top channel changed, or one channel's share moved more than
// SourceShiftSharePP points between the two windows.
func detectSourceShift(cur, prior storage.OverviewResult) []finding {
	curRows, priorRows := cur.Content.TopSources.Rows, prior.Content.TopSources.Rows
	var curTotal, priorTotal uint64
	for _, r := range curRows {
		curTotal += r.Count
	}
	for _, r := range priorRows {
		priorTotal += r.Count
	}
	if curTotal < SourceShiftMinPageviews || priorTotal < SourceShiftMinPageviews || len(curRows) == 0 || len(priorRows) == 0 {
		return nil
	}
	share := func(rows []storage.PathCount, total uint64) map[string]float64 {
		m := make(map[string]float64, len(rows))
		for _, r := range rows {
			m[r.Value] = float64(r.Count) / float64(total) * 100
		}
		return m
	}
	curShare, priorShare := share(curRows, curTotal), share(priorRows, priorTotal)

	evidence := func(observed map[string]any) evidenceEnvelope {
		return evidenceEnvelope{
			QueryRef:      map[string]any{"kind": "metric", "id_or_definition": storage.MetricTopSources, "version": cur.Context.MetricVersion},
			MetricVersion: cur.Context.MetricVersion,
			Range:         rangeLabel(cur.Context.Range, prior.Context.Range),
			Timezone:      cur.Context.Timezone,
			Watermark:     watermark(cur),
			Detector:      "source_shift",
			Observed:      observed,
		}
	}

	out := []finding{}
	if top := curRows[0].Value; top != priorRows[0].Value {
		out = append(out, finding{
			dedupeKey: "source_shift:top",
			category:  "growth",
			title:     fmt.Sprintf("Top acquisition channel changed: %s → %s", priorRows[0].Value, top),
			rationale: fmt.Sprintf(
				"%s led the prior 7-day window at %.0f%% of pageviews; %s leads this one at %.0f%%. Check whether the new mix is a campaign landing or a channel drying up.",
				priorRows[0].Value, priorShare[priorRows[0].Value], top, curShare[top]),
			impact:   60,
			evidence: evidence(map[string]any{"prior_top": priorRows[0].Value, "current_top": top, "prior_share_pct": priorShare[priorRows[0].Value], "current_share_pct": curShare[top]}),
		})
	}
	for channel, cs := range curShare {
		move := cs - priorShare[channel]
		if move < SourceShiftSharePP && move > -SourceShiftSharePP {
			continue
		}
		out = append(out, finding{
			dedupeKey: "source_shift:" + channel,
			category:  "growth",
			title:     fmt.Sprintf("Channel %s moved %+.0fpp of traffic share", channel, move),
			rationale: fmt.Sprintf(
				"%s went from %.0f%% to %.0f%% of pageviews between the two windows. A swing that size usually means a campaign started or a referral source stopped sending.",
				channel, priorShare[channel], cs),
			impact:   impactFor(abs(move)),
			evidence: evidence(map[string]any{"channel": channel, "prior_share_pct": priorShare[channel], "current_share_pct": cs, "move_pp": move}),
		})
	}
	return out
}

// detectStaleData flags capture and sync health the overview already
// measures: the project went quiet after receiving events, or a configured
// source is erroring, paused, or past its freshness expectation.
func detectStaleData(cur storage.OverviewResult, now time.Time) []finding {
	out := []finding{}
	ds := cur.DataStatus
	if ds.EverReceived && ds.State == "quiet" {
		out = append(out, finding{
			dedupeKey: "stale:capture",
			category:  "data",
			title:     "Event capture has gone quiet",
			rationale: fmt.Sprintf(
				"The project has received events before but nothing arrived for %.0f hours — the SDK, a deploy, or an upstream outage may have stopped the feed. Check the integration before the gap becomes a reporting hole.",
				float64(ds.AgeSeconds)/3600),
			impact: 80,
			evidence: evidenceEnvelope{
				QueryRef:  map[string]any{"kind": "metric", "id_or_definition": "data_status", "version": cur.Context.MetricVersion},
				Range:     rangeLabel(cur.Context.Range, cur.Context.PreviousRange),
				Timezone:  cur.Context.Timezone,
				Watermark: watermark(cur),
				Detector:  "stale_data",
				Observed:  map[string]any{"state": ds.State, "age_seconds": ds.AgeSeconds},
			},
		})
	}
	for _, src := range ds.Sources {
		var reason, detail string
		switch {
		case src.State == "error":
			reason, detail = "error", fmt.Sprintf("its last sync failed (%s)", src.LastError)
		case src.State == "paused":
			reason, detail = "paused", "the sync is paused"
		case src.SyncConfigured && src.Enabled && src.LastSuccessAt != nil && now.Sub(*src.LastSuccessAt) > StaleSourceAfter:
			reason = "stale"
			detail = fmt.Sprintf("its last successful sync was %.0f hours ago", now.Sub(*src.LastSuccessAt).Hours())
		case src.SyncConfigured && src.Enabled && src.LastSuccessAt == nil && src.LastRunAt != nil:
			reason, detail = "stale", "it has run but never succeeded"
		default:
			continue
		}
		out = append(out, finding{
			dedupeKey: "stale:source:" + src.ConnectorID,
			category:  "data",
			title:     fmt.Sprintf("Source %s is %s", src.ConnectorName, reason),
			rationale: fmt.Sprintf(
				"The %s source (%s) needs attention: %s. Anything it feeds — dashboards, metrics, findings — is reading data that stopped moving.",
				src.ConnectorName, src.ConnectorKind, detail),
			impact: 65,
			evidence: evidenceEnvelope{
				QueryRef:  map[string]any{"kind": "metric", "id_or_definition": "data_status", "version": cur.Context.MetricVersion},
				Range:     rangeLabel(cur.Context.Range, cur.Context.PreviousRange),
				Timezone:  cur.Context.Timezone,
				Watermark: watermark(cur),
				Detector:  "stale_data",
				Observed: map[string]any{
					"connector_id": src.ConnectorID, "connector_name": src.ConnectorName,
					"state": src.State, "reason": reason,
				},
			},
		})
	}
	return out
}

// detectFunnelDrop re-runs one declared watch over the current and prior 7d
// windows and flags the worst step whose conversion fell more than
// FunnelDropPP points. Returns nil when nothing fired.
func detectFunnelDrop(ctx context.Context, st ProjectStore, projectID string, w storage.FunnelWatch, now time.Time) (*finding, error) {
	run := func(from, to time.Time) ([]storage.FunnelStep, error) {
		res, err := st.RunInsight(ctx, projectID, "funnel", "users", w.Steps,
			storage.EventFilter{From: from, To: to, HumansOnly: true})
		if err != nil {
			return nil, err
		}
		return res.Funnel, nil
	}
	cur, err := run(now.Add(-7*24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	prior, err := run(now.Add(-14*24*time.Hour), now.Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}

	priorByStep := make(map[int]storage.FunnelStep, len(prior))
	for _, s := range prior {
		priorByStep[s.Step] = s
	}
	var worst *finding
	worstDrop := 0.0
	for _, step := range cur {
		if step.Step == 1 {
			continue // step 1 converts at 100% by definition
		}
		prev, ok := priorByStep[step.Step]
		if !ok {
			continue
		}
		if step.Users < FunnelMinUsers && prev.Users < FunnelMinUsers {
			continue
		}
		drop := (prev.Conversion - step.Conversion) * 100
		if drop < FunnelDropPP {
			continue
		}
		if drop <= worstDrop {
			continue
		}
		worstDrop = drop
		worst = &finding{
			dedupeKey: "funnel:" + w.ID,
			category:  "product",
			title:     fmt.Sprintf("Funnel %q step %d conversion fell %.0fpp", w.Name, step.Step, drop),
			rationale: fmt.Sprintf(
				"Step %d (%s) converted %.0f%% of entrants this week vs %.0f%% the week before — the worst drop in the watched funnel. Reproduce the step and check what shipped between the windows.",
				step.Step, step.EventName, step.Conversion*100, prev.Conversion*100),
			impact: impactFor(drop),
			evidence: evidenceEnvelope{
				QueryRef:  map[string]any{"kind": "funnel_watch", "id_or_definition": w.ID, "version": curVersion(cur)},
				Range:     fmt.Sprintf("%s → %s vs %s → %s", now.Add(-7*24*time.Hour).Format("2006-01-02"), now.Format("2006-01-02"), now.Add(-14*24*time.Hour).Format("2006-01-02"), now.Add(-7*24*time.Hour).Format("2006-01-02")),
				Detector:  "funnel_drop",
				Observed: map[string]any{
					"watch_id": w.ID, "watch_name": w.Name, "steps": w.Steps,
					"step": step.Step, "event": step.EventName,
					"conversion_pct": step.Conversion * 100, "prior_conversion_pct": prev.Conversion * 100,
					"drop_pp": drop,
				},
			},
		}
	}
	return worst, nil
}

// curVersion keeps the funnel evidence's version field honest: the funnel
// engine has no metric version of its own, so the finding cites the insight
// type it ran.
func curVersion(steps []storage.FunnelStep) string {
	return fmt.Sprintf("funnel/%d-steps", len(steps))
}

func impactFor(magnitudePct float64) float64 {
	switch {
	case magnitudePct >= 60:
		return 80
	case magnitudePct >= 40:
		return 65
	default:
		return 50
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
