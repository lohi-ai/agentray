package channels

import (
	"slices"
	"testing"
)

func TestBuiltinKindsRegistered(t *testing.T) {
	want := []Kind{KindChat, KindMCP, KindSchedule, KindWebhook, KindLab}
	for _, k := range want {
		info, ok := Lookup(k)
		if !ok {
			t.Errorf("missing built-in channel %q", k)
			continue
		}
		if info.Reserved {
			t.Errorf("%q must not be reserved — it has a shipped adapter", k)
		}
		if info.Description == "" {
			t.Errorf("%q has empty description", k)
		}
	}
}

func TestReservedKindsAreNotDispatchable(t *testing.T) {
	for _, k := range []Kind{KindSupportWidget, KindVoice} {
		info, ok := Lookup(k)
		if !ok {
			t.Fatalf("reserved kind %q must be in the catalog so callers can plan against it", k)
		}
		if !info.Reserved {
			t.Errorf("%q should be reserved until an adapter ships", k)
		}
		if _, err := NewEnvelope(k, "proj", "", "hi"); err == nil {
			t.Errorf("NewEnvelope(%q) must refuse a reserved kind", k)
		}
	}
}

func TestNewEnvelopeUnknownKind(t *testing.T) {
	if _, err := NewEnvelope(Kind("sms"), "proj", "", "hi"); err == nil {
		t.Fatal("expected error for unregistered kind")
	}
}

func TestNewEnvelopeRequiresProject(t *testing.T) {
	if _, err := NewEnvelope(KindWebhook, "", "agent", "hi"); err == nil {
		t.Fatal("webhook envelope without project_id must fail")
	}
	env, err := NewEnvelope(KindWebhook, "proj", "agent", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != KindWebhook || env.ProjectID != "proj" || env.Body != "hi" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestKindsSortedAndIncludeBuiltins(t *testing.T) {
	got := Kinds()
	if !slices.Contains(got, string(KindChat)) || !slices.Contains(got, string(KindWebhook)) {
		t.Fatalf("Kinds() missing builtins: %v", got)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("Kinds() not sorted: %v", got)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register must panic")
		}
	}()
	Register(Info{Kind: KindChat, Description: "dup"})
}
