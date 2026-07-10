package storage

import (
	"testing"
	"time"
)

func TestBudgetExceeded(t *testing.T) {
	cases := []struct {
		name   string
		budget AgentBudget
		spend  BudgetSpend
		want   bool
	}{
		{"all zero limits never trip", AgentBudget{}, BudgetSpend{CostUSD: 999, Tokens: 1e9, Runs: 1000}, false},
		{"cost under cap", AgentBudget{MaxCostUSD: 0.50}, BudgetSpend{CostUSD: 0.49}, false},
		{"cost at cap trips", AgentBudget{MaxCostUSD: 0.50}, BudgetSpend{CostUSD: 0.50}, true},
		{"cost over cap trips", AgentBudget{MaxCostUSD: 0.50}, BudgetSpend{CostUSD: 0.51}, true},
		{"tokens at cap trips", AgentBudget{MaxTokens: 1000}, BudgetSpend{Tokens: 1000}, true},
		{"runs at cap trips", AgentBudget{MaxRuns: 5}, BudgetSpend{Runs: 5}, true},
		{"runs under cap", AgentBudget{MaxRuns: 5}, BudgetSpend{Runs: 4}, false},
		{"one dimension of many trips", AgentBudget{MaxCostUSD: 1, MaxTokens: 10, MaxRuns: 100}, BudgetSpend{CostUSD: 0.1, Tokens: 10, Runs: 1}, true},
	}
	for _, c := range cases {
		got, reason := budgetExceeded(c.budget, c.spend)
		if got != c.want {
			t.Errorf("%s: budgetExceeded=%v want %v (reason %q)", c.name, got, c.want, reason)
		}
		if got && reason == "" {
			t.Errorf("%s: exceeded but empty reason", c.name)
		}
	}
}

func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 7, 2, 15, 30, 45, 0, time.UTC)
	if got := periodStart("day", now); !got.Equal(time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day start=%v", got)
	}
	if got := periodStart("month", now); !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month start=%v", got)
	}
}

func TestNormalizeBudgetPeriod(t *testing.T) {
	if p, err := normalizeBudgetPeriod(""); err != nil || p != "day" {
		t.Errorf("empty -> %q %v", p, err)
	}
	if p, err := normalizeBudgetPeriod("month"); err != nil || p != "month" {
		t.Errorf("month -> %q %v", p, err)
	}
	if _, err := normalizeBudgetPeriod("week"); err == nil {
		t.Error("week should be rejected")
	}
}
