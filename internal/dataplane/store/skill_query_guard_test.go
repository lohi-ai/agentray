package storage

import "testing"

// The exact query shapes the new revenue-integrity and money-sized-funnel skills
// tell agents to run. If the guard rejects these, the skills are inert.
func TestSkillPrescribedRevenueQueriesPassTheGuard(t *testing.T) {
	qs := []string{
		"SELECT event_name, count() AS n, countIf(JSONExtractString(properties,'lt_cost') != '') AS with_amount FROM events WHERE event_name IN ('subscription_activated','subscription_cancelled') GROUP BY event_name",
		"SELECT avg(toFloat64OrNull(JSONExtractString(properties,'amount'))) FROM events WHERE event_name = 'revenue'",
		"SELECT event_name, arrayDistinct(groupArrayArray(JSONExtractKeys(properties))) AS keys FROM events WHERE event_name IN ('subscription_started','subscription_cancelled') GROUP BY event_name",
		"SELECT JSONExtractString(properties,'currency') AS cur, sum(JSONExtractFloat(properties,'amount')) FROM events WHERE event_name = 'revenue' GROUP BY cur",
		"SELECT event_name, count() AS n FROM events WHERE event_name IN ('tts_requested','audio_upsell_shown','audio_upsell_clicked','subscription_started','subscription_activated') GROUP BY event_name",
	}
	for _, q := range qs {
		if err := validateReadonlySQL(q); err != nil {
			t.Errorf("skill-prescribed query rejected (%v): %s", err, q)
		}
	}
}
