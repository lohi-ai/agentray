package integration

// End-to-end proof of the memory-curation contract through the real loop:
// the model calls learn / memory_edit, the calls pass the permission gate,
// the store applies them under the run's scope, and the NEXT run's recalled
// block shows the result — including the entry ids the edit tool needs.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/memory"
)

// memStore is an in-memory MemoryStore + MemoryCurator with the shipped
// store's semantics: supersede keeps the row and filters it from recall.
type memStore struct {
	entries []agentcore.MemoryEntry
	live    map[string]bool // id -> not superseded
	next    int
}

func newMemStore() *memStore { return &memStore{live: map[string]bool{}} }

func (s *memStore) Remember(_ context.Context, e agentcore.MemoryEntry) error {
	s.next++
	e.ID = fmt.Sprintf("mem-%d", s.next)
	s.entries = append(s.entries, e)
	s.live[e.ID] = true
	return nil
}

func (s *memStore) Recall(_ context.Context, scopeID, _ string, limit int) ([]agentcore.MemoryEntry, error) {
	var out []agentcore.MemoryEntry
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.entries[i]
		if e.ScopeID == scopeID && s.live[e.ID] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memStore) Supersede(_ context.Context, scopeID, id, _ string) error {
	for _, e := range s.entries {
		if e.ID == id && e.ScopeID == scopeID && s.live[id] {
			s.live[id] = false
			return nil
		}
	}
	return fmt.Errorf("agent memory not found in scope")
}

func (s *memStore) Update(_ context.Context, scopeID, id string, e agentcore.MemoryEntry) error {
	for _, old := range s.entries {
		if old.ID == id && old.ScopeID == scopeID && s.live[id] {
			s.next++
			e.ID = fmt.Sprintf("mem-%d", s.next)
			e.ScopeID = scopeID
			e.Kind = old.Kind // the successor inherits the kind
			s.entries = append(s.entries, e)
			s.live[e.ID] = true
			s.live[id] = false
			return nil
		}
	}
	return fmt.Errorf("agent memory not found in scope")
}

func (s *memStore) CreateSession(context.Context, string) (agentcore.Session, error) {
	return agentcore.Session{}, nil
}
func (s *memStore) SaveSession(context.Context, agentcore.Session) error { return nil }
func (s *memStore) Fork(context.Context, string) (agentcore.Session, error) {
	return agentcore.Session{}, nil
}

var (
	_ agentcore.MemoryStore   = (*memStore)(nil)
	_ agentcore.MemoryCurator = (*memStore)(nil)
)

func buildMemoryAgent(t *testing.T, store agentcore.MemoryStore, pol agentcore.Policy, provider *agentcore.FauxProvider) *agentcore.Agent {
	t.Helper()
	a, err := agentcore.Build(
		agentcore.ModelPlugin{Provider: provider, Model: "faux"},
		agentcore.DefinitionPlugin{Definition: agentcore.AgentDefinition{ScopeID: "agent-1"}},
		agentcore.PolicyPlugin{Policy: pol},
		memory.Plugin{Store: store},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return a
}

// The primary journey: learn files a lesson, the next run recalls it WITH its
// id, memory_edit updates then forgets it, and a third run's recall shows only
// the superseding wording — then nothing.
func TestMemoryCurationEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	allow := agentcore.NewAllowList(memory.ToolLearn, memory.ToolMemoryEdit)

	// Run 1: the model learns a lesson.
	p1 := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", "learn", `{"lesson":"deploys moved to Thursday","tags":["deploy"]}`),
		agentcore.AssistantText("noted"),
	)
	if _, err := buildMemoryAgent(t, store, allow, p1).Prompt(ctx, "remember this"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(store.entries) != 1 || store.entries[0].Kind != agentcore.MemoryLearning || store.entries[0].ScopeID != "agent-1" {
		t.Fatalf("learn wrote %+v", store.entries)
	}
	learnedID := store.entries[0].ID

	// Run 2: recall shows the lesson with its id; the model updates it, then
	// forgets the successor, then tries a second forget on the retracted id.
	p2 := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", "memory_edit", fmt.Sprintf(`{"id":%q,"action":"update","content":"deploys moved to Wednesday"}`, learnedID)),
		agentcore.AssistantToolCall("c2", "memory_edit", `{"id":"mem-2","action":"forget"}`),
		agentcore.AssistantToolCall("c3", "memory_edit", `{"id":"mem-2","action":"forget"}`),
		agentcore.AssistantText("done"),
	)
	res, err := buildMemoryAgent(t, store, allow, p2).Prompt(ctx, "fix that memory")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// The recalled block carried the id — the model could only have passed it
	// back if the prompt rendered it.
	sys := p2.Recorded[0].Messages[0].Content
	if !strings.Contains(sys, "id "+learnedID) {
		t.Fatalf("recalled block did not render the entry id:\n%s", sys)
	}

	// update produced a live successor; forget retracted it; the second forget
	// was refused by the store (already retracted).
	var updateOK, forgetOK, reforgetRefused bool
	for _, tr := range res.Tools {
		switch tr.CallID {
		case "c1":
			updateOK = tr.Allowed && tr.Error == ""
		case "c2":
			forgetOK = tr.Allowed && tr.Error == ""
		case "c3":
			reforgetRefused = tr.Error != "" || strings.Contains(tr.Error, "not found")
		}
	}
	if !updateOK || !forgetOK {
		t.Fatalf("update/forget did not execute: %+v", res.Tools)
	}
	if !reforgetRefused {
		t.Error("re-forgetting a retracted memory was not refused")
	}
	if len(store.entries) != 2 {
		t.Fatalf("update should keep the old row as history, got %d rows", len(store.entries))
	}

	// Run 3: recall is empty — every row is retracted.
	p3 := agentcore.NewFauxProvider(agentcore.AssistantText("ok"))
	if _, err := buildMemoryAgent(t, store, allow, p3).Prompt(ctx, "what do you know"); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if strings.Contains(p3.Recorded[0].Messages[0].Content, "deploys moved") {
		t.Error("a retracted memory still appears in recall")
	}
}

// A run whose policy does not permit the tools never sees them advertised,
// and a direct call is blocked by the gate.
func TestMemoryCurationGatedByPolicy(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	p := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", "learn", `{"lesson":"x"}`),
		agentcore.AssistantText("done"),
	)
	res, err := buildMemoryAgent(t, store, agentcore.NewAllowList("run_sql"), p).Prompt(ctx, "learn something")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, s := range p.Recorded[0].Tools {
		if s.Name == memory.ToolLearn || s.Name == memory.ToolMemoryEdit {
			t.Errorf("%q advertised to a run whose policy denies it", s.Name)
		}
	}
	if len(res.Tools) != 1 || res.Tools[0].Allowed {
		t.Fatalf("a denied learn call was not blocked: %+v", res.Tools)
	}
	if len(store.entries) != 0 {
		t.Error("a blocked learn call still wrote memory")
	}
}

// A run with no memory store gets neither tool — the plugin declines.
func TestMemoryCurationAbsentWithoutStore(t *testing.T) {
	ctx := context.Background()
	p := agentcore.NewFauxProvider(agentcore.AssistantText("ok"))
	res, err := buildMemoryAgent(t, nil, agentcore.NewAllowList(memory.ToolLearn, memory.ToolMemoryEdit), p).Prompt(ctx, "hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = res
	for _, s := range p.Recorded[0].Tools {
		if s.Name == memory.ToolLearn || s.Name == memory.ToolMemoryEdit {
			t.Errorf("%q advertised on a memoryless run", s.Name)
		}
	}
}
