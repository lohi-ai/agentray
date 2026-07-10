package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// TestSessionEntryFromRowRoundTrip verifies a stored row's JSON payload is
// unmarshalled back into the full typed entry and the store-assigned Seq wins.
func TestSessionEntryFromRowRoundTrip(t *testing.T) {
	row := storage.AgentSessionEntry{
		Seq:         7,
		Kind:        string(agentcore.EntryMessage),
		Turn:        3,
		PayloadJSON: `{"seq":0,"kind":"message","turn":3,"message":{"role":"assistant","content":"done"}}`,
	}
	e := sessionEntryFromRow(row)
	if e.Seq != 7 {
		t.Fatalf("Seq = %d, want 7 (row is authoritative)", e.Seq)
	}
	if e.Kind != agentcore.EntryMessage || e.Turn != 3 {
		t.Fatalf("kind/turn = %s/%d, want message/3", e.Kind, e.Turn)
	}
	if e.Message == nil || e.Message.Content != "done" {
		t.Fatalf("message not restored: %+v", e.Message)
	}
}

// TestSessionEntryFromRowMalformedDegrades verifies a bad payload yields an entry
// carrying only the row's kind/turn/seq rather than failing.
func TestSessionEntryFromRowMalformedDegrades(t *testing.T) {
	row := storage.AgentSessionEntry{Seq: 2, Kind: "leaf", Turn: 5, PayloadJSON: "not json"}
	e := sessionEntryFromRow(row)
	if e.Seq != 2 || e.Kind != agentcore.EntryLeaf || e.Turn != 5 {
		t.Fatalf("degraded entry = %+v, want seq2/leaf/turn5", e)
	}
	if e.Message != nil {
		t.Fatalf("malformed payload should leave Message nil, got %+v", e.Message)
	}
}
