package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	goalplugin "github.com/lohi-ai/agentray/agentcore/plugins/goal"
)

// These tests exercise native turn admission and command handling without a database.

func collect(sink *[]agentcore.StreamEvent) agentcore.StreamSink {
	return func(ev agentcore.StreamEvent) { *sink = append(*sink, ev) }
}

func TestChatRoutesToData(t *testing.T) {
	card := &agentcore.ResultCard{Title: "Signups", Kind: "stat"}
	handled := false
	svc := &ChatService{

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
	// Native run usage is forwarded unchanged.
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 50 {
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

// goalForMessage is the two-step the chat turn does: parse, then resolve a /goal
// directive into (condition, prompt). Anything that is not a goal directive keeps
// the message verbatim — the property the table below is really asserting.
func goalForMessage(message string) (goal, prompt string) {
	d := parseDirective(message)
	if d.Name != CmdGoal || d.Arg == "" {
		return "", message
	}
	return goalTurn(d)
}

func TestParseGoalDirective(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		wantGoal string
		wantRest string
	}{
		{"condition and task", "/goal all tests pass\nfix the flaky suite", "all tests pass", "fix the flaky suite"},
		{"quoted condition", `/goal "STATUS report published"` + "\ndo it", "STATUS report published", "do it"},
		{"condition only", "/goal churn dashboard exists", "churn dashboard exists", "Work toward this goal until it is satisfied: churn dashboard exists"},
		{"bare directive", "/goal", "", "/goal"},
		{"different word", "/goals for this quarter?", "", "/goals for this quarter?"},
		{"plain message", "how many signups?", "", "how many signups?"},
		{"slash mid-prose", "what is the q1/q2 split?", "", "what is the q1/q2 split?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goal, rest := goalForMessage(tc.message)
			if goal != tc.wantGoal || rest != tc.wantRest {
				t.Fatalf("goalForMessage(%q) = (%q,%q), want (%q,%q)", tc.message, goal, rest, tc.wantGoal, tc.wantRest)
			}
		})
	}
}

// A handled command must never reach the agent: the whole
// reason it exists is to answer without a run, and a model call here would be
// both a bill and a chance for the agent to do something else instead.
func TestChatHandledCommandsNeverRun(t *testing.T) {
	for _, msg := range []string{"/help", "/plan", "/compact", "/clear", "/agents"} {
		t.Run(msg, func(t *testing.T) {
			svc := &ChatService{

				handle: func(context.Context, chatWork, agentcore.StreamSink) (ChatResult, error) {
					t.Fatalf("agent ran for %q", msg)
					return ChatResult{}, nil
				},
			}
			res, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: msg}, nil)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Route != routeCommand {
				t.Fatalf("route = %q, want %q", res.Route, routeCommand)
			}
			if strings.TrimSpace(res.Final) == "" {
				t.Fatal("a command must answer with something")
			}
			if res.RunID != "" {
				t.Fatalf("command opened a run: %q", res.RunID)
			}
		})
	}
}

// The catalog is what the client's `/` menu renders. Every entry has to be
// parseable back into the command it advertises, or the menu offers something the
// server treats as prose.
func TestChatCommandCatalogRoundTrips(t *testing.T) {
	for _, c := range ChatCommands {
		typed := "/" + c.Name
		if c.Arg != "" {
			typed += " something"
		}
		if d := parseDirective(typed); d.Name != c.Name {
			t.Fatalf("catalog entry %q parsed as %q", c.Name, d.Name)
		}
	}
}

func TestChatGoalDirectiveConfiguresNativeRun(t *testing.T) {
	var got chatWork
	svc := &ChatService{

		handle: func(_ context.Context, req chatWork, _ agentcore.StreamSink) (ChatResult, error) {
			got = req
			return ChatResult{RunID: "run1", Final: "done.\nSTATUS: DONE"}, nil
		},
	}
	res, err := svc.Chat(context.Background(), ChatOptions{ProjectID: "p", Message: "/goal weekly report saved\nwrite the weekly report"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Goal != "weekly report saved" {
		t.Fatalf("goal not threaded to the handler: %+v", got)
	}
	if got.Message != "write the weekly report" {
		t.Fatalf("directive not stripped from the prompt: %q", got.Message)
	}
	if res.Route != routeData {
		t.Fatalf("route = %q, want data", res.Route)
	}
}

func TestChatDataErrorReturnsPartial(t *testing.T) {
	svc := &ChatService{

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

// A handled command must be recognizable to the HTTP layer BEFORE it decides
// whether a message is an amendment to a live run. /goal is deliberately not
// handled — it configures a run, so mid-run it is a genuine redirect.
func TestIsHandledCommand(t *testing.T) {
	cases := map[string]bool{
		"/compact":                  true,
		"/clear":                    true,
		"/plan":                     true,
		"/help":                     true,
		"/agents":                   true,
		"/goal all tests pass":      false,
		"/goals for the quarter":    false,
		"how is the q1/q2 split?":   false,
		"compact the thread please": false,
	}
	for msg, want := range cases {
		if got := IsHandledCommand(msg); got != want {
			t.Fatalf("IsHandledCommand(%q) = %v, want %v", msg, got, want)
		}
	}
}

// A command reply is markdown. Word-streaming that flattened newlines showed the
// user one unreadable paragraph until `done` swapped in the real text — a visible
// flicker on a reply that is supposed to be instant.
func TestStreamTextKeepsLineShape(t *testing.T) {
	const text = "Here's what you can type.\n\n**Commands**\n\n- `/plan` — show the plan\n"
	var got strings.Builder
	streamText(text, func(ev agentcore.StreamEvent) {
		if ev.Type == agentcore.StreamToken {
			got.WriteString(ev.Token)
		}
	})
	if got.String() != text {
		t.Fatalf("streamed text lost its shape:\n got %q\nwant %q", got.String(), text)
	}
}

// The gate's status line is a protocol between the loop and the plugin. Shown to
// a reader it is noise; persisted, it comes back on reload and is replayed to the
// model as its own prior words.
func TestStripGoalSentinel(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"done", "Here are the numbers.\n\nSTATUS: DONE", "Here are the numbers."},
		{"blocked", "I need a Stripe key to finish.\n\nSTATUS: BLOCKED", "I need a Stripe key to finish."},
		{"decorated", "All set.\n\n**STATUS: DONE**", "All set."},
		{"trailing blank lines", "All set.\nSTATUS: DONE\n\n", "All set."},
		{"sentence containing the sentinel is kept", "I can't write STATUS: DONE yet — two tests fail.", "I can't write STATUS: DONE yet — two tests fail."},
		{"sentinel with a trailing note is kept", "Done.\n\nSTATUS: DONE (all 12 checks green)", "Done.\n\nSTATUS: DONE (all 12 checks green)"},
		{"ungated answer", "Signups are up 12%.", "Signups are up 12%."},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goalplugin.PublicText(tc.in); got != tc.want {
				t.Fatalf("stripGoalSentinel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGreetingUsesTheSameNativeRunAsOtherQuestions(t *testing.T) {
	calls := 0
	svc := &ChatService{handle: func(_ context.Context, work chatWork, sink agentcore.StreamSink) (ChatResult, error) {
		calls++
		if work.Message != "hi" {
			t.Fatal(work.Message)
		}
		streamText("Hi there", sink)
		return ChatResult{RunID: "native-greeting", Final: "Hi there", Usage: agentcore.Usage{InputTokens: 3}}, nil
	}}
	var events []agentcore.StreamEvent
	result, err := svc.Chat(context.Background(), ChatOptions{Message: "hi"}, collect(&events))
	if err != nil || calls != 1 || result.RunID != "native-greeting" || result.Route != routeData || result.Usage.InputTokens != 3 {
		t.Fatalf("greeting escaped native execution: %+v %v", result, err)
	}
	var text strings.Builder
	for _, event := range events {
		if event.Type == agentcore.StreamToken {
			text.WriteString(event.Token)
		}
	}
	if strings.TrimSpace(text.String()) != "Hi there" {
		t.Fatalf("stream: %q", text.String())
	}
}
