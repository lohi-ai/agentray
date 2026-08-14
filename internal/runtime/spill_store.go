package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// pgSpillStore is the Postgres-backed spill.SpillStore: it persists a tool
// result too large to sit inline in the model's context and serves bounded byte
// windows of it back through read_spill. It lives here, in the consumer, for the
// same reason pgSessionStore does — this is the one place that may import both
// agentcore and storage (storage never imports agentcore).
//
// Durability is the whole point. The loop replaces an oversized result with a
// preview plus a locator and writes that locator into the durable session log,
// so an in-process store would leave a resumed run holding a handle to bytes
// that died with the process. spill.MemorySpillStore stays what it says it is:
// the test and single-shot default.
type pgSpillStore struct {
	rows spillRows
	// timeout bounds a fence check, which the SpillOwner contract has to make
	// without a context of its own.
	timeout time.Duration
}

// spillRows is the storage surface this adapter needs, narrowed to three calls
// so the round-trip (locator minting, rune snapping, the fence) is unit-testable
// against a fake instead of only against a live database.
type spillRows interface {
	SaveAgentSpill(ctx context.Context, a storage.AgentSpillArtifact) (int, error)
	AgentSpillWindowAt(ctx context.Context, locator string, offset, limit int) (storage.AgentSpillWindow, error)
	OwnsAgentSpill(ctx context.Context, locator, sessionKey string) (bool, error)
}

// defaultSpillFenceTimeout bounds the ownership check. spill.SpillOwner is a
// pure predicate — no context, no error — so a hung database must fail the read
// rather than park the run on it. Failing closed is right: the alternative is
// serving bytes whose ownership was never established.
const defaultSpillFenceTimeout = 5 * time.Second

// defaultSpillReadBytes bounds a read that asked for no limit. It mirrors the
// read_spill tool's own cap, which is what a model actually goes through; this
// is the backstop for a direct caller.
const defaultSpillReadBytes = 8 * 1024

// NewSpillStore returns a SpillStore that persists oversized tool results to
// Postgres, keyed by the run's session.
func NewSpillStore(store *storage.Store) spill.SpillStore {
	return &pgSpillStore{rows: store, timeout: defaultSpillFenceTimeout}
}

// SaveText persists the full text and returns its locator.
//
// The locator is a digest that COVERS THE SESSION, so two sessions producing
// byte-identical output never collide onto one artifact and a locator carries no
// guessable structure. The row also records the session verbatim, which is what
// OwnsSpill checks — the digest makes a locator hard to guess, the column makes
// the fence true regardless.
func (s *pgSpillStore) SaveText(ctx context.Context, req spill.SpillRequest) (spill.SpillRecord, error) {
	locator := spillLocator(req)
	bytes, err := s.rows.SaveAgentSpill(ctx, storage.AgentSpillArtifact{
		RunID:      rootRunID(req.SessionID),
		SessionKey: req.SessionID,
		Locator:    locator,
		ToolName:   req.ToolName,
		CallID:     req.CallID,
		Label:      req.Label,
		Content:    req.Content,
	})
	if err != nil {
		// A spill failure must never turn a successful tool call into a failed
		// one: the plugin reads the error as "fall back to plain truncation".
		return spill.SpillRecord{}, err
	}
	return spill.SpillRecord{Locator: locator, Bytes: bytes}, nil
}

// ReadText returns a bounded, rune-aligned slice of a stored artifact. The byte
// window is cut in Postgres (see AgentSpillWindowAt), so paging a large artifact
// does not read the whole column per call.
//
// It over-reads by utf8.UTFMax-1 bytes so the window can be snapped back to a
// rune boundary and still carry every rune that STARTED inside the requested
// limit. Without the slack a limit landing mid-rune would return fewer bytes
// than asked for — and a small enough limit would return none at all, parking a
// paging model at the same offset forever.
func (s *pgSpillStore) ReadText(ctx context.Context, locator string, offset, limit int) (spill.SpillSlice, error) {
	if limit <= 0 {
		// An unbounded read would defeat the point: spilling exists to keep an
		// oversized result out of the context, so retrieving it must be bounded
		// too. The model pages a large artifact with successive offsets.
		limit = defaultSpillReadBytes
	}
	w, err := s.rows.AgentSpillWindowAt(ctx, locator, offset, limit+utf8.UTFMax-1)
	if err != nil {
		if errors.Is(err, storage.ErrAgentSpillNotFound) {
			return spill.SpillSlice{}, spill.ErrSpillNotFound
		}
		return spill.SpillSlice{}, err
	}
	return snapSpillWindow(w), nil
}

// OwnsSpill reports whether the locator was minted for this session. The loop
// calls it before every read, so the fence holds even for a locator that leaked
// into another run's transcript.
func (s *pgSpillStore) OwnsSpill(locator, sessionID string) bool {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultSpillFenceTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ok, err := s.rows.OwnsAgentSpill(ctx, locator, sessionID)
	return err == nil && ok
}

// spillLocator mints the opaque model-facing handle. It mirrors the digest
// agentcore's in-memory store uses (session + call + tool + name), so an
// artifact is addressed the same way whichever store holds it.
func spillLocator(req spill.SpillRequest) string {
	sum := sha256.Sum256([]byte(req.SessionID + "\x00" + req.CallID + "\x00" + req.ToolName + "\x00" + req.SuggestedName))
	return "spill_" + hex.EncodeToString(sum[:12])
}

// snapSpillWindow turns a raw byte window into a rune-aligned slice.
//
// The cut offsets come from the model, so both ends can land mid-rune. Both are
// trimmed INWARD — leading continuation bytes dropped, a trailing partial rune
// dropped — which keeps the returned text valid UTF-8 and keeps the reported
// offset honest, so a model paging with offset += len(content) never skips or
// re-reads a byte. EOF is computed from the window's true end, before trimming,
// so a partial rune at the artifact's very end (impossible for valid UTF-8, but
// cheap to be right about) does not claim there is more to read.
func snapSpillWindow(w storage.AgentSpillWindow) spill.SpillSlice {
	content := w.Content
	offset := w.Offset
	end := offset + len(content)

	// Front: skip continuation bytes so the slice starts on a rune.
	for len(content) > 0 && !utf8.RuneStart(content[0]) {
		content = content[1:]
		offset++
	}
	// Back: drop a rune the window cut in half. Only the last few bytes can be
	// partial, and DecodeLastRune reports that as RuneError with size 1.
	for len(content) > 0 {
		r, size := utf8.DecodeLastRune(content)
		if r != utf8.RuneError || size > 1 {
			break
		}
		content = content[:len(content)-1]
	}
	return spill.SpillSlice{
		Content: string(content),
		Offset:  offset,
		Total:   w.Total,
		EOF:     end >= w.Total,
	}
}
