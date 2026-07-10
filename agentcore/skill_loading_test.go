package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestSystemPromptAdvertisesHeadersNotBodies verifies progressive disclosure:
// all enabled skill headers are listed, but no body is inlined and the loader is
// not called up front.
func TestSystemPromptAdvertisesHeadersNotBodies(t *testing.T) {
	var loaderCalls int
	faux := NewFauxProvider(AssistantText("done"))
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Definition: AgentDefinition{
			Soul:   "identity",
			Agents: "mission",
			Skills: []Skill{
				{ID: "query", Name: "query-runner", Description: "use for query reporting", Enabled: true},
				{ID: "email", Name: "emailer", Description: "use for outbound email", Enabled: true},
				{ID: "off", Name: "disabled-skill", Description: "never advertised", Enabled: false},
			},
			SkillLoader: func(_ context.Context, ids []string) (map[string]string, error) {
				loaderCalls++
				return map[string]string{"query": "steps for running safe queries"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "please help with query reporting"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if loaderCalls != 0 {
		t.Fatalf("loader must not run during perceive, got %d calls", loaderCalls)
	}
	system := faux.Recorded[0].Messages[0].Content
	// All enabled headers present; disabled one absent.
	for _, want := range []string{"id: query", "query-runner", "id: email", "emailer"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing header %q: %q", want, system)
		}
	}
	if strings.Contains(system, "disabled-skill") {
		t.Fatalf("disabled skill advertised: %q", system)
	}
	// No bodies inlined.
	if strings.Contains(system, "steps for running safe queries") {
		t.Fatalf("system prompt should not inline skill bodies: %q", system)
	}
}

// TestReadSkillRoundTrip verifies the model can pull exactly one body on demand
// via read_skill, even under the default-deny policy, and the loader is asked for
// only that skill.
func TestReadSkillRoundTrip(t *testing.T) {
	var loadedIDs []string
	faux := NewFauxProvider(
		AssistantToolCall("c1", readSkillToolName, `{"id":"query"}`),
		AssistantText("loaded and done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Definition: AgentDefinition{
			Skills: []Skill{
				{ID: "query", Name: "query-runner", Description: "use for query reporting", Enabled: true},
				{ID: "email", Name: "emailer", Description: "use for outbound email", Enabled: true},
			},
			SkillLoader: func(_ context.Context, ids []string) (map[string]string, error) {
				loadedIDs = append(loadedIDs, ids...)
				return map[string]string{
					"query": "steps for running safe queries",
					"email": "steps for sending email",
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Loader asked for exactly the one requested skill.
	if len(loadedIDs) != 1 || loadedIDs[0] != "query" {
		t.Fatalf("loaded IDs = %v, want [query]", loadedIDs)
	}
	// The tool result fed back to the model carries that one body.
	var sawBody bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.Name == readSkillToolName && strings.Contains(m.Content, "steps for running safe queries") {
			sawBody = true
		}
		if strings.Contains(m.Content, "steps for sending email") {
			t.Fatalf("unrequested skill body entered context: %q", m.Content)
		}
	}
	if !sawBody {
		t.Fatalf("read_skill body never reached the model: %+v", res.Messages)
	}
	// The call was allowed despite default-deny policy.
	for _, tr := range res.Tools {
		if tr.Tool == readSkillToolName && !tr.Allowed {
			t.Fatalf("read_skill was blocked by policy: %+v", tr)
		}
	}
}

// TestReadSkillServesPreloadedBody verifies a preloaded body is returned without
// a loader configured.
func TestReadSkillServesPreloadedBody(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", readSkillToolName, `{"id":"sql-runner"}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Definition: AgentDefinition{
			Skills: []Skill{{Name: "sql-runner", Description: "sql helper", Body: "preloaded body", Enabled: true}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "need sql help")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var sawBody bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && strings.Contains(m.Content, "preloaded body") {
			sawBody = true
		}
	}
	if !sawBody {
		t.Fatalf("preloaded body not returned by read_skill: %+v", res.Messages)
	}
}
