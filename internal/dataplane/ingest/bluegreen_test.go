package ingestion

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Blue-green coherence, proven end to end against a real JetStream broker and
// two real DuckDB files — one per colour. This is the evidence the ticket asks
// for: capture → query parity across a colour switch, for events AND connector
// rows, plus the negative case where the gate must refuse.
//
// The broker runs in-process (nats-server is a test dependency), so the
// experiment needs no Docker daemon and no broker binary on the machine.

func startBroker(t *testing.T) string {
	t.Helper()
	return startBrokerWith(t, nil)
}

// startBrokerWith starts an in-process broker, optionally shaped by tune — used
// to shrink the payload limit so a test can reach the oversize guard without
// allocating megabytes.
func startBrokerWith(t *testing.T, tune func(*natsserver.Options)) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	if tune != nil {
		tune(opts)
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(15 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

func testConfig(durable string) config.Config {
	return config.Config{
		IngestSubject:          "agentray.events.ingest.test",
		IngestConnectorSubject: "agentray.events.ingest.test.connectors",
		IngestStreamName:       "AGENTRAY_EVENTS_TEST",
		IngestDLQSubject:       "agentray.events.dlq.test",
		IngestMaxDeliver:       3,
		IngestDurable:          durable,
	}
}

// colour is one side of the blue-green pair: its own durable consumer and its
// own DuckDB file, exactly as the two compose services have.
type colour struct {
	name string
	ss   *StreamSet
	duck *storage.DuckDB
	nc   *nats.Conn

	worker *EventWorker
}

func newColour(t *testing.T, url string, cfg config.Config) *colour {
	t.Helper()
	ctx := context.Background()
	nc, err := nats.Connect(url, nats.Name("test-"+cfg.IngestDurable))
	if err != nil {
		t.Fatalf("connect %s: %v", cfg.IngestDurable, err)
	}
	ss, err := EnsureStreams(ctx, nc, cfg)
	if err != nil {
		t.Fatalf("ensure streams for %s: %v", cfg.IngestDurable, err)
	}
	duck, err := storage.OpenDuckDB(ctx, filepath.Join(t.TempDir(), "agentray.duckdb"))
	if err != nil {
		t.Fatalf("open DuckDB for %s: %v", cfg.IngestDurable, err)
	}
	c := &colour{name: cfg.IngestDurable, ss: ss, duck: duck, nc: nc}
	t.Cleanup(func() {
		c.park()
		_ = duck.Close()
		nc.Close()
	})
	return c
}

// serve starts this colour's ingest worker: it consumes both subjects and
// applies each message to this colour's DuckDB.
func (c *colour) serve(t *testing.T) {
	t.Helper()
	if c.worker != nil {
		return
	}
	worker, err := StartJetStreamWorker(context.Background(), c.ss, c.duck, nil)
	if err != nil {
		t.Fatalf("start worker for %s: %v", c.name, err)
	}
	c.worker = worker
}

// park stops the worker but keeps the process's state — the deploy's "old
// colour" during the switch window.
func (c *colour) park() {
	if c.worker != nil {
		_ = c.worker.Stop()
		c.worker = nil
	}
}

func (c *colour) status(t *testing.T) ReplayVerdict {
	t.Helper()
	v, err := c.ss.ReplayStatus(context.Background())
	if err != nil {
		t.Fatalf("replay status for %s: %v", c.name, err)
	}
	return v
}

// waitReady polls the same predicate /readyz serves until the colour is caught
// up, which is when a deploy would be allowed to switch to it.
func (c *colour) waitReady(t *testing.T, within time.Duration) ReplayVerdict {
	t.Helper()
	deadline := time.Now().Add(within)
	var last ReplayVerdict
	for time.Now().Before(deadline) {
		v, err := c.ss.ReplayStatus(context.Background())
		if err == nil {
			last = v
			if v.Ready {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became ready: %+v", c.name, last)
	return ReplayVerdict{}
}

// externalRowKeys reads the landed connector rows back out of one colour's file
// — the query a dataset preview or run_sql would make.
func (c *colour) externalRowKeys(t *testing.T, table string) []string {
	t.Helper()
	return duckRowKeys(t, c.duck, table)
}

func (c *colour) eventIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	err := c.duck.Read(context.Background(), func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(),
			`SELECT event_id::text FROM events ORDER BY event_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read events from %s: %v", c.name, err)
	}
	return ids
}

const (
	parityProject   = "11111111-1111-1111-1111-111111111111"
	parityConnector = "22222222-2222-2222-2222-222222222222"
	parityTable     = "users"
)

func parityEvent(seq int) storage.Event {
	return storage.Event{
		ProjectID:  parityProject,
		EventID:    uuidAt(seq),
		DistinctID: "reader",
		EventName:  "page_view",
		Properties: "{}",
		Timestamp:  time.Now().UTC(),
	}
}

// uuidAt builds a distinct, parseable event UUID per sequence.
func uuidAt(seq int) string {
	return "33333333-3333-3333-3333-" + pad12(seq)
}

func pad12(n int) string {
	const digits = "0123456789"
	out := []byte("000000000000")
	for i := 11; i >= 0 && n > 0; i-- {
		out[i] = digits[n%10]
		n /= 10
	}
	return string(out)
}

func parityRows(keys ...string) []connector.LandedRow {
	rows := make([]connector.LandedRow, 0, len(keys))
	for _, key := range keys {
		data, _ := json.Marshal(map[string]any{"id": key, "email": key + "@example.com"})
		rows = append(rows, connector.LandedRow{Key: key, Cursor: key, DataJSON: string(data)})
	}
	return rows
}

// TestBlueGreenConnectorParity is acceptance 1 and 2: connector rows and events
// acknowledged before a colour switch are all present on the colour that takes
// over, and both colours converge on the same rows after the next switch.
func TestBlueGreenConnectorParity(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	blue := newColour(t, url, testConfig("colour-blue"))
	blue.serve(t)
	// The serving colour runs the connector engine, whose publisher this is.
	queue := NewJetStreamQueue(blue.ss.JS, blue.ss.Subject, blue.ss.ConnectorSubject)

	// Phase A — blue serving: a sync lands rows and a capture lands events.
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k1", "k2", "k3")); err != nil {
		t.Fatalf("publish batch A: %v", err)
	}
	if err := queue.InsertEvents(ctx, []storage.Event{parityEvent(1), parityEvent(2)}); err != nil {
		t.Fatalf("publish events A: %v", err)
	}
	blue.waitReady(t, 20*time.Second)
	if got := blue.externalRowKeys(t, parityTable); len(got) != 3 {
		t.Fatalf("blue rows after batch A = %v, want 3 rows", got)
	}

	// Phase B — blue is parked (the switch window) while rows are still
	// accepted: a batch that replaces one row and adds another, plus an event.
	blue.park()
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k3", "k4")); err != nil {
		t.Fatalf("publish batch B: %v", err)
	}
	if err := queue.InsertEvents(ctx, []storage.Event{parityEvent(3)}); err != nil {
		t.Fatalf("publish events B: %v", err)
	}

	// The parked colour is behind, and the gate says so — this is the state that
	// used to be served behind a static /healthz 200.
	if v := blue.status(t); v.Ready {
		t.Fatalf("parked blue reports ready: %+v", v)
	}
	if got := blue.externalRowKeys(t, parityTable); len(got) != 3 {
		t.Fatalf("parked blue rows = %v, want the 3 rows it applied before parking", got)
	}

	// Phase C — green is started as the incoming colour: a fresh DuckDB file and
	// a durable that has never consumed, so it must replay the retained window.
	green := newColour(t, url, testConfig("colour-green"))
	if _, err := green.ss.ReplayStatus(ctx); err == nil {
		t.Fatal("green reports a verdict before its consumer exists; the gate must not be satisfiable from nothing")
	}
	green.serve(t)
	if v := green.waitReady(t, 20*time.Second); v.Reason != ReplayCaughtUp {
		t.Fatalf("green verdict = %+v, want caught-up", v)
	}

	wantKeys := []string{"k1", "k2", "k3", "k4"}
	gotKeys := green.externalRowKeys(t, parityTable)
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("green rows after the switch = %v, want %v", gotKeys, wantKeys)
	}
	for i, want := range wantKeys {
		if gotKeys[i] != want {
			t.Fatalf("green rows after the switch = %v, want %v", gotKeys, wantKeys)
		}
	}
	if got := len(green.eventIDs(t)); got != 3 {
		t.Fatalf("green events after the switch = %d, want 3", got)
	}

	// Phase D — the next deploy turns blue into the incoming colour; it resumes
	// from its own durable and must converge on exactly what green has.
	blue.serve(t)
	blue.waitReady(t, 20*time.Second)
	if blueKeys := blue.externalRowKeys(t, parityTable); len(blueKeys) != len(wantKeys) {
		t.Fatalf("blue rows after its replay = %v, want %v", blueKeys, wantKeys)
	}
	if blueEvents, greenEvents := blue.eventIDs(t), green.eventIDs(t); len(blueEvents) != len(greenEvents) {
		t.Fatalf("colours disagree: blue has %d events, green has %d", len(blueEvents), len(greenEvents))
	}
}

// TestBlueGreenRetentionGapRefusesReady is acceptance 4, in the shape the ticket
// describes: a colour parked longer than the stream's retention window. Its
// undelivered rows age out; when it next boots, the boot sample sees a purge
// frontier above its applied mark and the gate refuses — and keeps refusing even
// after the colour has applied everything that is still retained.
func TestBlueGreenRetentionGapRefusesReady(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	// A stream with a window short enough to age out inside the test. Production
	// is 30 days (28-day older than this test's three seconds); the behaviour
	// under test is the eviction, not the number.
	subjects := []string{"agentray.events.ingest.retention", "agentray.events.ingest.retention.connectors"}
	st, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "AGENTRAY_EVENTS_RETENTION",
		Subjects:  subjects,
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	// The marker lives beside this colour's DuckDB file, as it does in
	// production (EnsureStreams derives it from DUCKDB_PATH), so every instance
	// the deploy starts sees the same one.
	marker := filepath.Join(t.TempDir(), "blue.duckdb.ingest-loss")
	newStreamSet := func() *StreamSet {
		return &StreamSet{
			JS: js, Ingest: st,
			Subject:          subjects[0],
			ConnectorSubject: subjects[1],
			DLQSubj:          "agentray.events.dlq.retention",
			MaxDeliv:         3,
			Durable:          "colour-blue",
			LossMarkerPath:   marker,
		}
	}
	queue := NewJetStreamQueue(js, subjects[0], subjects[1])
	duckPath := filepath.Join(t.TempDir(), "blue.duckdb")
	duck, err := storage.OpenDuckDB(ctx, duckPath)
	if err != nil {
		t.Fatalf("open DuckDB: %v", err)
	}
	defer duck.Close()

	// The colour serves, applies three rows, then is parked for the deploy.
	serving := newStreamSet()
	worker, err := StartJetStreamWorker(ctx, serving, duck, nil)
	if err != nil {
		t.Fatalf("start serving worker: %v", err)
	}
	for i := range 3 {
		if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("kA"+string(rune('0'+i)))); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	waitVerdict(t, serving, 20*time.Second, func(v ReplayVerdict) bool { return v.Ready })
	if err := worker.Stop(); err != nil {
		t.Fatalf("stop serving worker: %v", err)
	}

	// Rows land while it is parked, then the window closes over them.
	for i := range 3 {
		if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("kB"+string(rune('0'+i)))); err != nil {
			t.Fatalf("publish while parked: %v", err)
		}
	}
	time.Sleep(4 * time.Second)

	// It boots again as the incoming colour: same durable, same DuckDB file.
	restarted := newStreamSet()
	worker, err = StartJetStreamWorker(ctx, restarted, duck, nil)
	if err != nil {
		t.Fatalf("restart worker: %v", err)
	}
	defer func() { _ = worker.Stop() }()

	v, err := restarted.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	if v.Ready || v.Reason != ReplayPurgedGap || v.Missing == 0 {
		t.Fatalf("verdict = %+v, want a purged-gap refusal with a non-zero missing count", v)
	}

	// Apply everything that is still retained: the refusal must survive it, which
	// is exactly why the boot sample is latched rather than recomputed.
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("kZ")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitVerdict(t, restarted, 20*time.Second, func(v ReplayVerdict) bool {
		return v.Pending == 0 && v.AckPending == 0
	})
	after := mustVerdict(t, restarted)
	if after.Ready || after.Reason != ReplayPurgedGap {
		t.Fatalf("verdict = %+v, want the refusal to hold after the replay drained", after)
	}
	if got := duckRowKeys(t, duck, parityTable); len(got) != 4 {
		t.Fatalf("rows = %v, want the 3 it applied plus the one retained message", got)
	}

	// And a RESTART must not forget. This is what the operator does next after a
	// refusal, and by now the live signal is gone for good: the colour has applied
	// everything the stream still holds, so its applied mark covers the retained
	// window and the broker reports nothing wrong. The loss is only knowable from
	// what the previous process wrote down.
	if err := worker.Stop(); err != nil {
		t.Fatalf("stop worker: %v", err)
	}
	again := newStreamSet()
	worker, err = StartJetStreamWorker(ctx, again, duck, nil)
	if err != nil {
		t.Fatalf("restart worker: %v", err)
	}
	if v := mustVerdict(t, again); v.Ready || v.Reason != ReplayPurgedGap || v.Missing == 0 {
		t.Fatalf("verdict after a restart = %+v, want the recorded loss to keep refusing", v)
	}
}

// TestBlueGreenStreamMismatchRefusesReady is the hazard the shipped topology can
// produce and the one a green healthcheck hides best: EnsureStreams rewrites the
// shared stream's subject list to its own env's on every boot, so an env that
// booted earlier keeps a durable whose filter the stream no longer carries. It
// will never be offered another row — the counts can even read zero, and any
// backlog still retained under the old subjects makes them non-zero instead —
// and the pending proof alone would report a colour caught-up that is done
// receiving. The gate must refuse on the broker's own configuration instead.
func TestBlueGreenStreamMismatchRefusesReady(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	blue := newColour(t, url, testConfig("colour-blue"))
	blue.serve(t)
	if v := blue.waitReady(t, 20*time.Second); v.Reason != ReplayCaughtUp {
		t.Fatalf("verdict before the rewrite = %+v, want caught-up", v)
	}

	// The other environment boots against the same broker with the same
	// (default) stream name and its OWN subjects — the production sequence, run
	// through the real EnsureStreams rather than a hand-written subject update.
	other := testConfig("colour-other-env")
	other.IngestSubject = "agentray.events.ingest.other"
	other.IngestConnectorSubject = "agentray.events.ingest.other.connectors"
	otherNC, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect other env: %v", err)
	}
	defer otherNC.Close()
	if _, err := EnsureStreams(ctx, otherNC, other); err != nil {
		t.Fatalf("other env ensure streams: %v", err)
	}

	v, err := blue.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	if v.Ready || v.Reason != ReplayStreamMismatch {
		t.Fatalf("verdict = %+v, want a stream-mismatch refusal: the stream no longer carries this colour's subjects", v)
	}

	// It is a property of the broker's configuration, not a transient: a second
	// read must refuse identically.
	if again := mustVerdict(t, blue.ss); again.Ready || again.Reason != ReplayStreamMismatch {
		t.Fatalf("second verdict = %+v, want the refusal to hold", again)
	}
}

// TestFreshColourIsNotHeldToAnotherColoursGap keeps the exemption honest: a
// colour whose durable has never consumed has no history to lose, so it replays
// the retained window and serves.
func TestFreshColourIsNotHeldToAnotherColoursGap(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	blue := newColour(t, url, testConfig("colour-blue"))
	blue.serve(t)
	queue := NewJetStreamQueue(blue.ss.JS, blue.ss.Subject, blue.ss.ConnectorSubject)
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	blue.waitReady(t, 20*time.Second)
	blue.park()

	// Drop the backlog the way an explicit purge does, then land one row after it.
	if err := blue.ss.Ingest.Purge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k2")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fresh := newColour(t, url, testConfig("colour-fresh"))
	fresh.serve(t)
	if v := fresh.waitReady(t, 20*time.Second); v.Reason != ReplayCaughtUp {
		t.Fatalf("fresh colour verdict = %+v, want caught-up", v)
	}
	if got := fresh.externalRowKeys(t, parityTable); len(got) != 1 || got[0] != "k2" {
		t.Fatalf("fresh colour rows = %v, want only what the stream still holds", got)
	}
}

// TestBootLatchesNeverMaskALiveDiagnosis pins how far a latch may go. The latch
// exists for the moment the live reading goes ready — the loss it records is
// invisible once the replay catches up — so it may DOWNGRADE a ready verdict,
// never replace one that already refuses: while a colour is replaying, "wait" is
// the answer that tells the operator to do nothing destructive, and a wiring
// mismatch is the one whose remedy actually unblocks the deploy. With two
// latches set, the specific diagnosis outranks the vague one.
func TestBootLatchesNeverMaskALiveDiagnosis(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	green := newColour(t, url, testConfig("colour-green"))
	green.serve(t)
	if v := green.waitReady(t, 20*time.Second); v.Reason != ReplayCaughtUp {
		t.Fatalf("verdict before the latches = %+v, want caught-up", v)
	}

	// A boot sample the broker could not answer: whether this colour lost rows
	// could not be established, so it must not take traffic, and only a restart
	// re-samples.
	green.ss.bootUnverified.Store(true)
	if v := mustVerdict(t, green.ss); v.Ready || v.Reason != ReplayUnverified {
		t.Fatalf("verdict = %+v, want a %q refusal", v, ReplayUnverified)
	}

	// A latched gap is the more specific diagnosis, so it wins.
	green.ss.bootGap.Store(417)
	if v := mustVerdict(t, green.ss); v.Ready || v.Reason != ReplayPurgedGap || v.Missing < 417 {
		t.Fatalf("verdict = %+v, want %q naming at least the latched 417", v, ReplayPurgedGap)
	}

	// A live refusal survives both: publish with no worker consuming and the
	// honest answer is "replaying". Reporting the latch here would tell the
	// operator to restart — a remedy that destroys a deploy cycle and, for the
	// retry, re-rolls the same dice.
	green.ss.bootGap.Store(0)
	green.park()
	queue := NewJetStreamQueue(green.ss.JS, green.ss.Subject, green.ss.ConnectorSubject)
	if err := queue.InsertEvents(ctx, []storage.Event{parityEvent(1)}); err != nil {
		t.Fatalf("publish with no consumer running: %v", err)
	}
	v := waitVerdict(t, green.ss, 20*time.Second, func(v ReplayVerdict) bool { return !v.Ready })
	if v.Reason != ReplayBehind {
		t.Fatalf("verdict = %+v, want %q rather than the latched %q", v, ReplayBehind, ReplayUnverified)
	}
}

// TestFreshColourOverAnEmptiedStreamStaysRefused is the boundary of the fresh
// colour's exemption, and the reason it has to be latched rather than derived:
// nothing is retained, so there is nothing to replay and nothing to catch up to,
// and a colour that has applied nothing would serve an empty file beside a
// sibling holding the history. The live predicate cannot hold that line once the
// first new message is applied — the mark then covers the whole retained window,
// which reads exactly like a caught-up colour — so the boot sample latches it,
// and the refusal must survive the rows that arrive afterwards.
func TestFreshColourOverAnEmptiedStreamStaysRefused(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	blue := newColour(t, url, testConfig("colour-blue"))
	blue.serve(t)
	queue := NewJetStreamQueue(blue.ss.JS, blue.ss.Subject, blue.ss.ConnectorSubject)
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k1", "k2")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	blue.waitReady(t, 20*time.Second)

	// Everything the stream ever held is gone, so a colour whose durable has
	// applied nothing has no history at all.
	if err := blue.ss.Ingest.Purge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}

	fresh := newColour(t, url, testConfig("colour-fresh"))
	fresh.serve(t)
	v := mustVerdict(t, fresh.ss)
	if v.Ready || v.Reason != ReplayPurgedGap || v.Missing == 0 {
		t.Fatalf("verdict = %+v, want a %q refusal naming what is gone", v, ReplayPurgedGap)
	}

	// Rows arrive and the colour applies them: the refusal must hold, because
	// catching up on what the stream still holds cannot restore what it lost.
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k3")); err != nil {
		t.Fatalf("publish after the purge: %v", err)
	}
	waitVerdict(t, fresh.ss, 20*time.Second, func(v ReplayVerdict) bool { return v.AckPending == 0 && v.Pending == 0 })
	if after := mustVerdict(t, fresh.ss); after.Ready || after.Reason != ReplayPurgedGap {
		t.Fatalf("verdict after applying the retained row = %+v, want the refusal to hold", after)
	}
}

func mustVerdict(t *testing.T, ss *StreamSet) ReplayVerdict {
	t.Helper()
	v, err := ss.ReplayStatus(context.Background())
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	return v
}

func waitVerdict(t *testing.T, ss *StreamSet, within time.Duration, ok func(ReplayVerdict) bool) ReplayVerdict {
	t.Helper()
	deadline := time.Now().Add(within)
	var last ReplayVerdict
	for time.Now().Before(deadline) {
		v, err := ss.ReplayStatus(context.Background())
		if err == nil {
			last = v
			if ok(v) {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("verdict never satisfied the condition: %+v", last)
	return ReplayVerdict{}
}

func duckRowKeys(t *testing.T, duck *storage.DuckDB, table string) []string {
	t.Helper()
	var keys []string
	err := duck.Read(context.Background(), func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(),
			`SELECT row_key FROM external_rows WHERE table_name = ? ORDER BY row_key`, table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read external_rows: %v", err)
	}
	return keys
}

// TestConnectorDeadLetterCarriesOriginAndReleasesGate is acceptance 6, in two
// halves: a poison connector batch is dead-lettered with the subject that
// decodes it, and a dead-lettered message does not hold the consumer's ack floor
// below the stream head — otherwise one bad batch would wedge every deploy.
func TestConnectorDeadLetterCarriesOriginAndReleasesGate(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()
	cfg := testConfig("colour-blue")
	cfg.IngestMaxDeliver = 1 // dead-letter on the first failed apply

	blue := newColour(t, url, cfg)
	blue.serve(t)
	queue := NewJetStreamQueue(blue.ss.JS, blue.ss.Subject, blue.ss.ConnectorSubject)

	// An unparseable project id can never insert: uuid.Parse fails in
	// DuckDB.InsertExternalRows on every attempt.
	if err := queue.PublishExternalRows(ctx, "not-a-uuid", parityConnector, parityTable, parityRows("poison")); err != nil {
		t.Fatalf("publish poison batch: %v", err)
	}
	// A good batch behind it must still land.
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k1")); err != nil {
		t.Fatalf("publish good batch: %v", err)
	}

	blue.waitReady(t, 20*time.Second)
	if got := blue.externalRowKeys(t, parityTable); len(got) != 1 || got[0] != "k1" {
		t.Fatalf("blue rows = %v, want just the good batch's row", got)
	}

	raw, err := blue.ss.DLQ.GetMsg(ctx, 1)
	if err != nil {
		t.Fatalf("read dead-letter: %v", err)
	}
	if origin := raw.Header.Get(OriginSubjectHeader); origin != cfg.IngestConnectorSubject {
		t.Fatalf("dead-letter origin = %q, want %q", origin, cfg.IngestConnectorSubject)
	}
	var batch ExternalRowsBatch
	if err := json.Unmarshal(raw.Data, &batch); err != nil {
		t.Fatalf("dead-letter body is not a connector batch: %v", err)
	}
	if batch.ProjectID != "not-a-uuid" {
		t.Fatalf("dead-letter body = %+v, want the poison batch", batch)
	}
}

// TestConnectorSettleNaksThenDeadLetters pins the failure policy the gate
// depends on: a failed apply is redelivered, and only once redeliveries are
// exhausted does the batch leave the stream into the DLQ.
func TestConnectorSettleNaksThenDeadLetters(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()
	cfg := testConfig("colour-settler")
	cfg.IngestMaxDeliver = 2

	blue := newColour(t, url, cfg)
	queue := NewJetStreamQueue(blue.ss.JS, blue.ss.Subject, blue.ss.ConnectorSubject)

	dlqed := make(chan []byte, 4)
	settler := externalRowsSettler{
		sink:       blue.duck,
		maxDeliver: cfg.IngestMaxDeliver,
		nakDelay:   25 * time.Millisecond,
		deadLetter: func(body []byte) error {
			dlqed <- body
			return nil
		},
	}

	cons, err := blue.ss.Ingest.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:        cfg.IngestDurable,
		AckPolicy:      jetstream.AckExplicitPolicy,
		FilterSubjects: []string{blue.ss.Subject, blue.ss.ConnectorSubject},
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	if err := queue.PublishExternalRows(ctx, "not-a-uuid", parityConnector, parityTable, parityRows("poison")); err != nil {
		t.Fatalf("publish poison batch: %v", err)
	}

	// The first delivery fails and is NAK'd; the second exhausts maxDeliver and
	// is dead-lettered.
	deliveries := 0
	deadline := time.Now().Add(20 * time.Second)
	for deliveries < 2 && time.Now().Before(deadline) {
		fetched, err := cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		for msg := range fetched.Messages() {
			deliveries++
			settler.settle(msg)
		}
	}
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want the batch redelivered exactly once", deliveries)
	}
	select {
	case <-dlqed:
	case <-time.After(5 * time.Second):
		t.Fatal("poison batch never reached the dead-letter sink")
	}
	// The point of dead-lettering rather than NAKing forever: the gate must not
	// stay refused because of a batch nobody can apply.
	verdict, err := blue.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	if !verdict.Ready {
		t.Fatalf("verdict after the dead-letter = %+v, want caught-up so the batch cannot wedge the gate", verdict)
	}
	if info, err := cons.Info(ctx); err != nil {
		t.Fatalf("consumer info: %v", err)
	} else if info.NumPending != 0 {
		t.Fatalf("consumer still has %d undelivered after the dead-letter", info.NumPending)
	}
}

// TestChunkRowsKeepsEveryRowInOrder is the boundary the wire format depends on:
// splitting a batch must not drop, duplicate or reorder a row, and the only
// chunk allowed to exceed the budget is one holding a single row that cannot be
// split further.
func TestChunkRowsKeepsEveryRowInOrder(t *testing.T) {
	const budget = 200
	rows := make([]connector.LandedRow, 0, 12)
	for i := range 10 {
		rows = append(rows, connector.LandedRow{
			Key:      fmt.Sprintf("k%02d", i),
			Cursor:   strconv.Itoa(i),
			DataJSON: fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("x", 40)),
		})
	}
	rows = append(rows, connector.LandedRow{Key: "oversize", Cursor: "10", DataJSON: `{"blob":"` + strings.Repeat("y", budget*2) + `"}`})
	rows = append(rows, connector.LandedRow{Key: "k11", Cursor: "11", DataJSON: `{"n":11}`})

	chunks := chunkRows(rows, budget)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want the batch split", len(chunks))
	}
	var got []string
	for _, chunk := range chunks {
		size := 0
		for _, r := range chunk {
			size += r.wireBytes()
		}
		if len(chunk) > 1 && size > budget {
			t.Fatalf("chunk of %d rows is %d bytes, over the %d budget", len(chunk), size, budget)
		}
		for _, r := range chunk {
			got = append(got, r.Key)
		}
	}
	want := make([]string, 0, len(rows))
	for _, r := range rows {
		want = append(want, r.Key)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows across chunks = %v, want %v", got, want)
	}
}

// TestConnectorBatchOverBrokerPayloadFailsSync is the guard for the one case the
// wire format cannot carry: a source row larger than a single message. The sync
// must fail naming the row — the cursor then holds, so both colours stay without
// it together instead of one colour quietly holding a row the other never got.
func TestConnectorBatchOverBrokerPayloadFailsSync(t *testing.T) {
	url := startBrokerWith(t, func(o *natsserver.Options) { o.MaxPayload = 8 << 10 })
	ctx := context.Background()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	cfg := testConfig("colour-payload")
	if _, err := EnsureStreams(ctx, nc, cfg); err != nil {
		t.Fatalf("ensure streams: %v", err)
	}
	q := NewJetStreamQueue(js, cfg.IngestSubject, cfg.IngestConnectorSubject)

	// A batch that fits still publishes: the guard must not cost normal syncs.
	fits := []connector.LandedRow{
		{Key: "k1", Cursor: "1", DataJSON: `{"n":1}`},
		{Key: "k2", Cursor: "2", DataJSON: `{"n":2}`},
	}
	if err := q.PublishExternalRows(ctx, "p", "c", parityTable, fits); err != nil {
		t.Fatalf("publish a fitting batch: %v", err)
	}

	over := append(append([]connector.LandedRow{}, fits...), connector.LandedRow{
		Key: "k-big", Cursor: "3", DataJSON: `{"blob":"` + strings.Repeat("x", 32<<10) + `"}`,
	})
	err = q.PublishExternalRows(ctx, "p", "c", parityTable, over)
	if err == nil {
		t.Fatal("an oversized row was published")
	}
	if !strings.Contains(err.Error(), "k-big") {
		t.Fatalf("error must name the row that cannot ride the stream: %v", err)
	}
}
