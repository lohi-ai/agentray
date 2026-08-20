package usecase

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// scopedMemory files entries by the scope they were written under and recalls
// from exactly that scope — the contract PgMemory implements against Postgres,
// minus the database.
type scopedMemory struct {
	byScope map[string][]agentcore.MemoryEntry
}

func (m *scopedMemory) Remember(_ context.Context, e agentcore.MemoryEntry) error {
	if m.byScope == nil {
		m.byScope = map[string][]agentcore.MemoryEntry{}
	}
	m.byScope[e.ScopeID] = append(m.byScope[e.ScopeID], e)
	return nil
}

func (m *scopedMemory) Recall(_ context.Context, scopeID, _ string, limit int) ([]agentcore.MemoryEntry, error) {
	got := m.byScope[scopeID]
	if limit > 0 && limit < len(got) {
		got = got[:limit]
	}
	return got, nil
}

func (m *scopedMemory) CreateSession(context.Context, string) (agentcore.Session, error) {
	return agentcore.Session{}, nil
}
func (m *scopedMemory) SaveSession(context.Context, agentcore.Session) error { return nil }
func (m *scopedMemory) Fork(context.Context, string) (agentcore.Session, error) {
	return agentcore.Session{}, nil
}

var _ agentcore.MemoryStore = (*scopedMemory)(nil)

const (
	scopeTestProject = "11111111-1111-1111-1111-111111111111"
	scopeTestAgentA  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	scopeTestAgentB  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func invokeRemember(t *testing.T, cc opcore.CallContext, args string) {
	t.Helper()
	op, ok := Registry().Get("remember")
	if !ok {
		t.Fatal("remember operation is not registered")
	}
	if _, err := op.OpInvoke(context.Background(), cc, args); err != nil {
		t.Fatalf("remember invoke: %v", err)
	}
}

// TestRememberWritesAgentScope pins the write scope of the remember tool.
//
// Recall reads the agent's scope (agentcore/loop.go hands def.ScopeID to
// Recall); the tool used to write under cc.ProjectID. For every agent whose
// scope is not the project — i.e. every non-default agent — that made the tool
// write-only: it filed the fact under the project and looked for it under
// itself, so nothing it chose to remember ever came back.
func TestRememberWritesAgentScope(t *testing.T) {
	mem := &scopedMemory{}
	cc := opcore.CallContext{
		ProjectID: scopeTestProject, ScopeID: scopeTestAgentA, RunID: "run-1",
		Deps: &Deps{Memory: mem},
	}
	invokeRemember(t, cc, `{"kind":"fact","content":"checkout drops off at the address step","tags":["funnel"]}`)

	got, err := mem.Recall(context.Background(), scopeTestAgentA, "checkout", 8)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent A recalled %d memories from its own scope, want 1 — remember wrote where the agent cannot read", len(got))
	}
	if got[0].Kind != agentcore.MemoryFact || got[0].Confidence != 0.7 || got[0].SourceRun != "run-1" {
		t.Errorf("entry = %+v, want a fact at 0.7 sourced from run-1", got[0])
	}

	// A sibling agent in the same project must not inherit it.
	if sib, _ := mem.Recall(context.Background(), scopeTestAgentB, "checkout", 8); len(sib) != 0 {
		t.Errorf("sibling agent recalled %d of agent A's memories, want 0", len(sib))
	}
	if proj := mem.byScope[scopeTestProject]; len(proj) != 0 {
		t.Errorf("%d memories filed under the bare project scope, want 0", len(proj))
	}
}

// TestRememberDefaultAgentAndProjectCallersUnchanged covers the two callers for
// which nothing may change: the default agent (scope id == project id) and the
// REST/CLI/MCP edges, which carry no agent scope at all and must keep writing
// to the project.
func TestRememberDefaultAgentAndProjectCallersUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		cc   opcore.CallContext
	}{
		{"default agent", opcore.CallContext{ProjectID: scopeTestProject, ScopeID: scopeTestProject}},
		{"no agent scope (REST/CLI/MCP)", opcore.CallContext{ProjectID: scopeTestProject}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &scopedMemory{}
			cc := tc.cc
			cc.Deps = &Deps{Memory: mem}
			invokeRemember(t, cc, `{"content":"wau is up 12%"}`)

			got, _ := mem.Recall(context.Background(), scopeTestProject, "wau", 8)
			if len(got) != 1 || got[0].Content != "wau is up 12%" {
				t.Fatalf("recalled %v from the project scope, want the single memory just written", got)
			}
		})
	}
}
