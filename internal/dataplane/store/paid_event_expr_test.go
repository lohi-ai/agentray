package storage

import (
	"strings"
	"testing"
)

const openExprFixture = "(event_name = 'sub_start' OR event_name = 'sub_renew')"

// The mapping UI collects an "Amount property" (web/modules/cohorts/audience-manager.tsx).
// It was stored, validated and round-tripped without ever reaching a query, so a
// product whose money rides on its own subscription events had an empty Paid
// audience while having paying subscribers.
func TestPaidEventExprConsumesTheConfiguredAmountProperty(t *testing.T) {
	got := paidEventExpr(SubscriptionMapping{AmountProp: "lt_cost"}, openExprFixture)
	if !strings.Contains(got, "'lt_cost'") {
		t.Errorf("configured amount property never reached the predicate: %s", got)
	}
	if !strings.Contains(got, openExprFixture) {
		t.Errorf("amount is not scoped to the subscription-open events: %s", got)
	}
}

// Strictly additive: whatever the mapping says, a row the SDK-shaped half
// recognised as a payment must still be recognised. Widening "ever paid" must
// never un-pay someone.
func TestPaidEventExprAlwaysKeepsTheSDKShape(t *testing.T) {
	mappings := []SubscriptionMapping{
		{},
		{AmountProp: "amount"},
		{AmountProp: "lt_cost"},
		{AmountProp: "   "},
	}
	for _, m := range mappings {
		got := paidEventExpr(m, openExprFixture)
		if !strings.Contains(got, "event_name = 'revenue'") || !strings.Contains(got, "JSONExtractFloat(properties, 'amount') > 0") {
			t.Errorf("SDK-shaped payment detection dropped for %+v: %s", m, got)
		}
	}
}

// An unconfigured or blank amount property must not widen the predicate at all —
// no dangling OR, no empty property name extracted.
func TestPaidEventExprIgnoresBlankConfiguration(t *testing.T) {
	base := paidEventExpr(SubscriptionMapping{}, openExprFixture)
	if strings.Contains(base, " OR ") {
		t.Errorf("blank amount property still widened the predicate: %s", base)
	}
	if got := paidEventExpr(SubscriptionMapping{AmountProp: "amount"}, ""); got != base {
		t.Errorf("missing openExpr must fall back to the SDK shape, got: %s", got)
	}
}

// The amount property is operator-supplied config, so it must be escaped like
// every other mapped token rather than concatenated raw.
func TestPaidEventExprEscapesTheAmountProperty(t *testing.T) {
	got := paidEventExpr(SubscriptionMapping{AmountProp: "am'ount"}, openExprFixture)
	if strings.Contains(got, "'am'ount'") {
		t.Errorf("amount property was not escaped: %s", got)
	}
	if !strings.Contains(got, chStringLit("am'ount")) {
		t.Errorf("expected chStringLit escaping in: %s", got)
	}
}
