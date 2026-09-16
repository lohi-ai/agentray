package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The acquisition board's UTM and referrer tiles all read the same population
// top_sources does — human user pageviews in the window — grouped by the new
// columns. This proves the grouping against a real DuckDB file: tagged visits
// rank by their tag, untagged visits report as unknown rather than vanishing,
// and the referrer tile drops direct/internal/empty hosts.
func TestAcquisitionBreakdownGroupsUTMTags(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	projectID := "cccccccc-1111-2222-3333-444444444444"
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	pv := func(distinctID, source, campaign, refHost, refChannel string) Event {
		return Event{
			ProjectID: projectID, EventID: uuid.NewString(), EventName: "user.pageview",
			EventType: "user", DistinctID: distinctID, VisitorClass: "human",
			Timestamp: at, UTMSource: source, UTMCampaign: campaign,
			ReferrerHost: refHost, ReferrerChannel: refChannel,
		}
	}
	events := []Event{
		pv("u1", "newsletter", "launch-week", "news.example.com", "referral"),
		pv("u2", "newsletter", "launch-week", "news.example.com", "referral"),
		pv("u3", "google", "brand", "google.com", "search"),
		// Untagged: must surface as unknown, not disappear.
		pv("u4", "", "", "", "direct"),
		// A crawler's tagged pageview is not acquisition traffic.
		{ProjectID: projectID, EventID: uuid.NewString(), EventName: "user.pageview",
			EventType: "user", DistinctID: "bot-1", VisitorClass: "search-bot",
			Timestamp: at, UTMSource: "newsletter", UTMCampaign: "launch-week",
			ReferrerHost: "crawler.example.com", ReferrerChannel: "search"},
	}
	if err := d.SinkEvents(context.Background(), events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}

	r := OverviewRange{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
	where, args := overviewWindowWhere(projectID, r, "")
	qualWhere := where + " AND " + overviewQualifying

	sources, err := s.acquisitionBreakdown(context.Background(), qualWhere, args, "utm_source", "")
	if err != nil {
		t.Fatalf("utm_source breakdown: %v", err)
	}
	if len(sources) != 3 || sources[0].Value != "newsletter" || sources[0].Count != 2 {
		t.Fatalf("utm_source rows = %+v, want newsletter(2) first of 3", sources)
	}
	var sawUnknown bool
	for _, row := range sources {
		if row.Value == "unknown" && row.Count == 1 {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatalf("utm_source rows = %+v, want an unknown row for the untagged visit", sources)
	}

	campaigns, err := s.acquisitionBreakdown(context.Background(), qualWhere, args, "utm_campaign", "")
	if err != nil {
		t.Fatalf("utm_campaign breakdown: %v", err)
	}
	if len(campaigns) != 3 || campaigns[0].Value != "launch-week" || campaigns[0].Count != 2 {
		t.Fatalf("utm_campaign rows = %+v, want launch-week(2) first of 3", campaigns)
	}

	referrers, err := s.acquisitionBreakdown(context.Background(), qualWhere, args, "referrer_host",
		"referrer_channel NOT IN ('', 'direct', 'internal') AND referrer_host IS NOT NULL AND referrer_host <> ''")
	if err != nil {
		t.Fatalf("referrer breakdown: %v", err)
	}
	// The direct visit and the crawler are both excluded: only the two real
	// external hosts rank.
	if len(referrers) != 2 || referrers[0].Value != "news.example.com" || referrers[0].Count != 2 {
		t.Fatalf("referrer rows = %+v, want news.example.com(2) then google.com(1)", referrers)
	}
	if referrers[1].Value != "google.com" || referrers[1].Count != 1 {
		t.Fatalf("referrer rows = %+v, want google.com(1) second", referrers)
	}
}

// A file written before the UTM columns existed gains them on open: the
// ALTERs in duckDBSchema are the migration, and a pre-existing row must keep
// its data with the new columns reading as empty.
func TestDuckDBReopenAddsUTMColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.duckdb")

	d, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDuckDB: %v", err)
	}
	old := duckEvent("dddddddd-1111-2222-3333-444444444444", uuid.NewString(), "reader-1", time.Now())
	if err := d.SinkEvents(ctx, []Event{old}, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	// Stand in for a pre-UTM file: the columns the ALTERs would add are gone.
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		for _, col := range []string{"utm_source", "utm_medium", "utm_campaign"} {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE events DROP COLUMN `+col); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("age the schema: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for _, col := range []string{"utm_source", "utm_medium", "utm_campaign"} {
		if got := duckCount(t, reopened, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'events' AND column_name = ?`, col); got != 1 {
			t.Fatalf("reopened file is missing events.%s", col)
		}
	}
	if got := duckCount(t, reopened, `SELECT count(*) FROM events WHERE utm_source = '' AND utm_campaign = ''`); got != 1 {
		t.Fatalf("the pre-existing row did not survive the column addition with empty tags")
	}
}
