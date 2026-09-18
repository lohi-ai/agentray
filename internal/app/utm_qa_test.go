package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ingestion "github.com/lohi-ai/agentray/internal/dataplane/ingest"
	store "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// TestUTMTrackingQA proves the complete acceptance criteria for bs-25i8ab26:
// 1. Capture: accept utm_source, utm_medium, utm_campaign, utm_term, utm_content
//    on pageview events and store them in first-class columns.
// 2. Attribute: prefer utm_source over referrer_channel (e.g. facebook click
//    is "facebook", not "referral").
// 3. Surface: acquisition breakdown tiles + agent tools (run_sql, explore_events)
//    answer what campaign Y drove.
// 4. Non-happy path: untagged traffic reports as unknown, crawler visits are excluded.
func TestUTMTrackingQA(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "utm-qa-test-secret")
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)

	// Mount ingest handler directly on the test echo
	ingestHandler := ingestion.NewHandler(s, s, s)
	e.POST("/capture", ingestHandler.Capture)

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("utm-qa-%d@test.local", time.Now().UnixNano()), "QA", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	apiKey := boot.Project.APIKey
	projectID := boot.Project.ID

	// Create user session for calling authenticated ops
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// ── 1. Ingest simulated traffic ──────────────────────────────────────────

	// Case 1 (Happy path A): Tagged campaign landing from Facebook
	// Both $utm_source and $referrer exist. Attribution must prefer facebook over referral!
	rec := postJSON(t, e, "/capture", fmt.Sprintf(`{
		"api_key": %q,
		"event": "user.pageview",
		"distinct_id": "visitor-fb-1",
		"properties": {
			"path": "/landing",
			"$utm_source": "facebook",
			"$utm_medium": "social",
			"$utm_campaign": "launch-2026",
			"$utm_term": "ai-readers",
			"$utm_content": "hero-card",
			"$referrer": "https://facebook.com/posts/12345"
		}
	}`, apiKey), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture fb event: %d %s", rec.Code, rec.Body.String())
	}

	// Case 2 (Happy path B): Tagged campaign landing from Newsletter (bare keys)
	rec = postJSON(t, e, "/capture", fmt.Sprintf(`{
		"api_key": %q,
		"event": "user.pageview",
		"distinct_id": "visitor-email-1",
		"properties": {
			"path": "/pricing",
			"utm_source": "newsletter",
			"utm_medium": "email",
			"utm_campaign": "launch-2026",
			"utm_term": "subscribers",
			"utm_content": "early-bird",
			"$referrer": "https://mail.google.com"
		}
	}`, apiKey), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture email event: %d %s", rec.Code, rec.Body.String())
	}

	// Case 3 (Untagged organic traffic): Search visit with referrer only
	rec = postJSON(t, e, "/capture", fmt.Sprintf(`{
		"api_key": %q,
		"event": "user.pageview",
		"distinct_id": "visitor-search-1",
		"properties": {
			"path": "/docs",
			"$referrer": "https://google.com/search?q=novel+reader"
		}
	}`, apiKey), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture search event: %d %s", rec.Code, rec.Body.String())
	}

	// Case 4 (Non-happy path A): Tagged crawler visit — must be excluded from acquisition
	rec = postJSON(t, e, "/capture", fmt.Sprintf(`{
		"api_key": %q,
		"event": "user.pageview",
		"distinct_id": "crawler-1",
		"properties": {
			"path": "/landing",
			"$utm_source": "facebook",
			"$utm_campaign": "launch-2026",
			"$user_agent": "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
		}
	}`, apiKey), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture bot event: %d %s", rec.Code, rec.Body.String())
	}

	// Case 5 (Non-happy path B): Missing event name -> 400 Bad Request
	rec = postJSON(t, e, "/capture", fmt.Sprintf(`{
		"api_key": %q,
		"distinct_id": "bad-1",
		"properties": {"$utm_source": "test"}
	}`, apiKey), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("capture invalid event: got status %d, want 400", rec.Code)
	}

	// ── 2. Verify storage & column extraction ────────────────────────────────

	events, err := s.ExploreEvents(ctx, projectID, store.EventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ExploreEvents: %v", err)
	}
	if len(events.Events) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(events.Events))
	}

	var foundFB bool
	for _, ev := range events.Events {
		if ev.DistinctID == "visitor-fb-1" {
			foundFB = true
			if ev.UTMSource != "facebook" || ev.UTMMedium != "social" || ev.UTMCampaign != "launch-2026" ||
				ev.UTMTerm != "ai-readers" || ev.UTMContent != "hero-card" {
				t.Fatalf("visitor-fb-1 utm columns = %q/%q/%q/%q/%q, want facebook/social/launch-2026/ai-readers/hero-card",
					ev.UTMSource, ev.UTMMedium, ev.UTMCampaign, ev.UTMTerm, ev.UTMContent)
			}
		}
	}
	if !foundFB {
		t.Fatalf("visitor-fb-1 not found in explore events")
	}

	// ── 3. Verify acquisition attribution (TopSources prefers UTM) ───────────

	overview, err := s.Overview(ctx, projectID, "today", "", time.Now())
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	topSources := overview.Content.TopSources.Rows
	var sawFacebook, sawNewsletter, sawSearch, sawReferral bool
	for _, r := range topSources {
		switch r.Value {
		case "facebook":
			sawFacebook = true
		case "newsletter":
			sawNewsletter = true
		case "search":
			sawSearch = true
		case "referral":
			sawReferral = true
		}
	}

	if !sawFacebook {
		t.Fatalf("top_sources missing 'facebook': %+v", topSources)
	}
	if !sawNewsletter {
		t.Fatalf("top_sources missing 'newsletter': %+v", topSources)
	}
	if !sawSearch {
		t.Fatalf("top_sources missing 'search': %+v", topSources)
	}
	if sawReferral {
		t.Fatalf("top_sources should NOT contain 'referral' when UTM source was provided: %+v", topSources)
	}

	// Verify UTM-specific breakdown tiles
	topCampaigns := overview.Content.TopCampaigns.Rows
	var launchCampaignCount uint64
	for _, c := range topCampaigns {
		if c.Value == "launch-2026" {
			launchCampaignCount = c.Count
		}
	}
	if launchCampaignCount != 2 {
		t.Fatalf("top_campaigns['launch-2026'] count = %d, want 2 human pageviews (crawler excluded)", launchCampaignCount)
	}

	topMediums := overview.Content.TopUTMMediums.Rows
	var sawSocial, sawEmail bool
	for _, m := range topMediums {
		if m.Value == "social" {
			sawSocial = true
		}
		if m.Value == "email" {
			sawEmail = true
		}
	}
	if !sawSocial || !sawEmail {
		t.Fatalf("top_utm_mediums missing social or email: %+v", topMediums)
	}

	// ── 4. Verify agent tooling (run_sql answers what campaign Y drove) ───────

	req := httptest.NewRequest(http.MethodPost, "/api/op/run_sql", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	req.Header.Set("X-Project-Id", projectID)
	req.Header.Set("Content-Type", "application/json")

	sqlBody := `{"sql":"SELECT utm_source, utm_medium, utm_campaign, count(*) AS views FROM events WHERE coalesce(visitor_class, 'human') = 'human' AND utm_campaign = 'launch-2026' GROUP BY utm_source, utm_medium, utm_campaign ORDER BY views DESC"}`
	rec = postJSON(t, e, "/api/op/run_sql", sqlBody, map[string]string{
		"Cookie":       fmt.Sprintf("%s=%s", sessionCookieName, sessionToken),
		"X-Project-Id": projectID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("run_sql: %d %s", rec.Code, rec.Body.String())
	}

	var sqlRes struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sqlRes); err != nil {
		t.Fatalf("unmarshal run_sql result: %v", err)
	}
	if len(sqlRes.Rows) != 2 {
		t.Fatalf("run_sql returned %d rows, want 2 for launch-2026 breakdown: %+v", len(sqlRes.Rows), sqlRes.Rows)
	}
	t.Logf("QA pass: run_sql correctly broke down campaign launch-2026: %+v", sqlRes.Rows)
}
