package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2found/2ai/agentcore"
)

func TestPiInvocationRejectsMalformedAndRepeatedContracts(t *testing.T) {
	for _, raw := range []string{"null", "[]", "3", `"text"`, `{"broken"}`} {
		if _, err := piStoredInvocation([]agentcore.SessionEntry{{Kind: agentcore.EntryPiInvocation, Content: raw}}); err == nil {
			t.Fatalf("accepted malformed invocation: %s", raw)
		}
	}
	entry := agentcore.SessionEntry{Kind: agentcore.EntryPiInvocation, Content: `{"prompt":"task"}`}
	if _, err := piStoredInvocation([]agentcore.SessionEntry{entry, entry}); err == nil {
		t.Fatal("accepted repeated contract")
	}
	if equalPiInvocation(nil, []byte(`{}`)) || !equalPiInvocation([]byte(`{"a":1,"b":2}`), []byte(`{"b":2,"a":1}`)) {
		t.Fatal("invocation comparison lost presence or JSON semantics")
	}
}

func TestPiChildCompletionBindsNativeTranscriptRevisionAndAnswer(t *testing.T) {
	state := json.RawMessage(`{"messages":[{"role":"user","content":"task","timestamp":1},{"role":"assistant","content":[{"type":"thinking","thinking":"private","thinkingSignature":"opaque"},{"type":"text","text":"answer"}],"extension":{"keep":true},"timestamp":2}]}`)
	digest, err := piMessagesDigest(state)
	if err != nil {
		t.Fatal(err)
	}
	receipt := piChildCompletion{Final: "answer", Revision: "revision", MessagesDigest: digest, StopReason: "stop"}
	raw, _ := json.Marshal(receipt)
	entries := []agentcore.SessionEntry{{Kind: agentcore.EntryPiInvocation, Content: `{"prompt":"task"}`}, {Kind: piStateEntry, Content: string(state), Model: "revision"}, {Kind: agentcore.EntryPiChildResult, Content: string(raw)}}
	got, done, err := piCompletedChild(entries)
	if err != nil || !done || got.Final != "answer" || got.Usage.InputTokens != 0 || !strings.Contains(string(got.NativeState), `"thinkingSignature":"opaque"`) {
		t.Fatalf("bad reattachment: %+v %v", got, err)
	}
	for name, alter := range map[string]func([]agentcore.SessionEntry) []agentcore.SessionEntry{
		"changed state": func(es []agentcore.SessionEntry) []agentcore.SessionEntry {
			es[1].Content = strings.ReplaceAll(es[1].Content, "opaque", "tampered")
			return es
		},
		"changed revision": func(es []agentcore.SessionEntry) []agentcore.SessionEntry { es[1].Model = "other"; return es },
		"changed answer": func(es []agentcore.SessionEntry) []agentcore.SessionEntry {
			es[2].Content = strings.ReplaceAll(es[2].Content, `"final":"answer"`, `"final":"invented"`)
			return es
		},
		"interrupted": func(es []agentcore.SessionEntry) []agentcore.SessionEntry {
			es[2].Content = strings.ReplaceAll(es[2].Content, `"stop_reason":"stop"`, `"stop_reason":"aborted"`)
			return es
		},
		"record after completion": func(es []agentcore.SessionEntry) []agentcore.SessionEntry {
			return append(es, agentcore.SessionEntry{Kind: piEventEntry, Content: `{"type":"agent_start"}`})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := piCompletedChild(alter(append([]agentcore.SessionEntry{}, entries...))); err == nil {
				t.Fatal("accepted unverified completion")
			}
		})
	}
}

func TestPiChildDigestPreservesOpaqueLargeIntegers(t *testing.T) {
	first := json.RawMessage(`{"messages":[{"role":"user","content":"task","opaque":9007199254740992}]}`)
	second := json.RawMessage(`{"messages":[{"role":"user","content":"task","opaque":9007199254740993}]}`)
	a, err := piMessagesDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := piMessagesDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("child receipt conflates distinct native fields above float64 precision")
	}
	reordered, err := piMessagesDigest(json.RawMessage(`{"messages":[{"opaque":9007199254740992,"content":"task","role":"user"}]}`))
	if err != nil || reordered != a {
		t.Fatalf("JSONB key ordering changed child identity: %v", err)
	}
}

func TestPiChildDigestMatchesSQLDecimalExponentForms(t *testing.T) {
	a, err := piMessagesDigest(json.RawMessage(`{"messages":[{"role":"assistant","usage":{"cost":{"total":4e-8}},"content":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := piMessagesDigest(json.RawMessage(`{"messages":[{"content":[],"usage":{"cost":{"total":0.0000000400}},"role":"assistant"}]}`))
	if err != nil || a != b {
		t.Fatalf("SQL number spelling invalidated the child receipt: %s / %s (%v)", a, b, err)
	}
}
