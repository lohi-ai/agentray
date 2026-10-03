package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSourcePolicyValidationAndBinding(t *testing.T) {
	p := &SourcePolicy{Version: 1,
		Destinations: []SourceDestination{{Host: "db.example", Port: 5432, AllowedIPCIDRs: []string{"10.20.0.0/24"}}},
		Bindings: []SourceBinding{{ProjectID: "p", ConnectorID: "c", Schema: "exports", Relation: "orders_v1", RelationKind: RelationKindView,
			Columns: []SourcePolicyColumn{{Name: "id", PGType: "bigint"}, {Name: "amount", PGType: "numeric"}}, KeyColumn: "id",
			KeyStability: KeyStabilityImmutableUnique, AllowedCursorColumns: []string{"id"}}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	b, err := p.Binding("p", "c")
	if err != nil || b.QualifiedRelation() != "exports.orders_v1" {
		t.Fatalf("binding=%+v err=%v", b, err)
	}
	if b.Digest("snapshot") == b.Digest("incremental") {
		t.Fatal("mode must participate in binding digest")
	}
	if _, err := p.Binding("p", "other"); !errors.Is(err, ErrSourcePolicyDenied) {
		t.Fatalf("denial=%v", err)
	}
}

func TestSourcePolicyRejectsBroadOrIncompleteEntries(t *testing.T) {
	tests := []SourcePolicy{
		{Version: 2},
		{Version: 1, Destinations: []SourceDestination{{Host: "db", Port: 5432}}},
		{Version: 1, Bindings: []SourceBinding{{ProjectID: "p", ConnectorID: "c", Schema: "x", Relation: "y", RelationKind: RelationKindView, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique}}},
		{Version: 1, Bindings: []SourceBinding{{ProjectID: "p", ConnectorID: "c", Schema: "x", Relation: "y", RelationKind: RelationKindView, Columns: []SourcePolicyColumn{{Name: "id", PGType: "geography"}}, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique}}},
	}
	for i := range tests {
		if err := tests[i].Validate(); err == nil {
			t.Fatalf("case %d unexpectedly valid", i)
		}
	}
}

func TestLoadSourcePolicyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourcePolicy(path); err == nil {
		t.Fatal("unknown policy field accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1} {"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourcePolicy(path); err == nil {
		t.Fatal("trailing policy document accepted")
	}
	if _, err := LoadSourcePolicy("relative-policy.json"); err == nil {
		t.Fatal("relative policy path accepted")
	}
	empty, err := LoadSourcePolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Binding("p", "c"); !errors.Is(err, ErrSourcePolicyDenied) {
		t.Fatalf("empty policy did not deny: %v", err)
	}
}

func TestDestinationAllowsOnlyExplicitCIDR(t *testing.T) {
	p := &SourcePolicy{Version: 1, Destinations: []SourceDestination{{Host: "db.example", Port: 5432, AllowedIPCIDRs: []string{"10.0.0.5/32"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Destination("DB.EXAMPLE.", 5432); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Destination("db.example", 5433); !errors.Is(err, ErrSourcePolicyDenied) {
		t.Fatalf("port denial=%v", err)
	}
}

func TestSourcePolicyPostgresCannotBypassGovernedOpen(t *testing.T) {
	_, err := Open(context.Background(), "postgres", "postgres://user:secret@127.0.0.1/db")
	if !errors.Is(err, ErrSourcePolicyDenied) {
		t.Fatalf("Open postgres error = %v, want ErrSourcePolicyDenied", err)
	}
}

func TestRepairG3PolicyAllowsMultipleRelationsPerConnector(t *testing.T) {
	base := SourceBinding{ProjectID: "p", ConnectorID: "c", Schema: "public", RelationKind: RelationKindLegacyTable,
		Columns: []SourcePolicyColumn{{Name: "id", PGType: "bigint"}}, KeyColumn: "id", KeyStability: KeyStabilityImmutableUnique}
	users := base
	users.Relation = "users"
	users.LegacySyncIDs = []string{"sync-users"}
	orders := base
	orders.Relation = "orders"
	orders.LegacySyncIDs = []string{"sync-orders"}
	p := &SourcePolicy{Version: 1, Bindings: []SourceBinding{users, orders}}
	if err := p.Validate(); err != nil {
		t.Fatalf("two explicit relation bindings rejected: %v", err)
	}
	for relation, wantSync := range map[string]string{"users": "sync-users", "orders": "sync-orders"} {
		binding, err := p.BindingForRelation("p", "c", relation)
		if err != nil || !binding.AllowsSync(wantSync) {
			t.Fatalf("binding for %s=%+v err=%v", relation, binding, err)
		}
	}
}
