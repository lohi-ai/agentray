package agentruntime

import (
	"encoding/json"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
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

func TestSessionEntryFromRowRestoresToolOutcome(t *testing.T) {
	want := agentcore.SessionEntry{
		Kind: agentcore.EntryToolOutcome, Turn: 4, CallID: "call-7",
		Outcome: &agentcore.ToolOutcomeRecord{
			Message:  agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "call-7", Name: "read", Content: "result"},
			Trace:    agentcore.ToolTrace{CallID: "call-7", Tool: "read", Args: `{}`, Allowed: true},
			Executed: true,
		},
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := sessionEntryFromRow(storage.AgentSessionEntry{
		Seq: 11, Kind: string(agentcore.EntryToolOutcome), Turn: 4, PayloadJSON: string(payload),
	})
	if got.Seq != 11 || got.Outcome == nil || got.Outcome.Message.Content != "result" || got.Outcome.Trace.CallID != "call-7" || !got.Outcome.Executed {
		t.Fatalf("tool outcome did not round-trip: %+v", got)
	}
}

// TestRootRunID verifies the child-session key convention: the root run UUID is
// everything before the first "/", so child appends ("<runID>/<callID>") keep a
// valid run_id FK while the full key stays the log identity.
func TestRootRunID(t *testing.T) {
	for in, want := range map[string]string{
		"a1b2":              "a1b2",
		"a1b2/call_9":       "a1b2",
		"a1b2/call_9/inner": "a1b2",
	} {
		if got := rootRunID(in); got != want {
			t.Errorf("rootRunID(%q) = %q, want %q", in, got, want)
		}
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
