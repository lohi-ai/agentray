package agentcore

import (
	"encoding/json"
	"strings"
)

// This file is the AgentCore Lab read model: the single definition of "one step"
// of a run, and the pure fold that turns a run's recorded facts into ordered
// steps. The SAME fold powers a live-stepped (explain-mode) run and a replayed
// (historical) run, so a run reads identically both ways (the Lab's core
// invariant). It is built only from facts already captured by the loop trace —
// the request messages sent each turn, the assistant response, the tool calls,
// the advertised tools, and the real per-turn token/cost — so replay
// reconstructs only what was recorded and adds no new persistence.
//
// Secrets never appear: tool arguments are traced in {{cred:NAME}} placeholder
// form (resolution happens after the trace, at the boundary), so the fold sees
// placeholders, never literals. The fold MUST NOT resolve credentials.

// LabStepKind classifies one step of a run as the Lab presents it.
type LabStepKind string

const (
	// LabStepTurn is one agent turn: reason -> tool calls + results (the default
	// "one step" per the requirement's open question).
	LabStepTurn LabStepKind = "turn"
	// LabStepCompaction is a context-compaction step: the older span was summarized
	// and the recent tail kept. It is shown as its own step so a builder sees what
	// the summary kept and what was dropped.
	LabStepCompaction LabStepKind = "compaction"
)

// LabSkillRef is one skill advertised to the model in the system prompt (header
// only — id + name + description). Whether its body was actually loaded is
// tracked separately in LabStep.SkillsLoaded (the read_skill calls observed).
type LabSkillRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// LabToolCall is one tool invocation within a step: the model's request (name +
// placeholder-form args) paired with the result it received and the gate outcome.
type LabToolCall struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Args    string `json:"args"`   // {{cred:NAME}} placeholder form — never a literal
	Result  string `json:"result"` // the tool-result message content, if recorded
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}

// LabStep is the full per-step inspector state the Lab renders: the context that
// entered the step, the assembled system prompt broken into persona / memory /
// advertised-skills, the tools available, the tool calls and their results, the
// assistant response, and both per-step and cumulative token/cost accounting
// (the real numbers from the run, never a re-estimate).
type LabStep struct {
	Index int         `json:"index"` // 0-based position in the step list
	Turn  int         `json:"turn"`  // 1-based agent turn this step belongs to
	Kind  LabStepKind `json:"kind"`

	// Assembled context + prompt as it entered this step.
	System  string    `json:"system"`  // the full assembled system prompt
	Persona string    `json:"persona"` // the Identity (SOUL) section of the prompt
	Memory  []string  `json:"memory"`  // recalled-memory bullet lines
	Context []Message `json:"context"` // the messages sent to the model this turn

	// Skills: advertised (headers in the prompt) vs loaded (read_skill calls seen
	// up to and including this step).
	SkillsAdvertised []LabSkillRef `json:"skills_advertised"`
	SkillsLoaded     []string      `json:"skills_loaded"`

	// Tools available this turn (advertised schemas, by name) and the tool calls
	// the model made this turn paired with their results.
	Tools     []string      `json:"tools"`
	ToolCalls []LabToolCall `json:"tool_calls"`

	Response   string `json:"response"`
	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`

	// Compaction (Kind == LabStepCompaction): the summary kept in place of the
	// dropped span. Context holds the retained tail.
	Summary string `json:"summary,omitempty"`

	// Per-step and cumulative accounting (real provider numbers).
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	CostUSD     float64 `json:"cost_usd"`
	CumTokensIn  int     `json:"cum_tokens_in"`
	CumTokensOut int     `json:"cum_tokens_out"`
	CumCostUSD   float64 `json:"cum_cost_usd"`
}

// TurnRecord is one recorded LLM call within a run — the unit the fold consumes.
// A consumer maps its persisted trace rows (storage.AgentLLMCall) onto this
// neutral shape so the fold itself imports no storage.
type TurnRecord struct {
	Messages   []Message  // the request messages sent to the model this turn
	Response   string     // assistant text returned
	ToolCalls  []ToolCall // the tool calls the model requested this turn
	Tools      []string   // advertised tool names this turn
	StopReason string
	Error      string
	TokensIn   int
	TokensOut  int
	CostUSD    float64
}

// FoldSteps turns a run's ordered turn records into the Lab's step list. It
// detects a compaction by the summary message the loop folds into the history
// (a system message prefixed with summaryMarker that first appears on a given
// turn) and emits it as its own step before that turn. Everything else is one
// turn = one step. Pure: same input -> same steps, for live and replay alike.
func FoldSteps(records []TurnRecord) []LabStep {
	// A run-wide map of tool-call id -> result content, harvested from every
	// tool-role message across all turns, so a call made on turn N can be paired
	// with its result regardless of which turn's context the result was captured
	// in (results land in the next turn's request messages).
	results := map[string]string{}
	for _, r := range records {
		for _, m := range r.Messages {
			if m.Role == RoleTool && m.ToolCallID != "" {
				results[m.ToolCallID] = m.Content
			}
		}
	}

	steps := make([]LabStep, 0, len(records)+1)
	loaded := []string{}
	loadedSeen := map[string]bool{}
	lastSummary := ""
	var cumIn, cumOut int
	var cumCost float64

	for i, r := range records {
		turn := i + 1

		// Compaction detection: the most recent summary message in this turn's
		// context, if it differs from the last one we saw, means a compaction
		// happened just before this turn.
		summary := latestSummary(r.Messages)
		if summary != "" && summary != lastSummary {
			steps = append(steps, LabStep{
				Index:        len(steps),
				Turn:         turn,
				Kind:         LabStepCompaction,
				Summary:      summary,
				Context:      r.Messages,
				CumTokensIn:  cumIn,
				CumTokensOut: cumOut,
				CumCostUSD:   cumCost,
			})
			lastSummary = summary
		}

		// Accumulate read_skill loads visible at this step.
		for _, c := range r.ToolCalls {
			if c.Name == readSkillToolName {
				if id := skillIDFromArgs(c.Arguments); id != "" && !loadedSeen[id] {
					loadedSeen[id] = true
					loaded = append(loaded, id)
				}
			}
		}

		system := systemPrompt(r.Messages)
		persona, memory, advertised := parseSystemPrompt(system)

		calls := make([]LabToolCall, 0, len(r.ToolCalls))
		for _, c := range r.ToolCalls {
			calls = append(calls, LabToolCall{
				ID:      c.ID,
				Name:    c.Name,
				Args:    c.Arguments,
				Result:  results[c.ID],
				Allowed: true,
			})
		}

		cumIn += r.TokensIn
		cumOut += r.TokensOut
		cumCost += r.CostUSD

		steps = append(steps, LabStep{
			Index:            len(steps),
			Turn:             turn,
			Kind:             LabStepTurn,
			System:           system,
			Persona:          persona,
			Memory:           memory,
			Context:          r.Messages,
			SkillsAdvertised: advertised,
			SkillsLoaded:     append([]string{}, loaded...),
			Tools:            r.Tools,
			ToolCalls:        calls,
			Response:         r.Response,
			StopReason:       r.StopReason,
			Error:            r.Error,
			TokensIn:         r.TokensIn,
			TokensOut:        r.TokensOut,
			CostUSD:          r.CostUSD,
			CumTokensIn:      cumIn,
			CumTokensOut:     cumOut,
			CumCostUSD:       cumCost,
		})
	}
	return steps
}

// systemPrompt returns the leading system message's content, the assembled
// system prompt for that turn (empty when the turn carries none).
func systemPrompt(messages []Message) string {
	for _, m := range messages {
		if m.Role == RoleSystem && !strings.HasPrefix(m.Content, summaryMarker) {
			return m.Content
		}
	}
	return ""
}

// latestSummary returns the content (without the marker) of the last compaction
// summary message in a turn's context, or "" when none is present.
func latestSummary(messages []Message) string {
	out := ""
	for _, m := range messages {
		if m.Role != RoleSystem {
			continue
		}
		// A real summary (preferred) or, when summarization degraded, the elide
		// breadcrumb — either marks that a compaction happened before this turn.
		if strings.HasPrefix(m.Content, summaryMarker) {
			out = strings.TrimSpace(strings.TrimPrefix(m.Content, summaryMarker))
		} else if strings.HasPrefix(m.Content, elideMarker) {
			out = strings.TrimSpace(m.Content)
		}
	}
	return out
}

// parseSystemPrompt splits an assembled system prompt into its persona (Identity
// section), recalled-memory bullets, and advertised-skill headers. It mirrors the
// exact section markers buildSystemPrompt writes, so the Lab shows the same parts
// the loop assembled — no separate persistence needed.
func parseSystemPrompt(system string) (persona string, memory []string, skills []LabSkillRef) {
	if system == "" {
		return "", nil, nil
	}
	sections := splitSections(system)
	persona = strings.TrimSpace(sections["# Identity"])
	for _, line := range strings.Split(sections["# Recalled memory"], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			memory = append(memory, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	for _, line := range strings.Split(sections["# Available skills"], "\n") {
		line = strings.TrimSpace(line)
		if ref, ok := parseSkillLine(line); ok {
			skills = append(skills, ref)
		}
	}
	return persona, memory, skills
}

// splitSections breaks a prompt into a map keyed by its "# Header" lines.
func splitSections(system string) map[string]string {
	out := map[string]string{}
	var key string
	var b strings.Builder
	flush := func() {
		if key != "" {
			out[key] = strings.TrimRight(b.String(), "\n")
		}
		b.Reset()
	}
	for _, line := range strings.Split(system, "\n") {
		if strings.HasPrefix(line, "# ") {
			flush()
			key = strings.TrimRight(line, " ")
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	flush()
	return out
}

// parseSkillLine parses one advertised-skill bullet of the form
// "- id: <id> — <name>: <desc>" written by buildSystemPrompt.
func parseSkillLine(line string) (LabSkillRef, bool) {
	if !strings.HasPrefix(line, "- id: ") {
		return LabSkillRef{}, false
	}
	rest := strings.TrimPrefix(line, "- id: ")
	idPart, after, ok := strings.Cut(rest, " — ")
	if !ok {
		return LabSkillRef{ID: strings.TrimSpace(rest)}, true
	}
	name, desc, _ := strings.Cut(after, ": ")
	return LabSkillRef{
		ID:          strings.TrimSpace(idPart),
		Name:        strings.TrimSpace(name),
		Description: strings.TrimSpace(desc),
	}, true
}

// skillIDFromArgs pulls the "id" field from a read_skill call's JSON arguments.
func skillIDFromArgs(args string) string {
	var a struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	return strings.TrimSpace(a.ID)
}

// LabStepDiff is what changed between two adjacent steps — the explain-mode
// "what changed since the previous step" view. Counts and added items keep it
// compact; the UI renders the full state from the steps themselves.
type LabStepDiff struct {
	ContextAdded   int      `json:"context_added"`   // new messages vs the prior step
	SkillsLoaded   []string `json:"skills_loaded"`   // skills loaded since the prior step
	ToolsCalled    []string `json:"tools_called"`    // tools invoked in this step
	MemoryAdded    []string `json:"memory_added"`    // recalled-memory lines new this step
	TokensInDelta  int      `json:"tokens_in_delta"`
	TokensOutDelta int      `json:"tokens_out_delta"`
	CostDelta      float64  `json:"cost_delta"`
	Compacted      bool     `json:"compacted"` // this step is a compaction
}

// DiffStep computes the change from prev to cur. A nil prev (the first step)
// diffs against the empty state, so the opening step shows its full setup.
func DiffStep(prev, cur LabStep) LabStepDiff {
	d := LabStepDiff{
		ContextAdded:   len(cur.Context),
		TokensInDelta:  cur.TokensIn,
		TokensOutDelta: cur.TokensOut,
		CostDelta:      cur.CostUSD,
		Compacted:      cur.Kind == LabStepCompaction,
	}
	for _, c := range cur.ToolCalls {
		d.ToolsCalled = append(d.ToolsCalled, c.Name)
	}
	prevLoaded := map[string]bool{}
	prevMemory := map[string]bool{}
	prevContext := 0
	if prev.Turn != 0 || prev.Kind != "" {
		prevContext = len(prev.Context)
		for _, s := range prev.SkillsLoaded {
			prevLoaded[s] = true
		}
		for _, m := range prev.Memory {
			prevMemory[m] = true
		}
	}
	d.ContextAdded = len(cur.Context) - prevContext
	if d.ContextAdded < 0 {
		d.ContextAdded = 0 // compaction shrank the context; not an "add"
	}
	for _, s := range cur.SkillsLoaded {
		if !prevLoaded[s] {
			d.SkillsLoaded = append(d.SkillsLoaded, s)
		}
	}
	for _, m := range cur.Memory {
		if !prevMemory[m] {
			d.MemoryAdded = append(d.MemoryAdded, m)
		}
	}
	return d
}
