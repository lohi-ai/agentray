package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func piConvMessage(id, role, text string, display bool) storage.AgentConversationEntry {
	payload, _ := json.Marshal(convMessagePayload{Text: text, PiDisplay: display})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindMessage, Role: role, PayloadJSON: string(payload), CreatedAt: time.UnixMilli(123)}
}
func piConvDelta(id, base, revision string, messages ...string) storage.AgentConversationEntry {
	raw := make([]json.RawMessage, len(messages))
	for i, m := range messages {
		raw[i] = json.RawMessage(m)
	}
	payload, _ := json.Marshal(piConversationDelta{BaseEntryID: base, Revision: revision, Messages: raw})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindPiHistory, PayloadJSON: string(payload)}
}
func piConvMessages(t *testing.T, history PiConversationHistory) []json.RawMessage {
	t.Helper()
	var messages []json.RawMessage
	if err := json.Unmarshal(history.Messages, &messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestPiConversationPreservesNativeBlocksAndPendingInput(t *testing.T) {
	user := `{"role":"user","content":"stripped prompt","timestamp":9,"agentrayInputId":"user"}`
	assistant := `{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"m","content":[{"type":"thinking","thinking":"private","thinkingSignature":"opaque > & \u2028"},{"type":"toolCall","id":"call","name":"read","arguments":{},"extension":{"keep":true}}],"stopReason":"toolUse","timestamp":10}`
	result := `{"role":"toolResult","toolCallId":"call","toolName":"read","content":[{"type":"image","mimeType":"image/png","data":"abc"}],"details":{"opaque":[1,2]},"isError":false,"timestamp":11}`
	entries := []storage.AgentConversationEntry{
		piConvMessage("user", "user", "ultrathink stripped prompt", false),
		piConvMessage("late", "user", "do this next", false),
		piConvDelta("native", "", "revision", user, assistant, result),
		piConvMessage("display", "assistant", "rendered answer without private fields", true),
	}
	history, err := foldPiHistory(entries)
	if err != nil {
		t.Fatal(err)
	}
	messages := piConvMessages(t, history)
	if len(messages) != 4 || !samePiJSON(messages[0], []byte(user)) || !samePiJSON(messages[1], []byte(assistant)) || !samePiJSON(messages[2], []byte(result)) {
		t.Fatalf("lost original native transcript: %s", history.Messages)
	}
	if !strings.Contains(string(messages[3]), `"agentrayInputId":"late"`) || history.LeafID != "display" || history.Revision != "revision" {
		t.Fatalf("lost pending correction or branch marker: %+v", history)
	}
	// A later run seeds the complete folded transcript, including the still-pending
	// user input. Its delta adds only new messages; neither input is duplicated.
	next := piConvDelta("next", "display", "revision", `{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":12}`)
	continued, err := foldPiHistory(append(entries, next))
	if err != nil || len(piConvMessages(t, continued)) != 5 {
		t.Fatalf("next run duplicated history: %s %v", continued.Messages, err)
	}
}

func TestPiConversationConsumedSteeringIsNotReplayed(t *testing.T) {
	history, err := foldPiHistory([]storage.AgentConversationEntry{
		piConvMessage("user", "user", "first", false), piConvMessage("steer", "user", "again", false),
		piConvDelta("native", "", "revision", `{"role":"user","content":"first","agentrayInputId":"user"}`, `{"role":"user","content":"again","agentrayInputId":"steer"}`),
	})
	if err != nil || len(piConvMessages(t, history)) != 2 {
		t.Fatalf("duplicated consumed input: %s %v", history.Messages, err)
	}
}

func TestPiConversationForkAndClear(t *testing.T) {
	first := []storage.AgentConversationEntry{piConvMessage("u1", "user", "first", false), piConvDelta("n1", "", "r", `{"role":"user","content":"first","agentrayInputId":"u1"}`, `{"role":"assistant","content":[],"branch":"first"}`), piConvMessage("a1", "assistant", "first", true)}
	branch := append(append([]storage.AgentConversationEntry{}, first...), piConvMessage("fork-user", "user", "replace", false), piConvDelta("fork-native", "a1", "r", `{"role":"user","content":"replace","agentrayInputId":"fork-user"}`, `{"role":"assistant","content":[],"branch":"replacement"}`))
	history, err := foldPiHistory(branch)
	if err != nil || len(piConvMessages(t, history)) != 4 || !strings.Contains(string(history.Messages), "replacement") {
		t.Fatalf("wrong fork: %s %v", history.Messages, err)
	}
	old := []storage.AgentConversationEntry{piConvMessage("legacy", "assistant", "legacy", false), {ID: "compact", Kind: ConvKindCompaction}, {ID: "clear", Kind: ConvKindClear}}
	old = append(old, piConvMessage("new-user", "user", "new", false), piConvDelta("new-native", "clear", "r2", `{"role":"user","content":"new","agentrayInputId":"new-user"}`))
	history, err = foldPiHistory(old)
	if err != nil || len(piConvMessages(t, history)) != 1 || history.Revision != "r2" {
		t.Fatalf("clear did not reset context: %s %v", history.Messages, err)
	}
}

func TestPiConversationRejectsInvalidNativePaths(t *testing.T) {
	valid := piConvDelta("first", "", "r", `{"role":"user","content":"native"}`)
	for name, path := range map[string][]storage.AgentConversationEntry{
		"legacy assistant":  {piConvMessage("legacy", "assistant", "text", false)},
		"legacy compaction": {{Kind: ConvKindCompaction}},
		"missing anchor":    {piConvDelta("delta", "absent", "r")},
		"revision changes":  {valid, piConvDelta("delta", "first", "other")},
		"superseded anchor": {valid, piConvDelta("delta", "", "r")},
		"incomplete tools":  {piConvDelta("delta", "", "r", `{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"write"}]}`)},
		"malformed delta":   {{Kind: ConvKindPiHistory, PayloadJSON: `{"revision":"r","messages":null}`}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := foldPiHistory(path); err == nil {
				t.Fatal("accepted invalid native path")
			}
		})
	}
	if err := requireLegacyConversation([]storage.AgentConversationEntry{valid}); err == nil {
		t.Fatal("legacy reducer accepted native history")
	}
	if err := requireLegacyConversation([]storage.AgentConversationEntry{valid, {Kind: ConvKindClear}}); err != nil {
		t.Fatal(err)
	}
}

func TestPiConversationSuffixChecksSeedAndToolBoundaries(t *testing.T) {
	before := json.RawMessage(`[{"role":"user","content":"seed","extension":{"signature":"opaque"}}]`)
	valid := json.RawMessage(`{"messages":[{"extension":{"signature":"opaque"},"content":"seed","role":"user"},{"role":"assistant","content":[],"extra":true}]}`)
	suffix, err := piConversationSuffix(before, valid)
	if err != nil || len(suffix) != 1 || !strings.Contains(string(suffix[0]), `"extra":true`) {
		t.Fatalf("lost native suffix: %s %v", suffix, err)
	}
	for _, state := range []string{`{"messages":[]}`, `{"messages":null}`, `{"messages":[{"role":"user","content":"seed"}]}`, `{"messages":[{"role":"user","content":"seed","extension":{"signature":"opaque"}},{"role":"assistant","content":[{"type":"toolCall","id":"missing","name":"write"}]}]}`} {
		if _, err := piConversationSuffix(before, []byte(state)); err == nil {
			t.Fatalf("accepted changed or incomplete history: %s", state)
		}
	}
}

func TestPiChatUsesNativeHandlerWithoutGoClassifier(t *testing.T) {
	history := &PiConversationHistory{Messages: json.RawMessage(`[]`), LeafID: "anchor"}
	svc := NewChatService(nil, WithPiRuntime(PiRuntimeConfig{Worker: "unused"}))
	svc.classify = func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
		t.Fatal("native chat invoked legacy classifier")
		return chatDecision{}, nil
	}
	calls := 0
	svc.handle = func(_ context.Context, work chatWork, _ agentcore.StreamSink) (ChatResult, error) {
		calls++
		if work.PiHistory != history || work.InputID != "input" || work.Message != "hello" {
			t.Fatalf("lost native turn: %+v", work)
		}
		return ChatResult{Final: "native hello"}, nil
	}
	result, err := svc.Chat(context.Background(), ChatOptions{Message: "hello", PiHistory: history, InputID: "input"}, nil)
	if err != nil || result.Final != "native hello" || calls != 1 {
		t.Fatalf("native route failed: %+v %v", result, err)
	}
	_, err = svc.Chat(context.Background(), ChatOptions{Message: "hello", History: []agentcore.Message{{Role: agentcore.RoleAssistant, Content: "legacy"}}}, nil)
	if err == nil || calls != 1 {
		t.Fatal("legacy history entered native handler")
	}
}

func TestPiLiveInputPersistsBeforeQueueAndKeepsIdentity(t *testing.T) {
	registry := NewLiveRegistry()
	live := registry.register("conversation", "project", LiveAuthority{CanWrite: true}, nil)
	persisted := false
	ok, err := registry.QueueInput("project", "conversation", false, LiveAuthority{CanWrite: true}, func() (agentcore.Message, error) {
		if got := drainMessages(live.steer); len(got) > 0 {
			t.Fatal("queued before persistence")
		}
		persisted = true
		return agentcore.Message{Role: agentcore.RoleUser, Content: "correction", InputID: "entry"}, nil
	})
	if err != nil || ok != LiveControlDelivered || !persisted {
		t.Fatalf("queue failed: %v %v", ok, err)
	}
	native, err := piHostMessages(drainMessages(live.steer))
	if err != nil || len(native) != 1 || !strings.Contains(string(native[0]), `"agentrayInputId":"entry"`) {
		t.Fatalf("lost correlation: %s %v", native, err)
	}
	_, err = registry.QueueInput("project", "conversation", false, LiveAuthority{CanWrite: true}, func() (agentcore.Message, error) { return agentcore.Message{}, context.Canceled })
	if err == nil || len(drainMessages(live.steer)) != 0 {
		t.Fatal("failed durable input reached queue")
	}
	ok, err = registry.QueueInput("foreign", "conversation", false, LiveAuthority{CanWrite: true}, func() (agentcore.Message, error) { t.Fatal("persisted foreign input"); return agentcore.Message{}, nil })
	if err != nil || ok != LiveControlNotFound {
		t.Fatal("foreign session accepted")
	}
}

func TestPiJSONIdentityPreservesNumbersAcrossJSONBSpelling(t *testing.T) {
	for _, pair := range [][2]string{
		{`1e3`, `1000`}, {`1.000`, `1`}, {`4e-8`, `0.00000004`},
		{`1e21`, `1000000000000000000000`}, {`-0.000`, `0`},
		{`9.007199254740993e15`, `9007199254740993`},
		{`1e00000000000000000000000000000003`, `1000`},
		{`1e1000000000000000000000000`, `10e999999999999999999999999`},
	} {
		if !samePiJSON([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("same number has different identity: %s / %s", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{`9007199254740992`, `9007199254740993`},
		{`0.123456789012345678901234567890`, `0.123456789012345678901234567891`},
		{`1e309`, `2e309`}, {`null`, `0`}, {`"1000"`, `1000`},
		{`{"extension":[9007199254740992]}`, `{"extension":[9007199254740993]}`},
		{`1 2`, `1`},
	} {
		if samePiJSON([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("different values share an identity: %s / %s", pair[0], pair[1])
		}
	}
	// Native number spellings emitted by JSON.stringify / encoding/json keep
	// their previous digest representation, including very small cost fields.
	for _, number := range []float64{0, 1, -123, 0.1, 0.000001, 0.0000001, 4e-8, 1e20, 1e21, 1.25e30, 1.2345678901234567, 5e-324} {
		raw, _ := json.Marshal(number)
		canonical, err := canonicalPiJSON(raw)
		if err != nil || string(canonical) != string(raw) {
			t.Errorf("ordinary native digest spelling changed: %s -> %s (%v)", raw, canonical, err)
		}
	}
}
