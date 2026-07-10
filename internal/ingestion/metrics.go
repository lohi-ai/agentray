package ingestion

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/storage"
)

// metricsSink is the narrow write surface the metrics emitter needs. storage.Store
// satisfies it; kept small so tests can substitute a fake.
type metricsSink interface {
	InsertEvents(ctx context.Context, events []storage.Event) error
}

// PipelineMetrics counts what the ingest pipeline does and periodically writes it
// back into ClickHouse as `system.pipeline.stats` events, so the *existing*
// alerting evaluator and dashboards observe the pipeline itself — no new alert
// machinery. Counters are cumulative; each emit reports the delta since the last
// emit (directly alertable: "insert_failures over the last minute > 0"). All
// record* methods are safe for concurrent callers and cheap (atomic adds), so the
// hot flush path is never blocked on metrics.
type PipelineMetrics struct {
	flushes        atomic.Int64
	eventsInserted atomic.Int64
	insertFailures atomic.Int64
	retries        atomic.Int64
	naks           atomic.Int64
	deadLetters    atomic.Int64
	lastLagMS      atomic.Int64

	sink      metricsSink
	projectID string
	interval  time.Duration

	prev counters
	done chan struct{}
}

type counters struct {
	flushes, eventsInserted, insertFailures, retries, naks, deadLetters int64
}

// NewPipelineMetrics returns a metrics collector. When projectID is empty the
// collector still counts (record* are no-op-safe) but Start does not emit — the
// caller has decided self-metrics are disabled.
func NewPipelineMetrics(sink metricsSink, projectID string, interval time.Duration) *PipelineMetrics {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &PipelineMetrics{sink: sink, projectID: projectID, interval: interval, done: make(chan struct{})}
}

func (m *PipelineMetrics) recordFlush(events, lagMS int) {
	if m == nil {
		return
	}
	m.flushes.Add(1)
	m.eventsInserted.Add(int64(events))
	m.lastLagMS.Store(int64(lagMS))
}

func (m *PipelineMetrics) recordInsertFailure() {
	if m == nil {
		return
	}
	m.insertFailures.Add(1)
}

func (m *PipelineMetrics) recordRetry() {
	if m != nil {
		m.retries.Add(1)
	}
}

func (m *PipelineMetrics) recordNak() {
	if m != nil {
		m.naks.Add(1)
	}
}

func (m *PipelineMetrics) recordDeadLetter() {
	if m != nil {
		m.deadLetters.Add(1)
	}
}

// Start launches the periodic emit loop. No-op (returns immediately) when the
// collector has no project to write to.
func (m *PipelineMetrics) Start() {
	if m == nil || m.projectID == "" || m.sink == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.emit()
			case <-m.done:
				return
			}
		}
	}()
}

// Stop halts the emit loop. Safe on a nil or never-Started collector.
func (m *PipelineMetrics) Stop() {
	if m == nil || m.projectID == "" {
		return
	}
	select {
	case <-m.done:
	default:
		close(m.done)
	}
}

func (m *PipelineMetrics) snapshot() counters {
	return counters{
		flushes:        m.flushes.Load(),
		eventsInserted: m.eventsInserted.Load(),
		insertFailures: m.insertFailures.Load(),
		retries:        m.retries.Load(),
		naks:           m.naks.Load(),
		deadLetters:    m.deadLetters.Load(),
	}
}

func (m *PipelineMetrics) emit() {
	cur := m.snapshot()
	delta := counters{
		flushes:        cur.flushes - m.prev.flushes,
		eventsInserted: cur.eventsInserted - m.prev.eventsInserted,
		insertFailures: cur.insertFailures - m.prev.insertFailures,
		retries:        cur.retries - m.prev.retries,
		naks:           cur.naks - m.prev.naks,
		deadLetters:    cur.deadLetters - m.prev.deadLetters,
	}
	m.prev = cur

	props := map[string]any{
		"flushes":          delta.flushes,
		"events_inserted":  delta.eventsInserted,
		"insert_failures":  delta.insertFailures,
		"retries":          delta.retries,
		"naks":             delta.naks,
		"dead_letters":     delta.deadLetters,
		"ingest_lag_ms":    m.lastLagMS.Load(),
		"interval_seconds": int(m.interval.Seconds()),
	}
	propsJSON, err := json.Marshal(props)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	ev := storage.Event{
		ProjectID:    m.projectID,
		EventID:      uuid.NewString(),
		EventName:    "system.pipeline.stats",
		EventType:    "system",
		DistinctID:   "pipeline",
		Properties:   string(propsJSON),
		Timestamp:    now,
		VisitorClass: "system",
	}
	// Best-effort: if this insert fails (e.g. ClickHouse is the very thing that is
	// down), skip it — real events are safely NAK'd for redelivery, and metrics
	// resume once ClickHouse recovers.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.sink.InsertEvents(ctx, []storage.Event{ev}); err != nil {
		log.Printf("ingestion metrics: emit pipeline stats: %v", err)
	}
}
