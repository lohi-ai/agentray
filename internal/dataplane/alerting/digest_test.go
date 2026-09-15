package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func uint64Pointer(v uint64) *uint64 { return &v }

func TestDigestNotificationCitesDecisionInputs(t *testing.T) {
	overview := storage.OverviewResult{
		DataStatus: storage.OverviewDataStatus{EverReceived: true},
		Metrics: storage.OverviewMetrics{
			ActiveUsers: storage.OverviewMetric{State: storage.OverviewStateOK, Value: uint64Pointer(150), Previous: uint64Pointer(100)},
		},
	}
	notification, hasChanges := digestNotification("Weekly review", overview, storage.DigestData{
		Recommendations: []storage.DigestRecommendation{{ID: "finding-1", Title: "Repair checkout"}},
		Decisions:       []storage.DigestDecision{{ID: "test-1", Hypothesis: "Keep onboarding", Status: storage.TestPassed}},
		StaleSyncs:      []storage.DigestStaleSync{{ID: "sync-1", SourceTable: "warehouse.orders"}},
	})
	if !hasChanges {
		t.Fatal("digest with decision inputs must not be quiet")
	}
	for _, citation := range []string{"[metric:active_users]", "[finding:finding-1]", "[test:test-1]", "[sync:sync-1]"} {
		if !strings.Contains(notification.Body, citation) {
			t.Errorf("body missing %s: %q", citation, notification.Body)
		}
	}
	if metrics, ok := notification.Data["metrics"].([]string); !ok || len(metrics) == 0 || metrics[0] != "active_users" {
		t.Fatalf("metric citations = %#v", notification.Data["metrics"])
	}
}

func TestDigestQuietWeekHonorsSendEmptyAtDelivery(t *testing.T) {
	notification, hasChanges := digestNotification("Weekly review", storage.OverviewResult{}, storage.DigestData{})
	if hasChanges || notification.Body != "All quiet — no material changes this week." {
		t.Fatalf("quiet digest = (%q, %v)", notification.Body, hasChanges)
	}

	var delivered Notification
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&delivered); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	deliverer := NewDeliverer(nil)
	deliverer.client = server.Client()
	channel := storage.AlertChannel{Kind: "webhook", Config: json.RawMessage(`{"url":"` + server.URL + `"}`)}
	if err := deliverer.Deliver(context.Background(), channel, notification); err != nil {
		t.Fatalf("deliver quiet digest: %v", err)
	}
	if delivered.Body != notification.Body || delivered.Level != "info" {
		t.Fatalf("delivered = %#v", delivered)
	}
}

func TestDigestNotificationTreatsStableMetricsAsQuiet(t *testing.T) {
	overview := storage.OverviewResult{
		DataStatus: storage.OverviewDataStatus{EverReceived: true},
		Metrics: storage.OverviewMetrics{
			ActiveUsers: storage.OverviewMetric{State: storage.OverviewStateOK, Value: uint64Pointer(100), Previous: uint64Pointer(100)},
		},
	}
	notification, hasChanges := digestNotification("Weekly review", overview, storage.DigestData{})
	if hasChanges || notification.Body != "All quiet — no material changes this week." {
		t.Fatalf("stable digest = (%q, %v)", notification.Body, hasChanges)
	}
}
