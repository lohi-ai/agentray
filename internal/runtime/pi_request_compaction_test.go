package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func piLongRequest() []json.RawMessage {
	result := []json.RawMessage{json.RawMessage(`{"role":"system","content":"policy","toolsAdded":[{"name":"read","parameters":{"type":"object"}}]}`), json.RawMessage(`{"role":"user","content":"complete the task"}`)}
	for i := 0; i < 4; i++ {
		result = append(result, json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"reason","thinkingSignature":"opaque"},{"type":"toolCall","id":"reused","name":"read","arguments":{}}]}`))
		raw, _ := json.Marshal(map[string]any{"role": "toolResult", "toolCallId": "reused", "toolName": "read", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("source ", 500)}}, "details": map[string]any{"opaque": i}})
		result = append(result, raw)
	}
	return result
}

func piRequestJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func TestPiRequestCompactionPreservesTranscriptAndReusesExactPrefix(t *testing.T) {
	messages := piLongRequest()
	raw := piRequestJSON(messages)
	before := string(raw)
	calls, writes, total := 0, 0, 0
	c := piRequestCompactor{revision: "r", config: PiContextCompaction{Budget: 1500, KeepRecent: 500, Summarize: func(_ context.Context, raw json.RawMessage, revision string) (string, agentcore.Usage, error) {
		calls++
		if validatePiConversationMessages(raw) != nil || revision != "r" {
			t.Fatal("summary split tool batch")
		}
		return "Task and earlier evidence.", agentcore.Usage{InputTokens: 17}, nil
	}}, record: func(context.Context, json.RawMessage) error { writes++; return nil }, usage: func(u agentcore.Usage) { total += u.InputTokens }}
	view := c.transform(context.Background(), raw)
	if c.saved == nil || calls != 1 || writes != 1 || total != 17 || len(view) >= len(raw) {
		t.Fatalf("compaction did not persist/save bytes: calls=%d writes=%d usage=%d", calls, writes, total)
	}
	if string(raw) != before {
		t.Fatal("request transform rewrote original transcript")
	}
	var got []json.RawMessage
	_ = json.Unmarshal(view, &got)
	if !samePiJSON(got[0], messages[0]) {
		t.Fatal("native system/tool state changed")
	}
	for i, tail := range messages[c.saved.PrefixCount:] {
		if !samePiJSON(tail, got[i+2]) {
			t.Fatal("retained block/signature/result changed")
		}
	}
	// A fresh compactor loads only its durable summary. No paid replay or usage
	// charge occurs when the same full native transcript is presented again.
	persisted, err := parsePiContextSummary(string(piRequestJSON(c.saved)))
	if err != nil {
		t.Fatal(err)
	}
	restored := piRequestCompactor{revision: "r", saved: &persisted, config: c.config, record: c.record, usage: c.usage}
	again := restored.transform(context.Background(), raw)
	if !samePiJSON(view, again) || calls != 1 || total != 17 {
		t.Fatal("restored summary reran or charged historical work")
	}
	// An opaque edit invalidates the summary even when rendered text is equal.
	messages[2] = json.RawMessage(strings.Replace(string(messages[2]), "opaque", "different-signature", 1))
	changed := restored.transform(context.Background(), piRequestJSON(messages))
	if calls != 2 || samePiJSON(view, changed) {
		t.Fatal("summary survived a changed native prefix")
	}
}

func TestPiRequestCompactionIteratesWithoutLosingEarlierSummary(t *testing.T) {
	messages := piLongRequest()
	calls := 0
	c := piRequestCompactor{revision: "r", config: PiContextCompaction{Budget: 1500, KeepRecent: 500, Summarize: func(_ context.Context, raw json.RawMessage, _ string) (string, agentcore.Usage, error) {
		calls++
		if calls > 1 && !strings.Contains(string(raw), "prior facts") {
			t.Fatal("iterative summary lost earlier facts")
		}
		return "prior facts and more evidence", agentcore.Usage{}, nil
	}}}
	c.transform(context.Background(), piRequestJSON(messages))
	first := c.saved.PrefixCount
	messages = append(messages, piLongRequest()[2:]...)
	view := c.transform(context.Background(), piRequestJSON(messages))
	if calls != 2 || c.saved.PrefixCount <= first || c.saved.PrefixDigest != piPrefixDigest(messages[:c.saved.PrefixCount]) || validatePiConversationMessages(view) != nil {
		t.Fatal("iterative compaction lost original prefix indexing")
	}
}

func TestPiRequestCompactionFailuresUseSafeNativeView(t *testing.T) {
	raw := piRequestJSON(piLongRequest())
	for _, kind := range []string{"error", "panic", "empty", "too long", "storage", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			charged := 0
			c := piRequestCompactor{revision: "r", config: PiContextCompaction{Budget: 1500, KeepRecent: 500, Summarize: func(context.Context, json.RawMessage, string) (string, agentcore.Usage, error) {
				u := agentcore.Usage{InputTokens: 9}
				switch kind {
				case "error":
					return "", u, errors.New("summary failed")
				case "panic":
					panic("broken observer")
				case "empty":
					return " ", u, nil
				case "too long":
					return strings.Repeat("larger ", 5000), u, nil
				case "cancel":
					cancel()
				}
				return "summary", u, nil
			}}, record: func(context.Context, json.RawMessage) error {
				if kind == "storage" {
					return errors.New("write failed")
				}
				t.Fatal("failed summary published")
				return nil
			}, usage: func(u agentcore.Usage) { charged += u.InputTokens }}
			if got := c.transform(ctx, raw); !samePiJSON(got, raw) || c.saved != nil {
				t.Fatal("failed summary changed view")
			}
			if kind != "panic" && charged != 9 {
				t.Fatal("failed model work was counted as free")
			}
		})
	}
}

func TestPiRequestCompactionDigestAndCorruptRecords(t *testing.T) {
	a := []json.RawMessage{json.RawMessage(`{"role":"user","extension":9007199254740992}`)}
	b := []json.RawMessage{json.RawMessage(`{"extension":9007199254740992,"role":"user"}`)}
	c := []json.RawMessage{json.RawMessage(`{"role":"user","extension":9007199254740993}`)}
	if piPrefixDigest(a) != piPrefixDigest(b) || piPrefixDigest(a) == piPrefixDigest(c) {
		t.Fatal("digest lost JSON identity or numeric precision")
	}
	if _, err := recoverPiState([]agentcore.SessionEntry{{Kind: agentcore.EntryPiState, Content: `{"messages":[]}`}, {Kind: agentcore.EntryPiContextSummary, Content: `{"prefix_count":0}`}}); err == nil {
		t.Fatal("corrupt summary was accepted on recovery")
	}
	broken := []json.RawMessage{json.RawMessage(`{"role":"user","content":"task"}`), json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"write"}]}`), json.RawMessage(`{"role":"user","content":"later"}`)}
	if piRequestCompactionCut(broken, 1, 1) != 0 {
		t.Fatal("compaction split an unfinished effect batch")
	}
}
