package agentruntime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

const maxCardPoints = 24

// cardFromMessages derives a result card from the analyst's tool outputs so a
// data answer renders a stat/chart, not text alone. It scans the run's messages
// newest-first for the most informative analytics result (an insight, then a
// SQL result) and shapes it into a stat or series card. Returns nil when no
// result fits — the caller falls back to prose (risk: structured data missing).
func cardFromMessages(messages []agentcore.Message) *agentcore.ResultCard {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != agentcore.RoleTool {
			continue
		}
		switch m.Name {
		case "run_insight":
			if card := cardFromInsight(m.Content); card != nil {
				return card
			}
		case "run_sql":
			if card := cardFromRows(m.Content); card != nil {
				return card
			}
		}
	}
	return nil
}

// cardFromInsight shapes a run_insight result into a card: a timeseries becomes a
// series card, a funnel/retention becomes a stat block.
func cardFromInsight(content string) *agentcore.ResultCard {
	var ins storage.InsightResult
	if err := json.Unmarshal([]byte(content), &ins); err != nil {
		return nil
	}
	title := strings.TrimSpace(ins.Title)
	if title == "" {
		title = humanizeMetric(ins.Metric)
	}

	switch {
	case len(ins.Series) > 0:
		points := make([]agentcore.CardPoint, 0, len(ins.Series))
		for _, p := range tailSeries(ins.Series) {
			points = append(points, agentcore.CardPoint{Label: p.Hour.Format("Jan 2 15:04"), Value: float64(p.Count)})
		}
		return &agentcore.ResultCard{Title: title, Kind: "series", Unit: humanizeMetric(ins.Metric), Points: points}

	case len(ins.Funnel) > 0:
		stats := make([]agentcore.CardStat, 0, len(ins.Funnel))
		for _, s := range ins.Funnel {
			stats = append(stats, agentcore.CardStat{
				Label: s.EventName,
				Value: fmt.Sprintf("%d (%.0f%%)", s.Users, s.Conversion*pctScale(s.Conversion)),
			})
		}
		return &agentcore.ResultCard{Title: orDefault(title, "Funnel"), Kind: "stat", Stats: stats}

	case len(ins.Retention) > 0:
		points := make([]agentcore.CardPoint, 0, len(ins.Retention))
		for _, r := range ins.Retention {
			points = append(points, agentcore.CardPoint{Label: r.Period, Value: r.Rate * pctScale(r.Rate)})
		}
		return &agentcore.ResultCard{Title: orDefault(title, "Retention"), Kind: "series", Unit: "%", Points: points}
	}
	return nil
}

// cardFromRows shapes a run_sql result into a card when the rows have a clean
// shape: a single numeric cell becomes a one-stat card; a {label, number} table
// becomes a series. Anything wider/ambiguous returns nil (prose fallback).
func cardFromRows(content string) *agentcore.ResultCard {
	var out struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil || len(out.Rows) == 0 {
		return nil
	}

	// Single scalar result -> one stat (e.g. SELECT count() AS total).
	if len(out.Rows) == 1 && len(out.Rows[0]) == 1 {
		for k, v := range out.Rows[0] {
			if num, ok := asNumber(v); ok {
				return &agentcore.ResultCard{
					Title: humanizeMetric(k), Kind: "stat",
					Stats: []agentcore.CardStat{{Label: humanizeMetric(k), Value: formatNumber(num)}},
				}
			}
		}
		return nil
	}

	// A {label column, numeric column} table -> series. Resolve the two columns
	// from the first row; bail if it isn't exactly one label + one number.
	labelKey, valueKey, ok := labelValueKeys(out.Rows[0])
	if !ok {
		return nil
	}
	points := make([]agentcore.CardPoint, 0, len(out.Rows))
	for _, row := range out.Rows {
		num, ok := asNumber(row[valueKey])
		if !ok {
			return nil
		}
		points = append(points, agentcore.CardPoint{Label: fmt.Sprint(row[labelKey]), Value: num})
		if len(points) >= maxCardPoints {
			break
		}
	}
	return &agentcore.ResultCard{Title: humanizeMetric(valueKey), Kind: "series", Unit: humanizeMetric(valueKey), Points: points}
}

// labelValueKeys returns the (string-ish label, numeric value) column names from
// a two-column row, or ok=false when the shape doesn't fit.
func labelValueKeys(row map[string]any) (string, string, bool) {
	if len(row) != 2 {
		return "", "", false
	}
	var labelKey, valueKey string
	for k, v := range row {
		if _, isNum := asNumber(v); isNum {
			valueKey = k
		} else {
			labelKey = k
		}
	}
	if labelKey == "" || valueKey == "" {
		return "", "", false
	}
	return labelKey, valueKey, true
}

// tailSeries caps a timeseries to the most recent maxCardPoints points.
func tailSeries(s []storage.TimelinePoint) []storage.TimelinePoint {
	if len(s) <= maxCardPoints {
		return s
	}
	return s[len(s)-maxCardPoints:]
}

// asNumber coerces a JSON-decoded value to float64 when it is numeric.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// pctScale returns 100 when a rate is expressed as a 0–1 fraction, else 1, so
// both fraction and already-percent conventions render as a percentage.
func pctScale(rate float64) float64 {
	if rate > 0 && rate <= 1 {
		return 100
	}
	return 1
}

// formatNumber renders a metric value without a trailing ".0" for whole numbers.
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}

// humanizeMetric turns a snake_case metric/column into a readable label.
func humanizeMetric(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Result"
	}
	s = strings.ReplaceAll(s, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
