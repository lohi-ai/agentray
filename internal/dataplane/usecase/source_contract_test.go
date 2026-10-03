package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

type governedSourceRepo struct {
	fakeRepo
	policy *connector.SourcePolicy
	dsn    string
}

func (r *governedSourceRepo) ConnectorDSNForProject(context.Context, string, string) (string, string, error) {
	return "postgres", r.dsn, nil
}
func (r *governedSourceRepo) SourcePolicy() *connector.SourcePolicy { return r.policy }

func TestSourceContractDeniesUnboundConnectorWithoutDSNLeak(t *testing.T) {
	repo := &governedSourceRepo{policy: &connector.SourcePolicy{Version: 1}, dsn: "postgres://user:super-secret@db.invalid/app"}
	_, err := openProjectSource(context.Background(), &Deps{Repo: repo}, "project", "connector")
	if err == nil {
		t.Fatal("unbound connector opened")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked DSN: %v", err)
	}
}

func TestSourceContractBoundMalformedDSNIsGeneric(t *testing.T) {
	b := connector.SourceBinding{ProjectID: "project", ConnectorID: "connector", Schema: "exports", Relation: "orders", RelationKind: connector.RelationKindView,
		Columns: []connector.SourcePolicyColumn{{Name: "id", PGType: "bigint"}}, KeyColumn: "id", KeyStability: connector.KeyStabilityImmutableUnique}
	repo := &governedSourceRepo{policy: &connector.SourcePolicy{Version: 1, Bindings: []connector.SourceBinding{b}}, dsn: "postgres://user:super-secret@bad host:notaport/app"}
	_, err := openProjectSource(context.Background(), &Deps{Repo: repo}, "project", "connector")
	if err == nil {
		t.Fatal("malformed DSN opened")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked DSN: %v", err)
	}
}
