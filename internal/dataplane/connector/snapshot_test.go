package connector

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotCanonicalDigestAndManifest(t *testing.T) {
	rows := []SnapshotRow{{Key: "1", Cursor: "", Data: []byte(`{"payment_status":"completed","id":1,"amount_vnd":100000}`)}}
	digest, err := SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "0aec0868e06383679d6599a22a8fd5d35618a3b3dcdbb6b305a0d71f53f64fcf" {
		t.Fatalf("digest=%s", digest)
	}
	manifest, err := SnapshotManifestDigest([]SnapshotManifestEntry{
		{Index: 1, BatchID: "batch-000001", PayloadSHA256: "2e2da584e7ad9de0e56ad38843bb20b945660738b570fe39e0786bf984f03f7a", RowCount: 1},
		{Index: 0, BatchID: "batch-000000", PayloadSHA256: digest, RowCount: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest != "f52627c82e7b54eca7c861082e57612713fe517d38f3afc8f4617980171e1e87" {
		t.Fatalf("manifest=%s", manifest)
	}
}

func TestSnapshotManifestRejectsMissingIndex(t *testing.T) {
	_, err := SnapshotManifestDigest([]SnapshotManifestEntry{{Index: 1, BatchID: "b", PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RowCount: 1}})
	if err == nil {
		t.Fatal("missing index accepted")
	}
}

func TestSnapshotEnvelopeRejectsCrossKindFieldsAndDuplicateKeys(t *testing.T) {
	started := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := []SnapshotRow{{Key: "1", Data: []byte(`{"id":1}`)}}
	digest, err := SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	env := SnapshotEnvelope{Protocol: SnapshotProtocolV1, ProjectID: "p", ConnectorID: "c", Table: "exports.rows", SyncID: "s", Generation: "g", GenerationSeq: 1,
		BindingDigest: strings.Repeat("a", 64), CaptureStartedAt: started, Kind: SnapshotKindBatch, RunID: "r", BatchID: "b", PayloadSHA256: digest, Rows: rows}
	env.ExpectedRows = 1
	if err := env.Validate(); err == nil {
		t.Fatal("batch carrying completion fields was accepted")
	}
	env.ExpectedRows = 0
	env.Rows = append(env.Rows, env.Rows[0])
	env.PayloadSHA256, err = SnapshotPayloadDigest(env.Rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Validate(); err == nil {
		t.Fatal("batch carrying duplicate row keys was accepted")
	}
}
