package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestParseMagicKeywords(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		fires    bool
		stripped string
	}{
		// Fires: standalone lowercase word in prose.
		{"bare", "ultrathink", true, ""},
		{"leading", "ultrathink please check the funnel", true, "please check the funnel"},
		{"trailing", "look deeper ultrathink", true, "look deeper"},
		{"middle", "please ultrathink this funnel", true, "please this funnel"},
		{"sentence end", "think hard. ultrathink.", true, "think hard."},
		{"punctuation", "ultrathink, then answer", true, "then answer"},
		{"quoted", `try "ultrathink" here`, true, `try "" here`},
		{"after colon", "note: ultrathink", true, "note:"},
		{"multiline prose", "first line\nultrathink\nthird", true, "first line\n\nthird"},

		// Never fires: code, markup, identifiers, paths.
		{"inline code", "use `ultrathink` literally", false, ""},
		{"fenced block", "before\n```\nultrathink\n```\nafter", false, ""},
		{"tilde fence", "~~~\nultrathink\n~~~", false, ""},
		{"unclosed fence", "```\nultrathink", false, ""},
		{"xml tag content", "<prompt>ultrathink</prompt>", false, ""},
		{"html comment", "<!-- ultrathink -->", false, ""},
		{"identifier suffix", "ultrathinking", false, ""},
		{"identifier prefix", "preultrathink", false, ""},
		{"capitalized", "Ultrathink this", false, ""},
		{"file name", "open ultrathink.ts", false, ""},
		{"path right", "see ultrathink/run.go", false, ""},
		{"path left", "see modes/ultrathink", false, ""},
		{"symbol ref", "call foo::ultrathink", false, ""},
		{"call", "run ultrathink()", false, ""},
		{"hyphenated", "an ultrathink-mode flag", false, ""},
		{"underscored", "my_ultrathink_var", false, ""},
		{"no keyword", "just a normal question", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stripped, fired := parseMagicKeywords(tc.message)
			if tc.fires {
				if len(fired) != 1 || fired[0].Word != "ultrathink" {
					t.Fatalf("expected ultrathink to fire, got %+v", fired)
				}
				if stripped != tc.stripped {
					t.Fatalf("stripped = %q, want %q", stripped, tc.stripped)
				}
			} else {
				if len(fired) != 0 {
					t.Fatalf("unexpected fire: %+v", fired)
				}
				if stripped != tc.message {
					t.Fatalf("message changed: %q → %q", tc.message, stripped)
				}
			}
		})
	}
}

func TestParseMagicKeywordsDedupesButStripsAll(t *testing.T) {
	stripped, fired := parseMagicKeywords("ultrathink and ultrathink again")
	if len(fired) != 1 {
		t.Fatalf("keyword should fire once, got %+v", fired)
	}
	if stripped != "and again" {
		t.Fatalf("every occurrence must be stripped, got %q", stripped)
	}
}

func TestChatMagicKeywordSetsEffortAndStrips(t *testing.T) {
	var got chatWork
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: routeData}, nil
		},
		handle: func(_ context.Context, w chatWork, _ agentcore.StreamSink) (ChatResult, error) {
			got = w
			return ChatResult{Final: "done"}, nil
		},
	}
	_, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "ultrathink why did signups drop?"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ReasoningEffort != "high" {
		t.Fatalf("effort = %q, want high", got.ReasoningEffort)
	}
	if got.Message != "why did signups drop?" {
		t.Fatalf("keyword not stripped from forwarded message: %q", got.Message)
	}
}

func TestChatMagicKeywordInCodeDoesNotFire(t *testing.T) {
	var got chatWork
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			return chatDecision{Route: routeData}, nil
		},
		handle: func(_ context.Context, w chatWork, _ agentcore.StreamSink) (ChatResult, error) {
			got = w
			return ChatResult{Final: "done"}, nil
		},
	}
	msg := "what does `ultrathink` do in omp?"
	_, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: msg}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ReasoningEffort != "" {
		t.Fatalf("effort = %q, want empty", got.ReasoningEffort)
	}
	if got.Message != msg {
		t.Fatalf("message changed: %q", got.Message)
	}
}

func TestChatMagicKeywordInsideGoalArg(t *testing.T) {
	var got chatWork
	svc := &ChatService{
		classify: func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
			t.Fatal("classifier must not run for a /goal turn")
			return chatDecision{}, nil
		},
		handle: func(_ context.Context, w chatWork, _ agentcore.StreamSink) (ChatResult, error) {
			got = w
			return ChatResult{Final: "done"}, nil
		},
	}
	_, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "/goal ultrathink all tests pass"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Goal == "" {
		t.Fatal("goal gate lost")
	}
	if got.ReasoningEffort != "high" {
		t.Fatalf("effort = %q, want high — keywords apply to gated turns too", got.ReasoningEffort)
	}
	if strings.Contains(got.Message, "ultrathink") {
		t.Fatalf("keyword leaked into prompt: %q", got.Message)
	}
}

func TestKeywordEntrySkippedByFoldHistory(t *testing.T) {
	payload := `{"keywords":["ultrathink"]}`
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "ultrathink why?"),
		{ID: "2", Kind: ConvKindKeyword, PayloadJSON: payload},
		msgEntry("3", "assistant", "because"),
	}
	history := foldHistory(entries)
	if len(history) != 2 {
		t.Fatalf("keyword entry must not enter model context, got %+v", history)
	}
}
