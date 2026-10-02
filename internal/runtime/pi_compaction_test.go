package agentruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func piCompactionFixture() []string {
	return []string{
		`{"role":"system","content":"","sections":{"agentray":"host policy"},"toolsAdded":[{"name":"read","description":"read","parameters":{"type":"object"}}],"timestamp":1}`,
		`{"role":"user","content":"old question","timestamp":2}`,
		`{"role":"assistant","content":[{"type":"text","text":"old answer"}],"timestamp":3}`,
		`{"role":"system","content":"","toolsRemoved":[{"name":"read"}],"sections":{"agentray":"new policy"},"timestamp":4}`,
		`{"role":"user","content":[{"type":"image","mimeType":"image/png","data":"cGk="},{"type":"text","text":"recent question"}],"timestamp":5,"opaque":{"signature":"unchanged"}}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","thinkingSignature":"opaque-signature"},{"type":"toolCall","id":"c","name":"inspect","arguments":{"x":1}}],"timestamp":6}`,
		`{"role":"toolResult","toolCallId":"c","toolName":"inspect","content":[{"type":"text","text":"native result"}],"details":{"extension":"preserve"},"isError":false,"timestamp":7}`,
		`{"role":"assistant","content":[{"type":"text","text":"recent answer"}],"timestamp":8}`,
	}
}

func piCompactionEntry(id, base, revision string, cut int, summary string) storage.AgentConversationEntry {
	payload, _ := json.Marshal(piConversationCompaction{BaseEntryID: base, Revision: revision, KeepFrom: cut, Summary: summary})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindPiCompaction, PayloadJSON: string(payload), CreatedAt: time.UnixMilli(9)}
}

func TestPiCompactionPreservesNativeTailAndSystemState(t *testing.T) {
	fixture := piCompactionFixture()
	entries := []storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)}
	history, err := foldPiHistory(entries)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPiCompaction(history, 0, true)
	if err != nil || plan.cut != 4 {
		t.Fatalf("bad native cut: %+v %v", plan, err)
	}
	if plan, err := planPiCompaction(history, 100000, false); err != nil || plan.cut != 0 {
		t.Fatalf("short context compacted: %+v %v", plan, err)
	}
	entries = append(entries, piCompactionEntry("compact", "native", "r", plan.cut, "old decision"))
	compacted, err := foldPiHistory(entries)
	if err != nil {
		t.Fatal(err)
	}
	got := piConvMessages(t, compacted)
	if len(got) != 7 || !samePiJSON(got[0], []byte(fixture[0])) || !samePiJSON(got[1], []byte(fixture[3])) {
		t.Fatalf("system state changed: %s", compacted.Messages)
	}
	for i, raw := range fixture[4:] {
		if !samePiJSON(got[i+3], []byte(raw)) {
			t.Fatalf("native tail changed: %s", got[i+3])
		}
	}
	if strings.Contains(string(compacted.Messages), "old answer") || !strings.Contains(string(got[2]), "old decision") {
		t.Fatal("summary did not replace old exchange")
	}
	if plan, err := planPiCompaction(compacted, 1, true); err != nil || plan.cut != 0 {
		t.Fatalf("immediate re-compaction: %+v %v", plan, err)
	}
	if err := requireLegacyConversation(entries); err == nil {
		t.Fatal("native compaction entered legacy reducer")
	}
	// The next native delta is anchored to the compacted branch, not its
	// superseded pre-compaction transcript. Future summaries retain the old one.
	entries = append(entries, piConvDelta("next", "compact", "r", `{"role":"user","content":"next question"}`, `{"role":"assistant","content":[]}`))
	next, err := foldPiHistory(entries)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planPiCompaction(next, 1, true)
	if err != nil || second.cut != 7 || !strings.Contains(string(second.messages[2]), "old decision") {
		t.Fatalf("lost iterative summary: %+v %v", second, err)
	}
	entries = append(entries, storage.AgentConversationEntry{ID: "clear", Kind: ConvKindClear})
	cleared, err := foldPiHistory(entries)
	if err != nil || string(cleared.Messages) != "[]" || cleared.Revision != "" {
		t.Fatalf("clear resurrected summary: %+v %v", cleared, err)
	}
}

func TestPiCompactionRejectsStaleOrBrokenCheckpoints(t *testing.T) {
	base := piConvDelta("native", "", "r", piCompactionFixture()...)
	for name, entry := range map[string]storage.AgentConversationEntry{
		"stale":      piCompactionEntry("c", "old", "r", 4, "summary"),
		"revision":   piCompactionEntry("c", "native", "other", 4, "summary"),
		"empty":      piCompactionEntry("c", "native", "r", 4, " "),
		"tool batch": piCompactionEntry("c", "native", "r", 6, "summary"),
		"no tail":    piCompactionEntry("c", "native", "r", 8, "summary"),
		"negative":   piCompactionEntry("c", "native", "r", -1, "summary"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := foldPiHistory([]storage.AgentConversationEntry{base, entry}); err == nil {
				t.Fatal("accepted invalid checkpoint")
			}
		})
	}
	valid := piCompactionEntry("compact", "native", "r", 4, "summary")
	if _, err := foldPiHistory([]storage.AgentConversationEntry{base, valid, piConvDelta("late", "native", "r", `{"role":"user","content":"stale result"}`)}); err == nil {
		t.Fatal("stale delta crossed compaction")
	}
}

func TestPiCompactionKeepsUnansweredQuestionsAndToolBatches(t *testing.T) {
	fixture := piCompactionFixture()
	fixture[6] = `{"role":"toolResult","toolCallId":"c","toolName":"ask","content":[],"details":{"parked":true,"questionId":"physical-call"}}`
	history, err := foldPiHistory([]storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)})
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := planPiCompaction(history, 1, true); !errors.Is(err, ErrPiQuestionPending) || plan.cut != 0 {
		t.Fatalf("compacted a pending human question: %+v %v", plan, err)
	}
	fixture = append(fixture, `{"role":"user","content":"human answer","agentrayAnswerId":"physical-call"}`, `{"role":"assistant","content":[]}`)
	history, err = foldPiHistory([]storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)})
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := planPiCompaction(history, 1, true); err != nil || plan.cut == 0 {
		t.Fatalf("answered question stayed blocked: %+v %v", plan, err)
	}
	if _, err := planPiCompaction(PiConversationHistory{Revision: "r", Messages: json.RawMessage(`[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"write"}]},{"role":"user","content":"second"}]`)}, 1, true); err == nil {
		t.Fatal("cut an unsettled batch")
	}
}

func TestPiCompactionAutomaticThresholdUsesNativeBlocks(t *testing.T) {
	fixture := piCompactionFixture()
	fixture[1] = `{"role":"user","content":"` + strings.Repeat("older ", 10000) + `"}`
	fixture[4] = `{"role":"user","content":"` + strings.Repeat("recent ", 15000) + `"}`
	history, err := foldPiHistory([]storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPiCompaction(history, 12000, false)
	if err != nil || plan.cut != 4 || plan.tokens < 12000 {
		t.Fatalf("native context not counted: %+v %v", plan, err)
	}
}

func TestPiCompactionRetainsOpaqueToolDetails(t *testing.T) {
	fixture := piCompactionFixture()
	fixture[6] = `{"role":"toolResult","toolCallId":"c","toolName":"inspect","content":[],"details":["opaque",{"version":2}]}`
	entries := []storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)}
	history, err := foldPiHistory(entries)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPiCompaction(history, 1, true)
	if err != nil || plan.cut != 4 {
		t.Fatalf("opaque native metadata blocked compaction: %+v %v", plan, err)
	}
	compacted, err := foldPiHistory(append(entries, piCompactionEntry("c", "native", "r", plan.cut, "old summary")))
	if err != nil || !samePiJSON(piConvMessages(t, compacted)[5], []byte(fixture[6])) {
		t.Fatalf("opaque native details changed: %s %v", compacted.Messages, err)
	}
}

func TestPiCompactionAdaptsToSmallModelWindow(t *testing.T) {
	fixture := piCompactionFixture()
	fixture[1] = `{"role":"user","content":"` + strings.Repeat("old ", 1500) + `"}`
	fixture[4] = `{"role":"user","content":"` + strings.Repeat("new ", 1500) + `"}`
	history, err := foldPiHistory([]storage.AgentConversationEntry{piConvDelta("native", "", "r", fixture...)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPiCompaction(history, 3000, false)
	if err != nil || plan.cut != 4 {
		t.Fatalf("small model kept an oversized default recent window: %+v %v", plan, err)
	}
	plan, err = planPiCompaction(history, 10000, false)
	if err != nil || plan.cut != 0 {
		t.Fatalf("below-threshold context compacted: %+v %v", plan, err)
	}
}
