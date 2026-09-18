package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// pgSessionStore is the Postgres-backed agentcore.SessionStore: it persists a
// run's append-only log to agent_session_log so a crashed or compacted run can be
// reduced and resumed (agentcore's durability seam). It lives here, in the
// consumer, because it is the one place that may import both agentcore and
// storage (storage never imports agentcore) — mirroring storeTraceSink.
//
// The agentcore SessionEntry is marshalled whole into the row's JSON payload, so
// every typed field (the message, model, summary, compaction markers) round-trips
// without storage needing to understand any of them; kind/turn are also lifted
// into columns for cheap ordering and filtering. The sessionID the loop passes is
// the run id (the same id the trace sink keys on), so the durable log and the
// per-LLM-call trace attribute to the same run — or, for a spawned sub-agent, the
// derived "<runID>/<toolCallID>" child key, which still attributes (and cascades)
// to the root run via its UUID prefix.
type pgSessionStore struct {
	store *storage.Store
}

var (
	_ agentcore.SessionStore       = (*pgSessionStore)(nil)
	_ agentcore.SessionBatchStore  = (*pgSessionStore)(nil)
	_ agentcore.SessionWindowStore = (*pgSessionStore)(nil)
	_ agentcore.SessionLeaseStore  = (*pgSessionStore)(nil)
)

const (
	sessionLeaseTTL       = 30 * time.Second
	sessionLeaseRenew     = 10 * time.Second
	sessionLeasePoll      = 100 * time.Millisecond
	sessionLeaseDBTimeout = 5 * time.Second
)

type sessionLeaseContextKey struct{}

type sessionLeaseToken struct {
	sessionID string
	ownerID   string
	epoch     int64
	cancel    context.CancelCauseFunc
}

// NewSessionStore returns a SessionStore that writes durable run logs to Postgres.
func NewSessionStore(store *storage.Store) agentcore.SessionStore {
	return &pgSessionStore{store: store}
}

// rootRunID extracts the agent_runs UUID a session key hangs off. A run's own
// log is keyed by its run id verbatim; a sub-agent child session is keyed
// "<runID>/<toolCallID>" (agentcore's deterministic child-session ids), so the
// prefix before the first "/" is the root run for the FK/cascade.
func rootRunID(sessionID string) string {
	if i := strings.IndexByte(sessionID, '/'); i >= 0 {
		return sessionID[:i]
	}
	return sessionID
}

// Append persists one entry. The store assigns the per-session sequence number;
// the returned seq is discarded here because the loop never reads it back mid-run
// (resume reads the whole ordered log). Best-effort is the loop's contract — a
// durability write must never break a run — but we surface the error so a failing
// store is visible to the (best-effort) caller.
func (s *pgSessionStore) Append(ctx context.Context, sessionID string, entry agentcore.SessionEntry) error {
	return s.AppendBatch(ctx, sessionID, []agentcore.SessionEntry{entry})
}

// AppendBatch commits one agentcore save point in a database transaction. The
// storage layer serializes sequence assignment per session, so concurrent side
// records land entirely before or after this batch and cannot split it.
func (s *pgSessionStore) AppendBatch(ctx context.Context, sessionID string, entries []agentcore.SessionEntry) error {
	rows := make([]storage.AgentSessionEntry, 0, len(entries))
	for _, entry := range entries {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		rows = append(rows, storage.AgentSessionEntry{
			RunID:       rootRunID(sessionID),
			SessionKey:  sessionID,
			Kind:        string(entry.Kind),
			Turn:        entry.Turn,
			PayloadJSON: string(payload),
		})
	}
	var err error
	if token, ok := ctx.Value(sessionLeaseContextKey{}).(sessionLeaseToken); ok && token.sessionID == sessionID {
		_, err = s.store.AppendAgentSessionEntriesFenced(ctx, rows, token.ownerID, token.epoch)
		if errors.Is(err, storage.ErrAgentSessionLeaseLost) {
			lost := fmt.Errorf("%w: %v", agentcore.ErrSessionLeaseLost, err)
			if token.cancel != nil {
				token.cancel(lost)
			}
			return lost
		}
	} else {
		_, err = s.store.AppendAgentSessionEntries(ctx, rows)
	}
	return err
}

// AcquireSessionLease waits for exclusive ownership of a durable session and
// renews it until release. The lease token rides on the returned context so all
// agentcore appends are fenced at the database boundary. PostgreSQL's clock is
// authoritative, avoiding skew between server replicas.
func (s *pgSessionStore) AcquireSessionLease(ctx context.Context, sessionID string) (context.Context, func() error, error) {
	ownerID := uuid.NewString()
	var epoch int64
	for {
		claimed, ok, err := s.store.AcquireAgentSessionLease(ctx, sessionID, rootRunID(sessionID), ownerID, sessionLeaseTTL)
		if err != nil {
			return ctx, nil, err
		}
		if ok {
			epoch = claimed
			break
		}
		timer := time.NewTimer(sessionLeasePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx, nil, ctx.Err()
		case <-timer.C:
		}
	}

	leaseBaseCtx, cancel := context.WithCancelCause(ctx)
	token := sessionLeaseToken{sessionID: sessionID, ownerID: ownerID, epoch: epoch, cancel: cancel}
	leaseCtx := context.WithValue(leaseBaseCtx, sessionLeaseContextKey{}, token)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(sessionLeaseRenew)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(context.WithoutCancel(ctx), sessionLeaseDBTimeout)
				ok, err := s.store.RenewAgentSessionLease(renewCtx, sessionID, ownerID, epoch, sessionLeaseTTL)
				renewCancel()
				if err != nil || !ok {
					cancel(sessionLeaseRenewalError(err))
					return
				}
			}
		}
	}()

	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			close(stop)
			<-done
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), sessionLeaseDBTimeout)
			releaseErr = s.store.ReleaseAgentSessionLease(releaseCtx, sessionID, ownerID, epoch)
			releaseCancel()
			cancel(nil)
		})
		return releaseErr
	}
	return leaseCtx, release, nil
}

// sessionLeaseRenewalError preserves the public ownership sentinel while retaining the
// backend failure for diagnostics.
func sessionLeaseRenewalError(err error) error {
	if err == nil {
		return agentcore.ErrSessionLeaseLost
	}
	return fmt.Errorf("%w: renewing PostgreSQL lease: %v", agentcore.ErrSessionLeaseLost, err)
}

// Log returns the full ordered entry log for a run, mapping each stored row back
// to the agentcore SessionEntry by unmarshalling its payload and stamping the
// store-assigned Seq. A malformed payload degrades to an empty entry carrying
// only kind/turn/seq rather than failing the whole reduce, mirroring how the Lab
// trace fold tolerates a bad row.
func (s *pgSessionStore) Log(ctx context.Context, sessionID string) ([]agentcore.SessionEntry, error) {
	rows, err := s.store.AgentSessionLog(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.SessionEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionEntryFromRow(r))
	}
	return out, nil
}

// LogFrom returns the tail of a session's log (entries with Seq >= sinceSeq),
// mapped the same way Log maps the whole thing.
func (s *pgSessionStore) LogFrom(ctx context.Context, sessionID string, sinceSeq int) ([]agentcore.SessionEntry, error) {
	rows, err := s.store.AgentSessionLogFrom(ctx, sessionID, sinceSeq)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.SessionEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionEntryFromRow(r))
	}
	return out, nil
}

// CheckpointSeq answers agentcore's two windowing questions from the partial
// index over compaction/leaf_move rows, so the cost of deciding whether a
// window is safe does not itself scale with the log.
func (s *pgSessionStore) CheckpointSeq(ctx context.Context, sessionID string) (int, bool, error) {
	return s.store.AgentSessionCheckpoint(ctx, sessionID)
}

// sessionEntryFromRow reconstructs an agentcore SessionEntry from a stored row.
// Pure (no DB) so it is unit-testable: the payload carries the full entry; Seq is
// authoritative from the row. A bad payload yields an entry with just the row's
// kind/turn/seq.
func sessionEntryFromRow(r storage.AgentSessionEntry) agentcore.SessionEntry {
	var e agentcore.SessionEntry
	if err := json.Unmarshal([]byte(r.PayloadJSON), &e); err != nil {
		e = agentcore.SessionEntry{Kind: agentcore.SessionEntryKind(r.Kind), Turn: r.Turn}
	}
	e.Seq = r.Seq
	return e
}
