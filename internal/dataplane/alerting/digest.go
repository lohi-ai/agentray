package alerting

import (
	"fmt"
	"math"
	"sort"
	"strings"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// digestMover is a catalog metric with a meaningful week-over-week comparison.
type digestMover struct {
	Key      string
	Unit     string
	Value    uint64
	Previous uint64
	Change   float64
}

const maxDigestItemRunes = 160

func truncateDigestItem(value string) string {
	runes := []rune(value)
	if len(runes) <= maxDigestItemRunes {
		return value
	}
	return string(runes[:maxDigestItemRunes]) + "…"
}

// digestMovers projects the catalog through the shared Overview result and
// ranks only honest value readings. A zero baseline has no relative delta, so
// it is omitted rather than presented as an invented percentage.
func digestMovers(overview storage.OverviewResult) []digestMover {
	movers := make([]digestMover, 0)
	for _, definition := range storage.MetricCatalog() {
		reading, err := storage.MetricReadingFor(definition, overview)
		if err != nil || reading.State != storage.OverviewStateOK || reading.Value == nil || reading.Previous == nil || *reading.Previous == 0 {
			continue
		}
		change := (float64(*reading.Value) - float64(*reading.Previous)) / float64(*reading.Previous)
		if change == 0 {
			continue
		}
		unit := reading.Unit
		if definition.Key == storage.MetricRevenue {
			if reading.Revenue == nil || reading.Revenue.Currency == "" {
				continue
			}
			unit = reading.Revenue.Currency
		}
		movers = append(movers, digestMover{
			Key: definition.Key, Unit: unit, Value: *reading.Value, Previous: *reading.Previous, Change: change,
		})
	}
	sort.Slice(movers, func(i, j int) bool {
		left, right := math.Abs(movers[i].Change), math.Abs(movers[j].Change)
		if left == right {
			return movers[i].Key < movers[j].Key
		}
		return left > right
	})
	return movers
}

// digestNotification turns the bounded store reads into a decision-shaped,
// self-contained plain-text message. The structured citations are carried in
// Data for webhook consumers; Slack and email intentionally render the text.
func digestNotification(name string, overview storage.OverviewResult, data storage.DigestData) (Notification, bool) {
	movers := digestMovers(overview)
	if len(movers) == 0 && len(data.Recommendations) == 0 && len(data.Decisions) == 0 && len(data.StaleSyncs) == 0 {
		return Notification{
			Title: fmt.Sprintf("Weekly digest: %s", name),
			Body:  "All quiet — no material changes this week.",
			Level: "info",
			Data:  map[string]any{"metrics": []string{}, "findings": []string{}, "tests": []string{}, "stale_sources": []string{}},
		}, false
	}

	lines := []string{"What changed this week:"}
	citations := map[string]any{
		"metrics":       make([]string, 0, len(movers)),
		"findings":      make([]string, 0, len(data.Recommendations)),
		"tests":         make([]string, 0, len(data.Decisions)),
		"stale_sources": make([]string, 0, len(data.StaleSyncs)),
		"truncated": map[string]bool{
			"findings":      data.RecommendationsTruncated,
			"tests":         data.DecisionsTruncated,
			"stale_sources": data.StaleSyncsTruncated,
		},
	}
	if len(movers) > 0 {
		lines = append(lines, "Top movers:")
		for _, mover := range movers {
			lines = append(lines, fmt.Sprintf("- %s: %d → %d %s (%+.1f%%) [metric:%s]", mover.Key, mover.Previous, mover.Value, mover.Unit, mover.Change*100, mover.Key))
			citations["metrics"] = append(citations["metrics"].([]string), mover.Key)
		}
	}
	if len(data.Recommendations) > 0 {
		lines = append(lines, "New findings:")
		for _, recommendation := range data.Recommendations {
			lines = append(lines, fmt.Sprintf("- %s [finding:%s]", truncateDigestItem(recommendation.Title), recommendation.ID))
			citations["findings"] = append(citations["findings"].([]string), recommendation.ID)
		}
	}
	if len(data.Decisions) > 0 {
		lines = append(lines, "Decided tests:")
		for _, test := range data.Decisions {
			lines = append(lines, fmt.Sprintf("- %s: %s [test:%s]", truncateDigestItem(test.Hypothesis), test.Status, test.ID))
			citations["tests"] = append(citations["tests"].([]string), test.ID)
		}
	}
	if len(data.StaleSyncs) > 0 {
		lines = append(lines, "Stale sources:")
		for _, sync := range data.StaleSyncs {
			lines = append(lines, fmt.Sprintf("- %s [sync:%s]", truncateDigestItem(sync.SourceTable), sync.ID))
			citations["stale_sources"] = append(citations["stale_sources"].([]string), sync.ID)
		}
	}
	if data.RecommendationsTruncated || data.DecisionsTruncated || data.StaleSyncsTruncated {
		lines = append(lines, "Additional items were omitted; open the dashboard for the complete list.")
	}
	return Notification{
		Title: fmt.Sprintf("Weekly digest: %s", name),
		Body:  strings.Join(lines, "\n"),
		Level: "info",
		Data:  citations,
	}, true
}
