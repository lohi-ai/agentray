package storage

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// SeedDemoEvents (#3b) fills a project with ~2 days of synthetic product events so
// a stranger who runs `docker compose up` lands on non-empty Dashboards,
// Web-analytics, and Persons instead of the empty-state screens that make the
// tool look broken. It is opt-in (AGENTRAY_SEED_DEMO=true, wired only in the
// compose bootstrap) and idempotent: it no-ops if the project already has events,
// so a real deployment or a restart never gets polluted or double-seeded.
//
// The generated data is a small, believable SaaS funnel — visit → signup →
// activate → purchase with realistic drop-off — plus repeat sessions across the
// two days so retention and Persons render a curve rather than a flat line. It is
// deliberately modest (~a few hundred events) to keep first-boot fast.
func (s *Store) SeedDemoEvents(ctx context.Context, projectID string) error {
	// Idempotency: never seed a project that already has traffic.
	var existing uint64
	if err := s.ch.QueryRow(ctx,
		"SELECT count() FROM events WHERE project_id = ?", projectID).Scan(&existing); err != nil {
		return fmt.Errorf("seed demo: count events: %w", err)
	}
	if existing > 0 {
		return nil
	}
	events := buildDemoEvents(projectID, time.Now().UTC())
	if len(events) == 0 {
		return nil
	}
	if err := s.InsertEvents(ctx, events); err != nil {
		return fmt.Errorf("seed demo: insert events: %w", err)
	}
	return nil
}

// demoChannels mimics a real acquisition mix so the Web-analytics referrer
// breakdown is not a single bar.
var demoChannels = []struct {
	host    string
	channel string
}{
	{"google.com", "organic"},
	{"", "direct"},
	{"twitter.com", "social"},
	{"news.ycombinator.com", "referral"},
	{"google.com", "paid"},
}

// buildDemoEvents is the pure generator (no DB), so it is unit-testable: given a
// project and a "now", it returns a deterministic-shaped funnel of events over the
// preceding ~2 days. Randomness is seeded from the day so counts vary a little but
// the shape (visit > signup > activate > purchase) always holds.
func buildDemoEvents(projectID string, now time.Time) []Event {
	rng := rand.New(rand.NewSource(now.Truncate(24 * time.Hour).UnixNano()))
	var events []Event

	// ~40 new visitors per day for 2 days; a share convert down the funnel and a
	// share return the next day (retention signal).
	var day0Users []string
	for day := 0; day < 2; day++ {
		// day 0 is ~2 days ago, day 1 is ~1 day ago; every event lands strictly in
		// the past (the funnel offsets below add at most ~13h, well under 24h).
		dayStart := now.Add(time.Duration(-(2-day)) * 24 * time.Hour).Truncate(time.Hour)
		newVisitors := 40 + rng.Intn(15)
		for i := 0; i < newVisitors; i++ {
			user := fmt.Sprintf("demo-%d-%03d", day, i)
			ch := demoChannels[rng.Intn(len(demoChannels))]
			ts := dayStart.Add(time.Duration(rng.Intn(12)) * time.Hour).Add(time.Duration(rng.Intn(60)) * time.Minute)
			session := uuid.NewString()

			events = append(events, demoEvent(projectID, user, session, "pageview", "web", ts, ch.host, ch.channel,
				`{"$current_url":"/","path":"/"}`))

			// 55% sign up.
			if rng.Float64() > 0.45 {
				su := ts.Add(2 * time.Minute)
				events = append(events, demoEvent(projectID, user, session, "signup", "web", su, ch.host, ch.channel,
					`{"plan":"free"}`))
				// 60% of signups activate.
				if rng.Float64() > 0.40 {
					av := su.Add(5 * time.Minute)
					events = append(events, demoEvent(projectID, user, session, "activation", "web", av, ch.host, ch.channel,
						`{"first_action":"created_project"}`))
					// 25% of activated purchase.
					if rng.Float64() > 0.75 {
						pv := av.Add(20 * time.Minute)
						events = append(events, demoEvent(projectID, user, session, "purchase", "web", pv, ch.host, ch.channel,
							`{"plan":"pro","amount":29}`))
					}
					if day == 0 {
						day0Users = append(day0Users, user)
					}
				}
			}
		}
	}

	// Retention: ~40% of day-0 activated users return on day 1 (~1 day ago) for a
	// repeat action — the signal that renders a non-flat retention curve.
	day1Start := now.Add(-24 * time.Hour).Truncate(time.Hour)
	for _, user := range day0Users {
		if rng.Float64() > 0.60 {
			ts := day1Start.Add(time.Duration(rng.Intn(10)) * time.Hour)
			events = append(events, demoEvent(projectID, user, uuid.NewString(), "activation", "web", ts, "", "direct",
				`{"first_action":"returned"}`))
		}
	}
	return events
}

func demoEvent(projectID, user, session, name, typ string, ts time.Time, host, channel, props string) Event {
	return Event{
		ProjectID:       projectID,
		EventID:         uuid.NewString(),
		DistinctID:      user,
		SessionID:       session,
		EventName:       name,
		EventType:       typ,
		Properties:      props,
		Timestamp:       ts,
		VisitorClass:    "human",
		ReferrerHost:    host,
		ReferrerChannel: channel,
		UserAgent:       "Mozilla/5.0 (demo seed)",
	}
}
