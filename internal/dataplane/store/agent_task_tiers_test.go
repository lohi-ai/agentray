package storage

import "testing"

func TestDefaultTaskTiers(t *testing.T) {
	// The default map must reproduce today's behavior: triage/compaction on the
	// cheap rung, the loop on flash, reflection on pro.
	d := DefaultTaskTiers()
	want := map[string]string{
		TaskTriage:     "lite",
		TaskRun:        "flash",
		TaskCompaction: "lite",
		TaskReflection: "pro",
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("default %s = %q, want %q", k, d[k], v)
		}
	}
}

func TestAgentTaskTiersMerge(t *testing.T) {
	// A partial stored map overlays the defaults; the unset kinds still resolve.
	got := AgentTaskTiers{TaskRun: "pro"}.merge()
	if got[TaskRun] != "pro" {
		t.Errorf("run = %q, want pro (stored override)", got[TaskRun])
	}
	if got[TaskCompaction] != "lite" {
		t.Errorf("compaction = %q, want lite (default)", got[TaskCompaction])
	}
	if len(got) != 4 {
		t.Errorf("merged map has %d kinds, want 4", len(got))
	}
}

func TestAgentTaskTiersMergeRejectsGarbage(t *testing.T) {
	// Unknown kinds and unknown tier values are dropped, falling back to defaults.
	got := AgentTaskTiers{
		"bogus_kind":   "lite",   // unknown kind
		TaskTriage:     "ultra",  // unknown tier value
		TaskReflection: "flash",  // valid override
	}.merge()
	if _, ok := got["bogus_kind"]; ok {
		t.Error("unknown kind must not survive merge")
	}
	if got[TaskTriage] != "lite" {
		t.Errorf("triage = %q, want lite (garbage value rejected → default)", got[TaskTriage])
	}
	if got[TaskReflection] != "flash" {
		t.Errorf("reflection = %q, want flash (valid override)", got[TaskReflection])
	}
}

func TestAgentTaskTiersMergeEmpty(t *testing.T) {
	// An absent row (empty map) resolves exactly to the default map.
	got := AgentTaskTiers{}.merge()
	for k, v := range DefaultTaskTiers() {
		if got[k] != v {
			t.Errorf("empty.merge()[%s] = %q, want %q", k, got[k], v)
		}
	}
}
