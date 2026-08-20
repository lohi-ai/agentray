package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// scopedMemory is an in-memory MemoryStore that files entries by the scope they
// were written under and recalls from exactly that scope — the same contract
// PgMemory implements against Postgres, minus the database.
type scopedMemory struct {
	byScope map[string][]agentcore.MemoryEntry
}

func newScopedMemory() *scopedMemory {
	return &scopedMemory{byScope: map[string][]agentcore.MemoryEntry{}}
}

func (m *scopedMemory) Remember(_ context.Context, e agentcore.MemoryEntry) error {
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

// TestReflectWritesAgentScope pins the write scope of the reflection pass.
//
// Recall reads the agent's scope (agentcore/loop.go hands def.ScopeID to
// Recall). The pass used to write under the project id, so for every agent
// whose scope is not the project — i.e. every non-default agent — reflection
// was write-only: it filed what it learned somewhere the agent never reads.
// The two ids coincide for the default agent, which is why nothing caught it.
func TestReflectWritesAgentScope(t *testing.T) {
	const (
		projectID = "11111111-1111-1111-1111-111111111111"
		agentA    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		agentB    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	mem := newScopedMemory()
	in := reflectInput{ProjectID: projectID, ScopeID: agentA, RunID: "run-1", Memory: mem}

	in.applyMemories(context.Background(), []reflectMemory{
		{Kind: "learning", Content: "the export tool is blocked; use run_sql instead"},
		{Kind: "", Content: "  "}, // blank content is still skipped
	})

	got, err := mem.Recall(context.Background(), agentA, "export", 8)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent A recalled %d memories from its own scope, want 1 — reflection wrote where the agent cannot read", len(got))
	}
	if got[0].Confidence != 0.6 || got[0].Kind != agentcore.MemoryLearning {
		t.Errorf("entry = kind %q confidence %v, want learning/0.6", got[0].Kind, got[0].Confidence)
	}

	// A sibling agent in the same project must not inherit it: memory is
	// agent-private, which is the whole point of keying on the agent scope.
	if sib, _ := mem.Recall(context.Background(), agentB, "export", 8); len(sib) != 0 {
		t.Errorf("sibling agent recalled %d of agent A's memories, want 0", len(sib))
	}
	// And nothing lands under the bare project id any more.
	if proj := mem.byScope[projectID]; len(proj) != 0 {
		t.Errorf("%d memories filed under the project scope, want 0", len(proj))
	}
}

// TestReflectDefaultAgentUnchanged: for the default agent the scope id IS the
// project id, so the fix must be byte-for-byte invisible there.
func TestReflectDefaultAgentUnchanged(t *testing.T) {
	const projectID = "11111111-1111-1111-1111-111111111111"
	mem := newScopedMemory()
	in := reflectInput{ProjectID: projectID, ScopeID: projectID, RunID: "run-1", Memory: mem}

	in.applyMemories(context.Background(), []reflectMemory{{Kind: "fact", Content: "wau is up 12%"}})

	got, _ := mem.Recall(context.Background(), projectID, "wau", 8)
	if len(got) != 1 || got[0].Content != "wau is up 12%" {
		t.Fatalf("default agent recalled %v, want its own single memory", got)
	}
}

// scopedSkills is an in-memory skillProposer that files proposals by the scope
// they were written under. loadSkills mirrors Runner.loadSkills /
// ActiveSkillHeadersForScope: only enabled, active headers for that exact
// scope — the same contract that made a project-scoped proposal invisible to
// the non-default agent that authored it.
type scopedSkills struct {
	byScope map[string][]storage.AgentSkill
}

func newScopedSkills() *scopedSkills {
	return &scopedSkills{byScope: map[string][]storage.AgentSkill{}}
}

func (s *scopedSkills) ProposeAgentSkill(_ context.Context, scopeID string, sk storage.AgentSkill) error {
	sk.Status = "proposed"
	sk.Enabled = false
	s.byScope[scopeID] = append(s.byScope[scopeID], sk)
	return nil
}

func (s *scopedSkills) approve(scopeID string) {
	for i, sk := range s.byScope[scopeID] {
		if sk.Status == "proposed" {
			sk.Status = "active"
			sk.Enabled = true
			s.byScope[scopeID][i] = sk
		}
	}
}

func (s *scopedSkills) loadSkills(scopeID string) []agentcore.Skill {
	out := []agentcore.Skill{}
	for _, sk := range s.byScope[scopeID] {
		if sk.Status == "active" && sk.Enabled {
			out = append(out, agentcore.Skill{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled})
		}
	}
	return out
}

var _ skillProposer = (*scopedSkills)(nil)

// TestReflectProposesSkillOnAgentScope pins the write scope of a reflect-
// proposed skill. loadSkills reads the agent's scope, so a proposal filed
// under the project was invisible to the agent that wrote it — even after
// the owner approved it, because ApproveAgentSkill is already agent-scoped.
// A sibling agent in the same project must not inherit it.
func TestReflectProposesSkillOnAgentScope(t *testing.T) {
	const (
		projectID = "11111111-1111-1111-1111-111111111111"
		agentA    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		agentB    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	skills := newScopedSkills()
	in := reflectInput{ProjectID: projectID, ScopeID: agentA}

	in.applySkills(context.Background(), skills, []reflectSkill{
		{Name: "avoid-export", Description: "use run_sql", Body: "Never call export; it is blocked."},
		{Name: "  ", Body: "skipped: blank name"},
		{Name: "no-body", Body: "  "},
	})

	if got := skills.byScope[agentA]; len(got) != 1 || got[0].Name != "avoid-export" || got[0].Status != "proposed" {
		t.Fatalf("agent A proposals = %+v, want one proposed skill", got)
	}
	if proj := skills.byScope[projectID]; len(proj) != 0 {
		t.Errorf("%d skills filed under the project scope, want 0", len(proj))
	}

	// Approval is already agent-scoped (ApproveAgentSkill keys on agentScope).
	// After the owner approves, the same agent must load the skill back, and a
	// sibling must not.
	skills.approve(agentA)
	got := skills.loadSkills(agentA)
	if len(got) != 1 || got[0].Name != "avoid-export" {
		t.Fatalf("agent A loaded %v after approval, want the skill it proposed", got)
	}
	if sib := skills.loadSkills(agentB); len(sib) != 0 {
		t.Errorf("sibling agent loaded %d of agent A's skills, want 0", len(sib))
	}
	if proj := skills.loadSkills(projectID); len(proj) != 0 {
		t.Errorf("default/project scope loaded %d of agent A's skills, want 0", len(proj))
	}
}

// TestReflectDefaultAgentSkillUnchanged: for the default agent the scope id
// IS the project id, so the fix must be byte-for-byte invisible there.
func TestReflectDefaultAgentSkillUnchanged(t *testing.T) {
	const projectID = "11111111-1111-1111-1111-111111111111"
	skills := newScopedSkills()
	in := reflectInput{ProjectID: projectID, ScopeID: projectID}

	in.applySkills(context.Background(), skills, []reflectSkill{
		{Name: "wau-trend", Description: "wau", Body: "WAU is the weekly active users chart."},
	})
	skills.approve(projectID)

	got := skills.loadSkills(projectID)
	if len(got) != 1 || got[0].Name != "wau-trend" {
		t.Fatalf("default agent loaded %v, want its own single skill", got)
	}
}
