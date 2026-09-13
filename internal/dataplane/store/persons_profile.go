package storage

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

// Person profile store (P3).
//
// $set / $set_once traits arrive inside each event's `properties` JSON and, until
// now, were only ever reconstructed ad hoc at query time for the two hard-coded
// traits email + name — every other custom trait was effectively write-only. This
// adds a first-class person profile: a persons table keyed by
// (project_id, distinct_id) — where distinct_id is the *identity-stitched
// canonical id*, so an anonymous session's traits fold into the same profile once
// it aliases to a logged-in id — holding the *merged* trait maps, maintained
// incrementally as events are durably stored and seeded once from history.
//
// Merge semantics mirror the PostHog model:
//   - $set     → last-write-wins  (newest event's value for a key wins)
//   - $set_once → first-write-wins (a key, once set, is never overwritten)
//
// The (project_id, distinct_id) primary key plus the transactional
// read-merge-write upsert (INSERT ... ON CONFLICT DO UPDATE, version = last_seen
// in ms) keeps exactly one row per person — there is no duplicate for a merge to
// collapse later. Each write carries the full merged profile, so that row is
// always the most complete one under the single-writer ingest model agentray
// runs today. (A future scale-out to concurrent ingest writers would need
// per-key CRDT columns; called out in the data-architecture doc.)

// personKey identifies one profile.
type personKey struct {
	projectID  string
	distinctID string
}

// personDelta is the trait change distilled from one batch for one person.
type personDelta struct {
	setProps  map[string]json.RawMessage // latest value per $set key in the batch
	onceProps map[string]json.RawMessage // earliest value per $set_once key in the batch
	setTS     map[string]time.Time       // timestamp backing each setProps entry
	onceTS    map[string]time.Time       // timestamp backing each onceProps entry
	email     string
	name      string
	emailTS   time.Time
	nameTS    time.Time
	firstSeen time.Time
	lastSeen  time.Time
}

type eventProps struct {
	Set     map[string]json.RawMessage `json:"$set"`
	SetOnce map[string]json.RawMessage `json:"$set_once"`
	Email   string                     `json:"email"`
	Name    string                     `json:"name"`
}

// extractPersonDeltas distills identity-bearing events into one delta per person,
// resolving within-batch ordering by timestamp ($set → latest, $set_once →
// earliest). Each event's raw distinct_id is mapped to its stitched canonical id
// via resolve (nil = identity), so anonymous and logged-in ids for the same person
// collapse into one delta — matching the canonical key the read path groups on. It
// is pure so the merge rules can be unit-tested without DuckDB.
func extractPersonDeltas(events []Event, resolve func(projectID, distinctID string) string) map[personKey]*personDelta {
	out := map[personKey]*personDelta{}
	for _, e := range events {
		if e.DistinctID == "" || e.ProjectID == "" {
			continue
		}
		var p eventProps
		if e.Properties != "" {
			_ = json.Unmarshal([]byte(e.Properties), &p)
		}
		emailTrait := firstNonEmpty(p.Email, stringTrait(p.Set, "email"))
		nameTrait := firstNonEmpty(p.Name, stringTrait(p.Set, "name"))
		if len(p.Set) == 0 && len(p.SetOnce) == 0 && emailTrait == "" && nameTrait == "" {
			continue
		}
		canonical := e.DistinctID
		if resolve != nil {
			canonical = resolve(e.ProjectID, e.DistinctID)
		}
		key := personKey{projectID: e.ProjectID, distinctID: canonical}
		d := out[key]
		if d == nil {
			d = &personDelta{
				setProps:  map[string]json.RawMessage{},
				onceProps: map[string]json.RawMessage{},
				setTS:     map[string]time.Time{},
				onceTS:    map[string]time.Time{},
				firstSeen: e.Timestamp,
				lastSeen:  e.Timestamp,
			}
			out[key] = d
		}
		if e.Timestamp.Before(d.firstSeen) {
			d.firstSeen = e.Timestamp
		}
		if e.Timestamp.After(d.lastSeen) {
			d.lastSeen = e.Timestamp
		}
		for k, v := range p.Set { // last-write-wins
			if prev, ok := d.setTS[k]; !ok || !e.Timestamp.Before(prev) {
				d.setProps[k] = v
				d.setTS[k] = e.Timestamp
			}
		}
		for k, v := range p.SetOnce { // first-write-wins
			if prev, ok := d.onceTS[k]; !ok || e.Timestamp.Before(prev) {
				d.onceProps[k] = v
				d.onceTS[k] = e.Timestamp
			}
		}
		if emailTrait != "" && (d.email == "" || !e.Timestamp.Before(d.emailTS)) {
			d.email, d.emailTS = emailTrait, e.Timestamp
		}
		if nameTrait != "" && (d.name == "" || !e.Timestamp.Before(d.nameTS)) {
			d.name, d.nameTS = nameTrait, e.Timestamp
		}
	}
	return out
}

// personRow is a fully-merged profile ready to write / return.
type personRow struct {
	SetProps  map[string]json.RawMessage
	OnceProps map[string]json.RawMessage
	Email     string
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// mergePersonDelta folds a batch delta onto the existing stored profile. $set
// keys from the batch overlay the stored ones, $set_once keys fill only where
// absent, and first_seen/last_seen widen. Pure — unit-tested.
//
// Last-write-wins for $set is guarded against out-of-order delivery: because
// the DuckDB transaction does a read-merge-write, a batch that redelivers (or
// arrives) after a newer batch for the same person has already been folded in
// must not stomp the fresher stored value with its own older one. Each incoming
// $set key is therefore applied only when it is new or at least as recent as
// the stored profile's high-water mark (last_seen), the best per-key freshness
// proxy available without storing a timestamp per trait. A brand-new key is
// always taken; a key already present is overwritten only by a value that is
// not provably older. email/name follow the same rule.
func mergePersonDelta(existing *personRow, d *personDelta) personRow {
	merged := personRow{
		SetProps:  map[string]json.RawMessage{},
		OnceProps: map[string]json.RawMessage{},
	}
	var watermark time.Time
	if existing != nil {
		for k, v := range existing.SetProps {
			merged.SetProps[k] = v
		}
		for k, v := range existing.OnceProps {
			merged.OnceProps[k] = v
		}
		merged.Email, merged.Name = existing.Email, existing.Name
		merged.FirstSeen, merged.LastSeen = existing.FirstSeen, existing.LastSeen
		watermark = existing.LastSeen
	}
	// setFresh reports whether a delta value backed by ts may overwrite an existing
	// key. Per-key ts falls back to the batch's newest event when unset (hand-built
	// deltas / traits carried by an event with no explicit per-key timestamp).
	fresh := func(ts time.Time) bool {
		if ts.IsZero() {
			ts = d.lastSeen
		}
		return watermark.IsZero() || !ts.Before(watermark)
	}
	for k, v := range d.setProps { // last-write-wins, guarded against stale overwrite
		if _, had := merged.SetProps[k]; !had || fresh(d.setTS[k]) {
			merged.SetProps[k] = v
		}
	}
	for k, v := range d.onceProps { // first-write-wins: only if absent
		if _, ok := merged.OnceProps[k]; !ok {
			merged.OnceProps[k] = v
		}
	}
	if d.email != "" && (merged.Email == "" || fresh(d.emailTS)) {
		merged.Email = d.email
	}
	if d.name != "" && (merged.Name == "" || fresh(d.nameTS)) {
		merged.Name = d.name
	}
	if merged.FirstSeen.IsZero() || (!d.firstSeen.IsZero() && d.firstSeen.Before(merged.FirstSeen)) {
		merged.FirstSeen = d.firstSeen
	}
	if d.lastSeen.After(merged.LastSeen) {
		merged.LastSeen = d.lastSeen
	}
	return merged
}

// PersonProfile returns the full merged trait map for one person, combining
// $set and $set_once ($set wins on key overlap, matching PostHog precedence).
func (s *Store) PersonProfile(ctx context.Context, projectID, distinctID string) (map[string]json.RawMessage, error) {
	// Profiles are stored under the canonical id; resolve the (possibly anonymous)
	// input id so a caller passing a pre-alias distinct_id still finds the profile.
	if r, err := s.identityResolver(ctx, projectID); err == nil {
		distinctID = r.canonicalID(distinctID)
	}
	if s.duck == nil {
		return map[string]json.RawMessage{}, nil
	}
	profiles, err := s.duck.PersonProfilesByKeys(ctx, projectID, []string{distinctID})
	if err != nil {
		return nil, err
	}
	p := profiles[distinctID]
	if p == nil {
		return map[string]json.RawMessage{}, nil
	}
	merged := map[string]json.RawMessage{}
	for k, v := range p.OnceProps {
		merged[k] = v
	}
	for k, v := range p.SetProps {
		merged[k] = v
	}
	return merged, nil
}

func stringTrait(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func marshalTraitMap(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseTraitMap(s string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if s == "" || s == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		log.Printf("persons: parse trait map: %v", err)
		return map[string]json.RawMessage{}
	}
	return out
}
