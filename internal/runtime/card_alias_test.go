package agentruntime

import "testing"

// The run's answer is attached to whatever its LAST SQL query returned, and the
// last query is routinely a throwaway check. A retention answer arrived with a
// card headed "Count()" reading 0 stapled under it — the finding said 24%, the
// card said nothing, and the card is the part a reader looks at first.

// TestUnaliasedScalarProducesNoCard is the reported case, exactly.
func TestUnaliasedScalarProducesNoCard(t *testing.T) {
	if card := cardFromRows(`{"rows":[{"count()":0}]}`); card != nil {
		t.Fatalf("built a card titled %q from an un-aliased probe query; the prose answer must stand alone", card.Title)
	}
}

// TestAliasedScalarStillProducesACard keeps the affordance: an analyst that
// names a column is asking for it to be shown, and that must keep working.
func TestAliasedScalarStillProducesACard(t *testing.T) {
	card := cardFromRows(`{"rows":[{"paying_customers":7}]}`)
	if card == nil {
		t.Fatal("an aliased scalar is the analyst saying what to show; it must still render")
	}
	if card.Title != "Paying customers" {
		t.Fatalf("title = %q, want the alias read back as a label", card.Title)
	}
	if len(card.Stats) != 1 || card.Stats[0].Value != "7" {
		t.Fatalf("stats = %+v, want the single value", card.Stats)
	}
}

// TestUnaliasedSeriesColumnProducesNoCard covers the two-column shape: a chart
// whose axis is labelled with the SQL that drew it is not a chart anyone reads.
func TestUnaliasedSeriesColumnProducesNoCard(t *testing.T) {
	if card := cardFromRows(`{"rows":[{"event_name":"signup","count()":12},{"event_name":"login","count()":30}]}`); card != nil {
		t.Fatalf("built a series card labelled %q from an un-aliased aggregate", card.Unit)
	}
}

// TestAliasedSeriesStillProducesACard is the same shape done right.
func TestAliasedSeriesStillProducesACard(t *testing.T) {
	card := cardFromRows(`{"rows":[{"event_name":"signup","people":12},{"event_name":"login","people":30}]}`)
	if card == nil || card.Kind != "series" || len(card.Points) != 2 {
		t.Fatalf("aliased two-column result must still chart, got %+v", card)
	}
}

// TestIsNamedColumnRejectsRawSQL pins the rule itself: a column named after an
// expression was never named at all.
func TestIsNamedColumnRejectsRawSQL(t *testing.T) {
	for _, raw := range []string{"count()", "uniqExact(canonical_id)", "count(*)", "", "   "} {
		if isNamedColumn(raw) {
			t.Fatalf("%q is raw SQL, not a label", raw)
		}
	}
	for _, named := range []string{"people", "total_users", "retention_rate"} {
		if !isNamedColumn(named) {
			t.Fatalf("%q is a perfectly good alias and must be shown", named)
		}
	}
}
