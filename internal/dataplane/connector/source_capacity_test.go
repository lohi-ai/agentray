package connector

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

type capacitySource struct {
	total         int
	validationErr error
}

func (s *capacitySource) Kind() string                                    { return "faketest" }
func (s *capacitySource) TestConnection(context.Context) error            { return nil }
func (s *capacitySource) DiscoverSchema(context.Context) ([]Table, error) { return nil, nil }
func (s *capacitySource) Close()                                          {}
func (s *capacitySource) ValidateSnapshotKey(context.Context, string, string) error {
	return s.validationErr
}
func (s *capacitySource) PullRows(_ context.Context, req PullRequest) (PullResult, error) {
	start := 0
	if req.CursorKey != "" {
		n, err := strconv.Atoi(req.CursorKey)
		if err != nil {
			return PullResult{}, err
		}
		start = n
	}
	if start >= s.total {
		return PullResult{}, nil
	}
	end := min(start+req.Limit, s.total)
	out := PullResult{HasMore: end < s.total}
	for i := start + 1; i <= end; i++ {
		key := strconv.Itoa(i)
		out.Rows = append(out.Rows, Row{Key: key, Data: map[string]any{"id": i}})
		out.NextCursorKey = key
	}
	return out, nil
}

type snapshotHarness struct {
	*fakeStore
	mu        sync.Mutex
	gen       SnapshotGeneration
	entries   []SnapshotManifestEntry
	pending   []SnapshotOutbox
	prepared  int
	published int
}

func (h *snapshotHarness) ClaimSnapshotGeneration(_ context.Context, job SyncJob, runID, owner string, epoch int64) (SnapshotGeneration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen.Generation == "" {
		h.gen = SnapshotGeneration{ProjectID: job.ProjectID, ConnectorID: job.ConnectorID, Table: job.Table, SyncID: job.SyncID, Generation: "00000000-0000-4000-8000-000000000099", GenerationSeq: 1, BindingDigest: job.SourceBinding.Digest("snapshot"), State: "capturing", CaptureStartedAt: time.Now().UTC()}
	}
	if h.gen.State == "yielded" {
		h.gen.State = "capturing"
	}
	h.gen.RunID = runID
	h.gen.Owner = owner
	h.gen.LeaseEpoch = epoch
	return h.gen, nil
}
func (h *snapshotHarness) PendingSnapshotOutbox(context.Context, SnapshotGeneration) ([]SnapshotOutbox, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]SnapshotOutbox(nil), h.pending...), nil
}
func (h *snapshotHarness) PrepareSnapshotEnvelope(_ context.Context, g SnapshotGeneration, env SnapshotEnvelope, lower, upper string) (SnapshotOutbox, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := MarshalSnapshotEnvelope(env)
	if err != nil {
		return SnapshotOutbox{}, err
	}
	item := SnapshotOutbox{ID: fmt.Sprintf("o-%d", h.prepared), Generation: g.Generation, BatchID: env.BatchID, BatchIndex: env.BatchIndex, Kind: env.Kind, Payload: raw, LowerKey: lower, UpperKey: upper, Rows: len(env.Rows)}
	h.prepared++
	h.pending = append(h.pending, item)
	return item, nil
}
func (h *snapshotHarness) SealSnapshotGeneration(_ context.Context, g SnapshotGeneration, env SnapshotEnvelope, runRows int) (SnapshotOutbox, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := MarshalSnapshotEnvelope(env)
	if err != nil {
		return SnapshotOutbox{}, err
	}
	item := SnapshotOutbox{ID: "complete", Generation: g.Generation, BatchID: "complete", BatchIndex: env.ExpectedBatches, Kind: env.Kind, Payload: raw}
	h.pending = append(h.pending, item)
	h.gen.State = "sealed"
	h.gen.CaptureFinishedAt = env.CaptureFinishedAt
	h.fakeStore.mu.Lock()
	if run := h.fakeStore.runs[g.RunID]; run != nil {
		run.Status = "succeeded"
		run.Rows = runRows
		now := time.Now()
		run.FinishedAt = &now
	}
	h.fakeStore.mu.Unlock()
	return item, nil
}
func (h *snapshotHarness) MarkSnapshotOutboxPublished(_ context.Context, _ SnapshotGeneration, item SnapshotOutbox) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.pending {
		if h.pending[i].ID == item.ID {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			break
		}
	}
	if item.Kind == SnapshotKindBatch {
		env, err := ParseSnapshotEnvelope(item.Payload)
		if err != nil {
			return err
		}
		h.entries = append(h.entries, SnapshotManifestEntry{Index: env.BatchIndex, BatchID: env.BatchID, PayloadSHA256: env.PayloadSHA256, RowCount: len(env.Rows)})
		h.gen.KeyPosition = item.UpperKey
		h.gen.NextBatchIndex = item.BatchIndex + 1
		h.gen.Rows += int64(item.Rows)
	}
	h.published++
	return nil
}
func (h *snapshotHarness) YieldSnapshotGeneration(context.Context, SnapshotGeneration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen.State = "yielded"
	return nil
}
func (h *snapshotHarness) SnapshotManifest(context.Context, string) ([]SnapshotManifestEntry, int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int64
	for _, e := range h.entries {
		n += int64(e.RowCount)
	}
	return append([]SnapshotManifestEntry(nil), h.entries...), n, nil
}
func (h *snapshotHarness) SnapshotGeneration(context.Context, string) (SnapshotGeneration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen, nil
}
func (h *snapshotHarness) FailSnapshotGeneration(context.Context, SnapshotGeneration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen.State = "failed"
	return nil
}
func (h *snapshotHarness) CancelSnapshotGeneration(context.Context, string, string, int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen.State != "sealed" {
		h.gen.State = "cancelled"
	}
	return nil
}
func (h *snapshotHarness) BuildSnapshotBatches(common SnapshotEnvelope, rows []LandedRow, start int64) ([]SnapshotEnvelope, error) {
	wire, err := SnapshotRows(rows)
	if err != nil {
		return nil, err
	}
	common.Kind = SnapshotKindBatch
	common.BatchIndex = start
	common.BatchID = fmt.Sprintf("batch-%06d", start)
	common.Rows = wire
	common.PayloadSHA256, err = SnapshotPayloadDigest(wire)
	return []SnapshotEnvelope{common}, err
}
func (h *snapshotHarness) PublishSnapshotEnvelope(context.Context, SnapshotEnvelope) error {
	return nil
}

func (h *snapshotHarness) generationState() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen.State, h.gen.Generation != ""
}

func snapshotCapacityJob() SyncJob {
	mode := "snapshot"
	binding := &SourceBinding{ProjectID: "p1", ConnectorID: "c1", Schema: "exports", Relation: "rows_v1", RelationKind: RelationKindView, Columns: []SourcePolicyColumn{{Name: "id", PGType: "integer"}}, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique}
	return SyncJob{ProjectID: "p1", ConnectorID: "c1", Kind: "faketest", Table: "exports.rows_v1", KeyColumn: "id", SyncMode: mode, SourcePolicy: &SourcePolicy{Version: 1}, SourceBinding: binding}
}

func TestSourceCapacitySnapshotResume250001(t *testing.T) {
	testSourceCapacitySnapshot(t, 250001, 2)
}

func TestRepairG4TransientValidationPreservesGeneration(t *testing.T) {
	source := &capacitySource{validationErr: context.DeadlineExceeded}
	useFakeSource(source, nil)
	h := &snapshotHarness{fakeStore: newFakeStore(snapshotCapacityJob())}
	e := NewEngine(h, h)
	runSync(t, e, h.fakeStore, "snapshot-validation-interrupted")
	state, exists := h.generationState()
	if !exists {
		t.Fatal("snapshot generation was not claimed")
	}
	if state == "failed" {
		t.Fatal("transient validation interruption terminalized generation")
	}
}

func TestSourceCapacitySnapshot1000001(t *testing.T) {
	testSourceCapacitySnapshot(t, 1000001, 6)
}

func testSourceCapacitySnapshot(t *testing.T, total, runs int) {
	t.Helper()
	useFakeSource(&capacitySource{total: total}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &snapshotHarness{fakeStore: base}
	engine := NewEngine(h, h)
	for i := 0; i < runs; i++ {
		runSync(t, engine, base, "s1")
		if i < runs-1 && h.gen.State != "yielded" {
			t.Fatalf("slice %d state=%s, want yielded", i+1, h.gen.State)
		}
	}
	if h.gen.State != "sealed" || h.gen.Rows != int64(total) {
		t.Fatalf("completed state=%s rows=%d", h.gen.State, h.gen.Rows)
	}
	expectedEntries := (total + 999) / 1000
	if len(h.entries) != expectedEntries || len(h.pending) != 0 {
		t.Fatalf("entries=%d pending=%d", len(h.entries), len(h.pending))
	}
	manifest, err := SnapshotManifestDigest(h.entries)
	if err != nil || manifest == "" {
		t.Fatalf("manifest=%q err=%v", manifest, err)
	}
}

func TestSnapshotShutdownPreservesResumableGeneration(t *testing.T) {
	useFakeSource(&fakeSource{blockCh: make(chan struct{})}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &snapshotHarness{fakeStore: base}
	engine := NewEngine(h, h)
	if _, enqueued, err := engine.EnqueueRun(context.Background(), "p1", "s1", ""); err != nil || !enqueued {
		t.Fatalf("enqueue=%v err=%v", enqueued, err)
	}
	waitForSnapshotGeneration(t, h)
	engine.Shutdown()
	if state, _ := h.generationState(); state != "capturing" {
		t.Fatalf("shutdown generation state=%s, want resumable capturing", state)
	}
}

func TestSnapshotExplicitCancelIsTerminal(t *testing.T) {
	useFakeSource(&fakeSource{blockCh: make(chan struct{})}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &snapshotHarness{fakeStore: base}
	engine := NewEngine(h, h)
	run, enqueued, err := engine.EnqueueRun(context.Background(), "p1", "s1", "")
	if err != nil || !enqueued {
		t.Fatalf("enqueue=%v err=%v", enqueued, err)
	}
	waitForSnapshotGeneration(t, h)
	base.requestCancel(run.ID)
	engine.CancelRun(run.ID)
	engine.Wait()
	if state, _ := h.generationState(); state != "cancelled" {
		t.Fatalf("cancelled generation state=%s", state)
	}
}

type completionAckLostPublisher struct{ *snapshotHarness }

func (p completionAckLostPublisher) PublishSnapshotEnvelope(_ context.Context, env SnapshotEnvelope) error {
	if env.Kind == SnapshotKindComplete {
		return fmt.Errorf("completion acknowledgement lost")
	}
	return nil
}

func TestSnapshotCompletionAckLossDoesNotFailSealedRun(t *testing.T) {
	useFakeSource(&capacitySource{total: 1}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &snapshotHarness{fakeStore: base}
	runSync(t, NewEngine(h, completionAckLostPublisher{h}), base, "s1")
	if h.gen.State != "sealed" {
		t.Fatalf("generation state=%s, want sealed", h.gen.State)
	}
	if got := base.runStatus("run-1"); got != "succeeded" {
		t.Fatalf("originating run=%s, want succeeded despite lost completion ack", got)
	}
	if len(h.pending) != 1 || h.pending[0].Kind != SnapshotKindComplete {
		t.Fatalf("completion was not left for independent drain: %+v", h.pending)
	}
}

type terminalHeartbeatSnapshotHarness struct {
	*snapshotHarness
	heartbeatObserved chan struct{}
	heartbeatOnce     sync.Once
}

func (h *terminalHeartbeatSnapshotHarness) HeartbeatConnectorRun(ctx context.Context, runID string) (bool, bool, error) {
	cancelRequested, stillRunning, err := h.fakeStore.HeartbeatConnectorRun(ctx, runID)
	if err == nil && !stillRunning {
		h.heartbeatOnce.Do(func() { close(h.heartbeatObserved) })
	}
	return cancelRequested, stillRunning, err
}

func (h *terminalHeartbeatSnapshotHarness) PublishSnapshotEnvelope(ctx context.Context, env SnapshotEnvelope) error {
	if env.Kind != SnapshotKindComplete {
		return nil
	}
	select {
	case <-h.heartbeatObserved:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("terminal heartbeat was not observed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(25 * time.Millisecond):
		return nil
	}
}

func TestSnapshotCompletionDrainSurvivesTerminalRunHeartbeat(t *testing.T) {
	useFakeSource(&capacitySource{total: 1}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &terminalHeartbeatSnapshotHarness{
		snapshotHarness:   &snapshotHarness{fakeStore: base},
		heartbeatObserved: make(chan struct{}),
	}
	engine := NewEngine(h, h)
	engine.heartbeatEvery = time.Millisecond
	runSync(t, engine, base, "s1")
	if h.gen.State != "sealed" {
		t.Fatalf("generation state=%s, want sealed", h.gen.State)
	}
	if got := base.runStatus("run-1"); got != "succeeded" {
		t.Fatalf("originating run=%s, want succeeded", got)
	}
	if len(h.pending) != 0 {
		t.Fatalf("sealed completion was not drained after terminal heartbeat: %+v", h.pending)
	}
}

func TestSnapshotEmptyBatchWithHasMoreFailsGeneration(t *testing.T) {
	useFakeSource(&fakeSource{batches: []PullResult{{HasMore: true}}}, nil)
	base := newFakeStore(snapshotCapacityJob())
	h := &snapshotHarness{fakeStore: base}
	runSync(t, NewEngine(h, h), base, "s1")
	if state, _ := h.generationState(); state != "failed" {
		t.Fatalf("generation state=%s, want failed", state)
	}
}

func waitForSnapshotGeneration(t *testing.T, h *snapshotHarness) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.generationState(); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("snapshot generation was not claimed")
}

func BenchmarkSnapshotSource(b *testing.B) {
	rows := make([]LandedRow, 1000)
	for i := range rows {
		rows[i] = LandedRow{Key: strconv.Itoa(i + 1), DataJSON: `{"id":1,"value":"bounded"}`}
	}
	b.ReportAllocs()
	for range b.N {
		wire, err := SnapshotRows(rows)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := SnapshotPayloadDigest(wire); err != nil {
			b.Fatal(err)
		}
	}
}
