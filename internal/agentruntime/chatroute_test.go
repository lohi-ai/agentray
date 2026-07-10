package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// These tests exercise ChatService's routing logic by driving its injectable
// classify/handle seams directly — no Runner, provider, or database required.

func TestDecodeDecision(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantRoute string
		wantReply string
	}{
		{"clean smalltalk", `{"route":"smalltalk","reply":"Hey there!"}`, "smalltalk", "Hey there!"},
		{"fenced", "```json\n{\"route\":\"smalltalk\",\"reply\":\"Hi!\"}\n```", "smalltalk", "Hi!"},
		{"prose around json", `Sure: {"route":"smalltalk","reply":"Hello {friend}"} done`, "smalltalk", "Hello {friend}"},
		{"data route", `{"route":"data"}`, "data", ""},
		{"garbage", `not json at all`, "", ""},
		{"empty", ``, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, reply := decodeDecision(tc.raw)
			if route != tc.wantRoute || reply != tc.wantReply {
				t.Fatalf("decodeDecision(%q) = (%q,%q), want (%q,%q)", tc.raw, route, reply, tc.wantRoute, tc.wantReply)
			}
		})
	}
}

func TestRecentHistory(t *testing.T) {
	hist := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: "sys"}, // dropped
		{Role: agentcore.RoleUser, Content: "1"},
		{Role: agentcore.RoleTool, Name: "x", Content: "tool"}, // dropped
		{Role: agentcore.RoleAssistant, Content: "2"},
		{Role: agentcore.RoleUser, Content: "3"},
	}
	got := recentHistory(hist, 2)
	if len(got) != 2 || got[0].Content != "2" || got[1].Content != "3" {
		t.Fatalf("recentHistory tail/filter wrong: %+v", got)
	}
}

func collect(sink *[]agentcore.StreamEvent) agentcore.StreamSink {
	return func(ev agentcore.StreamEvent) { *sink = append(*sink, ev) }
}

func TestChatDirectReply(t *testing.T) {
	handled := false
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: routeSmallTalk, Reply: "Hi there", Usage: agentcore.Usage{InputTokens: 3}}, nil
		},
		handle: func(context.Context, chatWork, agentcore.StreamSink) (ChatResult, error) {
			handled = true
			return ChatResult{}, nil
		},
	}
	var evs []agentcore.StreamEvent
	res, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "hi"}, collect(&evs))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if handled {
		t.Fatal("data handler must not run for a direct reply")
	}
	if res.Route != routeSmallTalk || res.Final != "Hi there" || res.RunID != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Usage.InputTokens != 3 {
		t.Fatalf("classify usage not reported: %+v", res.Usage)
	}
	// Opening progress beat + word-by-word tokens.
	var text strings.Builder
	tokens := 0
	for _, e := range evs {
		if e.Type == agentcore.StreamToken {
			text.WriteString(e.Token)
			tokens++
		}
	}
	if tokens != 2 || strings.TrimSpace(text.String()) != "Hi there" {
		t.Fatalf("streamed tokens wrong: %d %q", tokens, text.String())
	}
}

func TestChatRoutesToData(t *testing.T) {
	card := &agentcore.ResultCard{Title: "Signups", Kind: "stat"}
	handled := false
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: routeData, Usage: agentcore.Usage{InputTokens: 10, OutputTokens: 5}}, nil
		},
		handle: func(_ context.Context, _ chatWork, sink agentcore.StreamSink) (ChatResult, error) {
			handled = true
			if sink != nil {
				sink(agentcore.StreamEvent{Type: agentcore.StreamCard, Card: card})
			}
			return ChatResult{
				RunID: "run1", Final: "42 signups", Turns: 2,
				Usage: agentcore.Usage{InputTokens: 100, OutputTokens: 50}, Card: card,
			}, nil
		},
	}
	var evs []agentcore.StreamEvent
	res, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "how many signups?"}, collect(&evs))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !handled {
		t.Fatal("data handler should have run")
	}
	if res.RunID != "run1" || res.Route != routeData || res.Final != "42 signups" || res.Card != card {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Classify usage folds into the handler's usage.
	if res.Usage.InputTokens != 110 || res.Usage.OutputTokens != 55 {
		t.Fatalf("usage not folded: %+v", res.Usage)
	}
	sawCard := false
	for _, e := range evs {
		if e.Type == agentcore.StreamCard {
			sawCard = true
		}
	}
	if !sawCard {
		t.Fatal("handler card event was not relayed")
	}
}

func TestChatUnknownRoute(t *testing.T) {
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: "mystery"}, nil
		},
		handle: func(context.Context, chatWork, agentcore.StreamSink) (ChatResult, error) {
			t.Fatal("handler must not run for an unknown route")
			return ChatResult{}, nil
		},
	}
	_, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no handler") {
		t.Fatalf("expected no-handler error, got %v", err)
	}
}

func TestChatClassifierError(t *testing.T) {
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{}, errors.New("no key")
		},
		handle: func(context.Context, chatWork, agentcore.StreamSink) (ChatResult, error) {
			return ChatResult{}, nil
		},
	}
	_, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("expected classifier error to propagate, got %v", err)
	}
}

func TestChatDataErrorReturnsPartial(t *testing.T) {
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: routeData}, nil
		},
		handle: func(context.Context, chatWork, agentcore.StreamSink) (ChatResult, error) {
			return ChatResult{RunID: "run9", Turns: 1}, errors.New("boom")
		},
	}
	res, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "x"}, nil)
	if err == nil {
		t.Fatal("expected data handler error")
	}
	if res.RunID != "run9" || res.Route != routeData {
		t.Fatalf("partial result not returned on error: %+v", res)
	}
}
