package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestConnectorContractDraftPublishesExplicitSyncMode(t *testing.T) {
	if !strings.Contains(syncDraftSystem, "sync_mode") || !strings.Contains(syncDraftSystem, "snapshot") {
		t.Fatal("draft contract does not teach explicit sync modes")
	}
	var draft syncDraft
	draft.Syncs = make([]struct {
		SourceTable  string `json:"source_table"`
		KeyColumn    string `json:"key_column"`
		CursorColumn string `json:"cursor_column"`
		SyncMode     string `json:"sync_mode"`
		ScheduleCron string `json:"schedule_cron"`
		Reason       string `json:"reason,omitempty"`
	}, 1)
	draft.Syncs[0].SourceTable = "exports.orders_v1"
	draft.Syncs[0].KeyColumn = "id"
	draft.Syncs[0].SyncMode = "snapshot"
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sync_mode":"snapshot"`) {
		t.Fatalf("draft wire=%s", raw)
	}
}

func TestConnectorContractDiscoveryMetadataWire(t *testing.T) {
	table := connector.Table{Name: "exports.orders_v1", RelationKind: connector.RelationKindView, KeyColumn: "id", KeyStability: connector.KeyStabilityImmutableUnique, Columns: []connector.Column{{Name: "id", Type: "bigint", IsPrimaryKey: true}}}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"relation_kind":"view"`, `"key_column":"id"`, `"key_stability":"immutable_unique_non_null"`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("missing %s in %s", field, raw)
		}
	}
}
