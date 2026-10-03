package storage

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func sourceModePtr(s string) *string { return &s }

func TestConnectorConfigSnapshotRequiresApprovedView(t *testing.T) {
	p := &connector.SourcePolicy{Version: 1, Bindings: []connector.SourceBinding{{ProjectID: "p", ConnectorID: "c", Schema: "exports", Relation: "orders_v1", RelationKind: connector.RelationKindView,
		Columns: []connector.SourcePolicyColumn{{Name: "id", PGType: "bigint"}, {Name: "updated_at", PGType: "timestamp with time zone"}}, KeyColumn: "id", KeyStability: connector.KeyStabilityImmutableUnique, AllowedCursorColumns: []string{"updated_at"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	s := &Store{sourcePolicy: p, sourcePolicyConfigured: true}
	in := ConnectorSyncInput{SourceTable: "exports.orders_v1", KeyColumn: "id", SyncMode: sourceModePtr("snapshot")}
	if err := validateSyncInput(in); err != nil {
		t.Fatal(err)
	}
	if err := s.validateSourceBinding("p", "c", "", in, "snapshot"); err != nil {
		t.Fatal(err)
	}
	in.SourceTable = "public.users"
	if err := s.validateSourceBinding("p", "c", "", in, "snapshot"); err == nil {
		t.Fatal("unapproved table accepted")
	}
}

func TestConnectorConfigIncrementalAndLegacyRules(t *testing.T) {
	inc := ConnectorSyncInput{SourceTable: "exports.orders_v1", KeyColumn: "id", SyncMode: sourceModePtr("incremental")}
	if err := validateSyncInput(inc); err == nil {
		t.Fatal("incremental without cursor accepted")
	}
	snapshotWithCursor := ConnectorSyncInput{SourceTable: "exports.orders_v1", KeyColumn: "id", CursorColumn: "updated_at", SyncMode: sourceModePtr("snapshot")}
	if err := validateSyncInput(snapshotWithCursor); err == nil {
		t.Fatal("snapshot with cursor accepted")
	}
	p := &connector.SourcePolicy{Version: 1, Bindings: []connector.SourceBinding{{ProjectID: "p", ConnectorID: "c", Schema: "public", Relation: "orders", RelationKind: connector.RelationKindLegacyTable,
		Columns: []connector.SourcePolicyColumn{{Name: "id", PGType: "bigint"}}, KeyColumn: "id", KeyStability: connector.KeyStabilityImmutableUnique, LegacySyncIDs: []string{"existing"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	s := &Store{sourcePolicy: p, sourcePolicyConfigured: true}
	in := ConnectorSyncInput{SourceTable: "public.orders", KeyColumn: "id"}
	if err := s.validateSourceBinding("p", "c", "", in, ""); err == nil {
		t.Fatal("new legacy table sync was grandfathered")
	}
	if err := s.validateSourceBinding("p", "c", "existing", in, ""); err != nil {
		t.Fatal(err)
	}
}
