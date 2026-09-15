package usecase

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// annotations_e2e_test.go — the chart-annotation contract driven through the
// real operation registry (the same definitions REST /api/op, MCP, the CLI and
// the in-process agent share) against live Postgres. It proves the op-level
// contract the store tests cannot: access classes, the member floor, project
// isolation through the ops, and annotation ids resolving as evidence
// references. Skips without a reachable database.

func TestAnnotationsEndToEnd(t *testing.T) {
	s := openE2EStore(t)
	ctx := context.Background()
	reg := Registry()

	acct, err := s.CreateAccount(ctx, fmt.Sprintf("anno-%d@test.local", time.Now().UnixNano()), "Anno E2E", "password-1234", "anno-ws", "anno-proj")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	cc := opcore.CallContext{ProjectID: acct.Project.ID, Deps: &Deps{Repo: s}}

	// --- add + list through the ops ---
	added := invoke(t, reg, cc, "add_annotation", `{
		"label": "v2.4 deploy", "kind": "deploy",
		"starts_at": "2026-09-10T12:00:00Z", "idempotency_key": "anno-add-1"
	}`)
	annotationID, _ := added["id"].(string)
	if annotationID == "" {
		t.Fatalf("add_annotation returned %+v", added)
	}
	if added["kind"] != "deploy" || added["label"] != "v2.4 deploy" {
		t.Fatalf("add_annotation = %+v", added)
	}

	// A replayed add returns the same row — one mark, not two.
	replay := invoke(t, reg, cc, "add_annotation", `{
		"label": "v2.4 deploy", "kind": "deploy",
		"starts_at": "2026-09-10T12:00:00Z", "idempotency_key": "anno-add-1"
	}`)
	if replay["id"] != annotationID {
		t.Fatalf("replayed add returned a different annotation: %+v", replay)
	}

	// A range annotation overlapping the same window.
	invoke(t, reg, cc, "add_annotation", `{
		"label": "pricing campaign", "kind": "campaign",
		"starts_at": "2026-09-12T00:00:00Z", "ends_at": "2026-09-13T00:00:00Z"
	}`)

	listed := invoke(t, reg, cc, "list_annotations", `{"from":"2026-09-08T00:00:00Z","to":"2026-09-14T00:00:00Z"}`)
	rows, _ := listed["annotations"].([]any)
	if len(rows) != 2 {
		t.Fatalf("list_annotations = %+v", listed)
	}
	// Outside the window: nothing.
	outside := invoke(t, reg, cc, "list_annotations", `{"from":"2026-08-01T00:00:00Z","to":"2026-08-02T00:00:00Z"}`)
	if rows, _ := outside["annotations"].([]any); len(rows) != 0 {
		t.Fatalf("outside window = %+v", outside)
	}

	// --- Validation is the op's contract, not the store's ---
	if err := invokeErr(t, reg, cc, "add_annotation", `{"label":"x","starts_at":"not-a-date"}`); !strings.Contains(err.Error(), "starts_at") {
		t.Fatalf("bad starts_at err = %v", err)
	}
	if err := invokeErr(t, reg, cc, "add_annotation", `{"label":"x","kind":"incident","starts_at":"2026-09-10T00:00:00Z"}`); !strings.Contains(err.Error(), "kind") {
		t.Fatalf("bad kind err = %v", err)
	}

	// --- Access classes are the split the credential model claims ---
	capture := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredCapture}
	for _, op := range []string{"add_annotation", "list_annotations", "delete_annotation"} {
		if reg.Authorize(capture, op) {
			t.Fatalf("a capture credential reached %s", op)
		}
	}
	reader := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredManagement, Grants: []opcore.Access{opcore.AccessAnalyticsRead}}
	if !reg.Authorize(reader, "list_annotations") {
		t.Fatal("an analytics:read credential cannot list annotations")
	}
	if reg.Authorize(reader, "add_annotation") || reg.Authorize(reader, "delete_annotation") {
		t.Fatal("an analytics:read credential wrote an annotation")
	}
	writer := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredManagement, Grants: []opcore.Access{opcore.AccessPlansWrite}}
	if !reg.Authorize(writer, "add_annotation") || !reg.Authorize(writer, "delete_annotation") {
		t.Fatal("a plans:write credential cannot mark the timeline")
	}
	// The member floor: a viewer session is refused the writes even though
	// its grants would otherwise cover them.
	viewer := opcore.Principal{ProjectID: acct.Project.ID, Kind: opcore.CredSession, Role: "viewer",
		Grants: []opcore.Access{opcore.AccessAnalyticsRead, opcore.AccessPlansWrite}}
	if reg.Authorize(viewer, "add_annotation") {
		t.Fatal("a viewer session wrote an annotation")
	}
	if !reg.Authorize(viewer, "list_annotations") {
		t.Fatal("a viewer session cannot read annotations")
	}

	// --- Project isolation through the ops ---
	acctB, err := s.CreateAccount(ctx, fmt.Sprintf("anno-b-%d@test.local", time.Now().UnixNano()), "Anno E2E B", "password-1234", "anno-ws-b", "anno-proj-b")
	if err != nil {
		t.Fatalf("seed account B: %v", err)
	}
	ccB := opcore.CallContext{ProjectID: acctB.Project.ID, Deps: &Deps{Repo: s}}
	if err := invokeErr(t, reg, ccB, "delete_annotation", fmt.Sprintf(`{"annotation_id":%q}`, annotationID)); err == nil {
		t.Fatal("a foreign project deleted the annotation")
	}
	if rows, _ := invoke(t, reg, ccB, "list_annotations", `{"from":"2026-09-08T00:00:00Z","to":"2026-09-14T00:00:00Z"}`)["annotations"].([]any); len(rows) != 0 {
		t.Fatal("a foreign project listed the annotation")
	}

	// --- Annotations are evidence ---
	// A finding may cite one by id inside the envelope.
	rec := invoke(t, reg, cc, "submit_recommendation", fmt.Sprintf(`{
		"category": "product", "title": "Deploy lifted activation",
		"evidence": {"annotation_ids": [%q], "range": "2026-09-08/2026-09-14"}
	}`, annotationID))
	if rec["recommendation_id"] == "" {
		t.Fatalf("submit_recommendation = %+v", rec)
	}
	// An unknown id is refused, naming the reference that did not resolve.
	if err := invokeErr(t, reg, cc, "submit_recommendation", `{
		"category": "product", "title": "x",
		"evidence": {"annotation_ids": ["00000000-0000-0000-0000-000000000000"]}
	}`); !strings.Contains(err.Error(), "annotation_ids") {
		t.Fatalf("unresolvable citation err = %v", err)
	}
	// A foreign project's id does not resolve either — the citation is
	// project-scoped, not just well-formed.
	if err := invokeErr(t, reg, ccB, "submit_recommendation", fmt.Sprintf(`{
		"category": "product", "title": "x",
		"evidence": {"annotation_ids": [%q]}
	}`, annotationID)); !strings.Contains(err.Error(), "annotation_ids") {
		t.Fatalf("cross-project citation err = %v", err)
	}

	// record_outcome accepts annotation:<id> as its evidence_ref — on a
	// committed test, since proposed tests reject outcomes.
	proposed := invoke(t, reg, cc, "propose_test", `{"hypothesis":"deploy lifted activation","metric_event":"user.pageview","target_count":3}`)
	testID, _ := proposed["test_id"].(string)
	if testID == "" {
		t.Fatalf("propose_test = %+v", proposed)
	}
	if err := s.CommitValidationTest(ctx, acct.User.ID, acct.Project.ID, testID); err != nil {
		t.Fatalf("commit test: %v", err)
	}
	outcome := invoke(t, reg, cc, "record_outcome", fmt.Sprintf(`{
		"test_id": %q, "revision": 1, "value": 12.5, "unit": "activation %%",
		"window": "Sep 8-14", "evidence_ref": "annotation:%s"
	}`, testID, annotationID))
	if outcome["entries"] != float64(1) {
		t.Fatalf("record_outcome = %+v", outcome)
	}
	if err := invokeErr(t, reg, cc, "record_outcome", fmt.Sprintf(`{
		"test_id": %q, "revision": 2, "value": 1, "unit": "x",
		"evidence_ref": "annotation:00000000-0000-0000-0000-000000000000"
	}`, testID)); !strings.Contains(err.Error(), "annotation:") {
		t.Fatalf("unresolvable evidence_ref err = %v", err)
	}

	// --- delete through the op, replayed ---
	deleted := invoke(t, reg, cc, "delete_annotation", fmt.Sprintf(`{"annotation_id":%q,"idempotency_key":"anno-del-1"}`, annotationID))
	if deleted["id"] != annotationID {
		t.Fatalf("delete_annotation = %+v", deleted)
	}
	again := invoke(t, reg, cc, "delete_annotation", fmt.Sprintf(`{"annotation_id":%q,"idempotency_key":"anno-del-1"}`, annotationID))
	if again["id"] != annotationID {
		t.Fatalf("replayed delete = %+v", again)
	}
	if err := invokeErr(t, reg, cc, "delete_annotation", fmt.Sprintf(`{"annotation_id":%q}`, annotationID)); err == nil {
		t.Fatal("deleting a removed annotation succeeded")
	}
}
