package storage

import "testing"

// Every plan surface reads NormalizePlan's output directly, so an unknown or
// blank value must land on a real plan rather than an empty badge.
func TestNormalizePlan(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", PlanFree},
		{"free", PlanFree},
		{"FREE", PlanFree},
		{"  solo  ", PlanSolo},
		{"Solo", PlanSolo},
		{"team", PlanTeam},
		{"enterprise", PlanFree},
		{"null", PlanFree},
	}
	for _, tc := range cases {
		if got := NormalizePlan(tc.in); got != tc.want {
			t.Errorf("NormalizePlan(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// PlanIDs is the pricing page's display order and the "is this an upgrade"
// comparison order; a reorder is a product change, not a refactor.
func TestPlanIDsOrder(t *testing.T) {
	want := []string{PlanFree, PlanSolo, PlanTeam}
	if len(PlanIDs) != len(want) {
		t.Fatalf("PlanIDs = %v, want %v", PlanIDs, want)
	}
	for i, id := range want {
		if PlanIDs[i] != id {
			t.Errorf("PlanIDs[%d] = %q, want %q", i, PlanIDs[i], id)
		}
	}
}
