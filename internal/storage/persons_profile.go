package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// Person profile store (P3).
//
// $set / $set_once traits arrive inside each event's `properties` JSON and, until
// now, were only ever reconstructed ad hoc at query time for the two hard-coded
// traits email + name — every other custom trait was effectively write-only. This
// adds a first-class person profile: a ReplacingMergeTree keyed by
// (project_id, distinct_id) — where distinct_id is the *identity-stitched
// canonical id*, so an anonymous session's traits fold into the same profile once
// it aliases to a logged-in id — holding the *merged* trait maps, maintained
// incrementally as events are durably stored and seeded once from history.
//
// Merge semantics mirror the PostHog model:
//   - $set     → last-write-wins  (newest event's value for a key wins)
//   - $set_once → first-write-wins (a key, once set, is never overwritten)
//
// ReplacingMergeTree(version) collapses duplicate (project_id, distinct_id) rows
// to the highest version (= last_seen in ms). Each write carries the full merged
// profile, so the surviving row is always the most complete one under the
// single-writer ingest model agentray runs today. (A future scale-out to
// concurrent ingest writers would need per-key CRDT columns; called out in the
// data-architecture doc.)

// migratePersons provisions the profile table and seeds it once from history.
func (s *Store) migratePersons(ctx context.Context) error {
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS persons (
	project_id UUID,
	distinct_id String,       -- stitched canonical id (see ApplyPersonUpdates)
	properties String,        -- merged $set traits (JSON object)
	properties_once String,   -- merged $set_once traits (JSON object)
	email String,
	name String,
	first_seen DateTime64(3, 'UTC'),
	last_seen DateTime64(3, 'UTC'),
	version UInt64
)
ENGINE = ReplacingMergeTree(version)
ORDER BY (project_id, distinct_id)`); err != nil {
		return err
	}
	return s.backfillPersons(ctx)
}

// backfillPersons seeds profiles from existing events exactly once. Unlike the
// live path it cannot key-merge arbitrary traits in pure SQL, so it takes the
// newest event's whole $set object and the oldest event's whole $set_once object
// per person — a reasonable seed that live updates refine key-by-key thereafter.
//
// The seed keys by raw distinct_id (it runs during migrate, before the alias
// dictionary is populated from Postgres, so it cannot stitch here). Non-aliased
// persons — the vast majority at seed time — have raw == canonical, so their seed
// is correct. An already-aliased person's seed row lands under its raw anon id and
// is simply never read (reads group by canonical id); its canonical profile is
// rebuilt by the first live event for that person, matching the best-effort
// "self-heals on next event" contract the live path already relies on.
func (s *Store) backfillPersons(ctx context.Context) error {
	const marker = "backfill_persons_v1"
	var applied uint64
	if err := s.ch.QueryRow(ctx, `SELECT count() FROM schema_markers WHERE name = ?`, marker).Scan(&applied); err != nil {
		return fmt.Errorf("check persons backfill marker: %w", err)
	}
	if applied > 0 {
		return nil
	}
	const emailExpr = `if(JSONExtractString(properties, 'email') != '', JSONExtractString(properties, 'email'), JSONExtractString(properties, '$set', 'email'))`
	const nameExpr = `if(JSONExtractString(properties, 'name') != '', JSONExtractString(properties, 'name'), JSONExtractString(properties, '$set', 'name'))`
	if err := s.ch.Exec(ctx, `
INSERT INTO persons (project_id, distinct_id, properties, properties_once, email, name, first_seen, last_seen, version)
SELECT
	project_id,
	distinct_id,
	if(set_raw = '', '{}', set_raw) AS properties,
	if(once_raw = '', '{}', once_raw) AS properties_once,
	email,
	name,
	first_seen,
	last_seen,
	toUInt64(toUnixTimestamp64Milli(last_seen)) AS version
FROM (
	SELECT
		project_id,
		distinct_id,
		argMaxIf(JSONExtractRaw(properties, '$set'), timestamp, JSONExtractRaw(properties, '$set') != '') AS set_raw,
		argMinIf(JSONExtractRaw(properties, '$set_once'), timestamp, JSONExtractRaw(properties, '$set_once') != '') AS once_raw,
		argMaxIf(`+emailExpr+`, timestamp, `+emailExpr+` != '') AS email,
		argMaxIf(`+nameExpr+`, timestamp, `+nameExpr+` != '') AS name,
		min(timestamp) AS first_seen,
		max(timestamp) AS last_seen
	FROM events
	WHERE distinct_id != ''
	GROUP BY project_id, distinct_id
)`); err != nil {
		return fmt.Errorf("backfill persons: %w", err)
	}
	if err := s.ch.Exec(ctx, `INSERT INTO schema_markers (name) VALUES (?)`, marker); err != nil {
		return fmt.Errorf("record persons backfill marker: %w", err)
	}
	return nil
}

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
// is pure so the merge rules can be unit-tested without ClickHouse.
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
// ApplyPersonUpdates does a read-merge-write, a batch that redelivers (or arrives)
// *after* a newer batch for the same person has already been folded in must not
// stomp the fresher stored value with its own older one. Each incoming $set key is
// therefore applied only when it is new (never seen) or at least as recent as the
// stored profile's high-water mark (last_seen) — the best per-key freshness proxy
// available without storing a timestamp per trait. A brand-new key is always taken
// (first value can't be stale); a key already present is overwritten only by a
// value that isn't provably older. email/name follow the same rule.
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

// startPersonApplier launches the single background goroutine that owns every
// write to the persons table. Keeping it to one consumer preserves the
// single-writer read-merge-write invariant mergePersonDelta depends on, while
// moving the work off the ingest ack path. Only Open calls it, once.
func (s *Store) startPersonApplier() {
	s.personUpdates = make(chan []Event, 512)
	s.personDone = make(chan struct{})
	s.personWG.Add(1)
	go func() {
		defer s.personWG.Done()
		apply := func(events []Event) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.ApplyPersonUpdates(ctx, events); err != nil {
				log.Printf("persons: apply updates (best-effort): %v", err)
			}
		}
		for {
			select {
			case events := <-s.personUpdates:
				apply(events)
			case <-s.personDone:
				// Drain what's queued so a graceful shutdown doesn't lose the profile
				// side of already-acked events, then exit.
				for {
					select {
					case events := <-s.personUpdates:
						apply(events)
					default:
						return
					}
				}
			}
		}
	}()
}

// stopPersonApplier signals the applier to drain and waits for it. Safe to call
// when the applier was never started (nil channel).
func (s *Store) stopPersonApplier() {
	if s.personDone == nil {
		return
	}
	close(s.personDone)
	s.personWG.Wait()
}

// enqueuePersonUpdates hands a durably-stored batch to the background applier.
// When the applier isn't running (a Store built outside Open — e.g. a unit test)
// it applies inline so behavior is unchanged. When the applier's buffer is full it
// drops the hand-off: profiles self-heal on the next event for the affected
// persons, which is preferable to blocking ingest under a profile-write backlog.
func (s *Store) enqueuePersonUpdates(events []Event) {
	if s.personUpdates == nil {
		if err := s.ApplyPersonUpdates(context.Background(), events); err != nil {
			log.Printf("persons: apply updates inline (best-effort): %v", err)
		}
		return
	}
	select {
	case s.personUpdates <- events:
	default:
		log.Printf("persons: applier saturated, skipping %d-event batch (self-heals)", len(events))
	}
}

// ApplyPersonUpdates maintains the profile store from a durably-stored batch. It
// is best-effort: callers invoke it only after events are safely in ClickHouse,
// and a failure here must not fail the batch (profiles self-heal on the next
// event for that person). Governance: this runs in the store (the CH edge), never
// in agent/tool code.
func (s *Store) ApplyPersonUpdates(ctx context.Context, events []Event) error {
	// Resolve raw distinct ids to their stitched canonical id so profiles key the
	// same way the read path groups (canonical_distinct_id). Resolvers are cached
	// per project; a lookup failure falls open to the zero resolver (identity), so
	// a transient alias-store hiccup degrades to raw-keyed rather than erroring.
	resolvers := map[string]identityResolver{}
	resolve := func(projectID, distinctID string) string {
		r, ok := resolvers[projectID]
		if !ok {
			r, _ = s.identityResolver(ctx, projectID)
			resolvers[projectID] = r
		}
		return r.canonicalID(distinctID)
	}
	deltas := extractPersonDeltas(events, resolve)
	if len(deltas) == 0 {
		return nil
	}
	byProject := map[string][]string{}
	for k := range deltas {
		byProject[k.projectID] = append(byProject[k.projectID], k.distinctID)
	}

	batch, err := s.ch.PrepareBatch(ctx, `
INSERT INTO persons (project_id, distinct_id, properties, properties_once, email, name, first_seen, last_seen, version)`)
	if err != nil {
		return err
	}
	for projectID, ids := range byProject {
		existing, err := s.personProfilesByKeys(ctx, projectID, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			d := deltas[personKey{projectID: projectID, distinctID: id}]
			merged := mergePersonDelta(existing[id], d)
			pid, err := uuid.Parse(projectID)
			if err != nil {
				return err
			}
			if err := batch.Append(
				pid,
				id,
				marshalTraitMap(merged.SetProps),
				marshalTraitMap(merged.OnceProps),
				merged.Email,
				merged.Name,
				merged.FirstSeen,
				merged.LastSeen,
				uint64(merged.LastSeen.UnixMilli()),
			); err != nil {
				return err
			}
		}
	}
	return batch.Send()
}

// personProfilesByKeys reads the current merged profiles for a set of distinct ids
// within one project, collapsing ReplacingMergeTree duplicates with FINAL.
func (s *Store) personProfilesByKeys(ctx context.Context, projectID string, distinctIDs []string) (map[string]*personRow, error) {
	out := map[string]*personRow{}
	if len(distinctIDs) == 0 {
		return out, nil
	}
	args := []any{projectID}
	for _, id := range distinctIDs {
		args = append(args, id)
	}
	rows, err := s.ch.Query(ctx, `
SELECT distinct_id, properties, properties_once, email, name, first_seen, last_seen
FROM persons FINAL
WHERE project_id = ? AND distinct_id IN (`+placeholders(len(distinctIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, props, once, email, name string
			firstSeen, lastSeen          time.Time
		)
		if err := rows.Scan(&id, &props, &once, &email, &name, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		out[id] = &personRow{
			SetProps:  parseTraitMap(props),
			OnceProps: parseTraitMap(once),
			Email:     email,
			Name:      name,
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
		}
	}
	return out, rows.Err()
}

// PersonProfile returns the full merged trait map for one person, combining
// $set and $set_once ($set wins on key overlap, matching PostHog precedence).
func (s *Store) PersonProfile(ctx context.Context, projectID, distinctID string) (map[string]json.RawMessage, error) {
	// Profiles are stored under the canonical id; resolve the (possibly anonymous)
	// input id so a caller passing a pre-alias distinct_id still finds the profile.
	if r, err := s.identityResolver(ctx, projectID); err == nil {
		distinctID = r.canonicalID(distinctID)
	}
	profiles, err := s.personProfilesByKeys(ctx, projectID, []string{distinctID})
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
