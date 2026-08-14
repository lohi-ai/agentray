package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// fakeSpillRows stands in for Postgres so the round-trip — locator minting, the
// byte window, rune snapping, the session fence — is exercised in CI. It
// reproduces the SQL's semantics deliberately: substring() clamps a window past
// the end to empty, an unknown locator is ErrAgentSpillNotFound, and a re-save of
// the same locator overwrites.
type fakeSpillRows struct {
	saved map[string]storage.AgentSpillArtifact
	saves int
}

func newFakeSpillRows() *fakeSpillRows {
	return &fakeSpillRows{saved: map[string]storage.AgentSpillArtifact{}}
}

func (f *fakeSpillRows) SaveAgentSpill(_ context.Context, a storage.AgentSpillArtifact) (int, error) {
	a.Bytes = len(a.Content)
	f.saved[a.Locator] = a
	f.saves++
	return a.Bytes, nil
}

func (f *fakeSpillRows) AgentSpillWindowAt(_ context.Context, locator string, offset, limit int) (storage.AgentSpillWindow, error) {
	a, ok := f.saved[locator]
	if !ok {
		return storage.AgentSpillWindow{}, storage.ErrAgentSpillNotFound
	}
	total := len(a.Content)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	return storage.AgentSpillWindow{Content: []byte(a.Content[offset:end]), Offset: offset, Total: total}, nil
}

func (f *fakeSpillRows) OwnsAgentSpill(_ context.Context, locator, sessionKey string) (bool, error) {
	a, ok := f.saved[locator]
	return ok && a.SessionKey == sessionKey, nil
}

func newTestSpillStore() (*pgSpillStore, *fakeSpillRows) {
	rows := newFakeSpillRows()
	return &pgSpillStore{rows: rows}, rows
}

// TestSpillRoundTrip is the durability claim end to end: what the tool produced
// is exactly what a later read serves back, byte for byte, through the locator
// that went into the session log.
func TestSpillRoundTrip(t *testing.T) {
	st, rows := newTestSpillStore()
	content := strings.Repeat("row of query output\n", 5000)

	rec, err := st.SaveText(context.Background(), spill.SpillRequest{
		SessionID: "run-1", ToolName: "run_sql", CallID: "call-1",
		Label: "result", SuggestedName: "run_sql.txt", Content: content,
	})
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if !strings.HasPrefix(rec.Locator, "spill_") {
		t.Fatalf("locator is not opaque: %q", rec.Locator)
	}
	if rec.Bytes != len(content) {
		t.Fatalf("Bytes=%d want %d", rec.Bytes, len(content))
	}

	// Page the whole artifact back the way the model does: advance offset by the
	// bytes served until eof.
	var got strings.Builder
	offset := 0
	for range 1000 {
		slice, err := st.ReadText(context.Background(), rec.Locator, offset, 8*1024)
		if err != nil {
			t.Fatalf("ReadText at %d: %v", offset, err)
		}
		if slice.Total != len(content) {
			t.Fatalf("Total=%d want %d", slice.Total, len(content))
		}
		if slice.Offset != offset {
			t.Fatalf("Offset=%d want %d", slice.Offset, offset)
		}
		got.WriteString(slice.Content)
		offset = slice.Offset + len(slice.Content)
		if slice.EOF {
			break
		}
		if len(slice.Content) == 0 {
			t.Fatal("a non-EOF read served no bytes — a paging model would never finish")
		}
	}
	if got.String() != content {
		t.Fatalf("paged read lost content: got %d bytes, want %d", got.Len(), len(content))
	}
	if !strings.Contains(rows.saved[rec.Locator].SessionKey, "run-1") {
		t.Fatal("the artifact was not filed under its session")
	}
}

// TestSpillFenceIsPerSession: a locator is not a capability. One run may not read
// another's spilled output even holding the exact handle, and the two failures —
// unknown locator, someone else's locator — are indistinguishable.
func TestSpillFenceIsPerSession(t *testing.T) {
	st, _ := newTestSpillStore()
	rec, err := st.SaveText(context.Background(), spill.SpillRequest{
		SessionID: "run-1", ToolName: "run_sql", CallID: "call-1", Content: "secret rows",
	})
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if !st.OwnsSpill(rec.Locator, "run-1") {
		t.Fatal("the owning session cannot read its own spill")
	}
	if st.OwnsSpill(rec.Locator, "run-2") {
		t.Fatal("another session read a spill it did not produce")
	}
	if st.OwnsSpill("spill_deadbeef", "run-1") {
		t.Fatal("an unknown locator was accepted")
	}
	if _, err := st.ReadText(context.Background(), "spill_deadbeef", 0, 100); !errors.Is(err, spill.ErrSpillNotFound) {
		t.Fatalf("unknown locator returned %v, want spill.ErrSpillNotFound", err)
	}
}

// TestSpillLocatorCoversTheSession: two sessions producing byte-identical output
// from the same tool call must not land on one artifact, or the fence would be
// checking a row the other run also owns.
func TestSpillLocatorCoversTheSession(t *testing.T) {
	st, _ := newTestSpillStore()
	req := spill.SpillRequest{ToolName: "run_sql", CallID: "call-1", SuggestedName: "run_sql.txt", Content: "same bytes"}

	req.SessionID = "run-1"
	a, err := st.SaveText(context.Background(), req)
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	req.SessionID = "run-2"
	b, err := st.SaveText(context.Background(), req)
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if a.Locator == b.Locator {
		t.Fatalf("two sessions collided onto one locator: %q", a.Locator)
	}
}

// TestSpillReadIsRuneAligned: the offsets come from the model, so a window can
// start or end mid-rune. Every served slice must be valid UTF-8 and must report
// an offset a paging model can trust.
func TestSpillReadIsRuneAligned(t *testing.T) {
	st, _ := newTestSpillStore()
	// Three-byte runes: no read boundary that is a multiple of 4 falls on one.
	content := strings.Repeat("日本語テキスト", 200)
	rec, err := st.SaveText(context.Background(), spill.SpillRequest{
		SessionID: "run-1", ToolName: "fetch", CallID: "c1", Content: content,
	})
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}

	var got strings.Builder
	offset := 0
	for range 10000 {
		slice, err := st.ReadText(context.Background(), rec.Locator, offset, 10)
		if err != nil {
			t.Fatalf("ReadText: %v", err)
		}
		if len(slice.Content) == 0 && !slice.EOF {
			t.Fatalf("a 10-byte read at %d served nothing — paging would stall", offset)
		}
		if !isValidUTF8(slice.Content) {
			t.Fatalf("read at %d served a split rune", offset)
		}
		got.WriteString(slice.Content)
		offset = slice.Offset + len(slice.Content)
		if slice.EOF {
			break
		}
	}
	if got.String() != content {
		t.Fatalf("rune-aligned paging lost content: %d of %d bytes", got.Len(), len(content))
	}
}

// TestSpillResaveIsIdempotent: a resumed run replays retry-safe calls with their
// original ids, so the same artifact is legitimately saved twice. That must
// overwrite one row, not mint a second locator the log never mentions.
func TestSpillResaveIsIdempotent(t *testing.T) {
	st, rows := newTestSpillStore()
	req := spill.SpillRequest{SessionID: "run-1", ToolName: "run_sql", CallID: "c1", SuggestedName: "run_sql.txt", Content: "rows"}

	first, err := st.SaveText(context.Background(), req)
	if err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	second, err := st.SaveText(context.Background(), req)
	if err != nil {
		t.Fatalf("SaveText (replay): %v", err)
	}
	if first.Locator != second.Locator {
		t.Fatalf("a replayed call minted a new locator: %q then %q", first.Locator, second.Locator)
	}
	if len(rows.saved) != 1 {
		t.Fatalf("%d artifacts stored, want 1", len(rows.saved))
	}
}

// TestSpillStoreDrivesTheTool wires the store into the plugin the way a
// composition does and checks the model-facing path: an oversized result comes
// back as a preview plus a locator, and read_spill serves the rest.
func TestSpillStoreDrivesTheTool(t *testing.T) {
	st, _ := newTestSpillStore()
	ext, err := spill.To(st).BeginRun(context.Background(), agentcore.RunInfo{
		SessionID: "run-1",
		Limits:    agentcore.Limits{MaxToolResultLen: 512},
	})
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if ext == nil {
		t.Fatal("the spill plugin declined a run with a durable store")
	}

	out := strings.Repeat("x", 20000)
	interceptor, ok := ext.(interface {
		InterceptToolResult(context.Context, agentcore.ToolCall, string, error) agentcore.ToolResultDecision
	})
	if !ok {
		t.Fatal("the spill extension no longer intercepts tool results")
	}
	dec := interceptor.InterceptToolResult(context.Background(), agentcore.ToolCall{ID: "c1", Name: "run_sql"}, out, nil)
	if !dec.Replace {
		t.Fatal("an oversized result was not spilled")
	}
	if len(dec.Result) > 512 {
		t.Fatalf("the replacement (%d bytes) overran the inline cap", len(dec.Result))
	}
	if !strings.Contains(dec.Result, dec.Meta) {
		t.Fatalf("the notice does not carry the locator: %q", dec.Result)
	}

	slice, err := st.ReadText(context.Background(), dec.Meta, 0, 1024)
	if err != nil {
		t.Fatalf("ReadText on the minted locator: %v", err)
	}
	if slice.Total != len(out) {
		t.Fatalf("Total=%d want %d — the full result was not persisted", slice.Total, len(out))
	}
}

// isValidUTF8 reports whether s decodes cleanly, without importing the check
// into every assertion above.
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
