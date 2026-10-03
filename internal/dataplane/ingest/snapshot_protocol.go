package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/nats-io/nats.go/jetstream"
)

// BuildSnapshotBatches applies the broker's actual payload budget before the
// producer persists anything. The returned envelopes therefore are the exact
// immutable wire chunks counted by completion.
func (q EventQueue) BuildSnapshotBatches(common connector.SnapshotEnvelope, rows []connector.LandedRow, startIndex int64) ([]connector.SnapshotEnvelope, error) {
	if q.js == nil {
		return nil, fmt.Errorf("snapshot sync requires durable JetStream")
	}
	chunks := chunkRows(rows, q.publishBudget())
	out := make([]connector.SnapshotEnvelope, 0, len(chunks))
	for i, chunk := range chunks {
		landed := make([]connector.LandedRow, 0, len(chunk))
		for _, row := range chunk {
			landed = append(landed, connector.LandedRow{Key: row.Key, Cursor: "", DataJSON: string(row.Data)})
		}
		wireRows, err := connector.SnapshotRows(landed)
		if err != nil {
			return nil, err
		}
		idx := startIndex + int64(i)
		env := common
		env.Protocol = connector.SnapshotProtocolV1
		env.Kind = connector.SnapshotKindBatch
		env.BatchIndex = idx
		env.BatchID = fmt.Sprintf("batch-%06d", idx)
		env.Rows = wireRows
		env.CaptureFinishedAt = nil
		env.PayloadSHA256, err = connector.SnapshotPayloadDigest(wireRows)
		if err != nil {
			return nil, err
		}
		if _, err := connector.MarshalSnapshotEnvelope(env); err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, nil
}

func (q EventQueue) PublishSnapshotEnvelope(ctx context.Context, env connector.SnapshotEnvelope) error {
	if q.js == nil {
		return fmt.Errorf("snapshot sync requires durable JetStream")
	}
	if admission, ok := q.publicationObserver.(interface{ AdmitDataPublication() error }); ok {
		if err := admission.AdmitDataPublication(); err != nil {
			return err
		}
	}
	body, err := connector.MarshalSnapshotEnvelope(env)
	if err != nil {
		return err
	}
	id := strings.Join([]string{env.Protocol, env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.Kind}, ":")
	if env.Kind == connector.SnapshotKindBatch {
		id += ":" + env.BatchID
	} else {
		id += ":complete"
	}
	if _, err := q.js.Publish(ctx, q.connectorSubject, body, jetstream.WithMsgID(id)); err != nil {
		return fmt.Errorf("publish snapshot envelope: %w", err)
	}
	if q.publicationObserver != nil {
		published := time.Now().UTC()
		mark := sourceReceiptFromSnapshotEnvelope(env, &published)
		stableID := env.Generation + ":" + env.Kind
		payloadDigest := env.PayloadSHA256
		if env.Kind == connector.SnapshotKindBatch {
			stableID += ":" + env.BatchID
		} else {
			stableID += ":complete"
			payloadDigest = env.BatchManifestSHA256
		}
		if err := q.publicationObserver.RecordPublication(ctx, storage.PublicationObservation{
			SourceReceiptMark: *mark, StableBatchID: stableID, PayloadSHA256: payloadDigest, PublishedAt: published,
		}); err != nil {
			return fmt.Errorf("record accepted snapshot publication: %w", err)
		}
	}
	return nil
}

func sourceReceiptFromSnapshotEnvelope(env connector.SnapshotEnvelope, published *time.Time) *storage.SourceReceiptMark {
	seq := uint64(env.GenerationSeq)
	mark := &storage.SourceReceiptMark{ProjectID: env.ProjectID, ConnectorID: env.ConnectorID, Table: env.Table,
		SyncID: env.SyncID, RunID: env.RunID, Generation: env.Generation, GenerationSeq: seq,
		BindingDigest: env.BindingDigest, CaptureStartedAt: &env.CaptureStartedAt, PublishedAt: published}
	if env.Kind == connector.SnapshotKindBatch {
		index := uint64(env.BatchIndex)
		mark.BatchID, mark.BatchIndex, mark.PayloadSHA256 = env.BatchID, &index, env.PayloadSHA256
	} else {
		mark.Complete, mark.CaptureFinishedAt = true, env.CaptureFinishedAt
		expectedBatches, expectedRows := uint64(env.ExpectedBatches), uint64(env.ExpectedRows)
		mark.ExpectedBatches, mark.ExpectedRows = &expectedBatches, &expectedRows
	}
	return mark
}

func decodeConnectorEnvelope(raw []byte) (*ExternalRowsBatch, *connector.SnapshotEnvelope, error) {
	// Inspect top-level fields structurally. Legacy row data is arbitrary JSON
	// and may legitimately contain keys such as "kind" or "generation"; a
	// byte search would misclassify those established payloads as snapshots.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, err
	}
	protocolRaw, hasProtocol := probe["protocol"]
	if hasProtocol {
		var protocol string
		if err := json.Unmarshal(protocolRaw, &protocol); err != nil {
			return nil, nil, fmt.Errorf("invalid connector protocol")
		}
		if protocol != connector.SnapshotProtocolV1 {
			return nil, nil, fmt.Errorf("unknown connector protocol %q", protocol)
		}
		var env connector.SnapshotEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, nil, err
		}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		return nil, &env, nil
	}
	for _, field := range []string{"kind", "sync_id", "generation", "generation_seq", "batch_id", "batch_index", "expected_batches", "batch_manifest_sha256", "binding_digest"} {
		if _, ok := probe[field]; ok {
			return nil, nil, fmt.Errorf("snapshot field %s requires an explicit protocol", strconv.Quote(field))
		}
	}
	return decodeLegacyConnectorBatch(raw)
}

func decodeLegacyConnectorBatch(raw []byte) (*ExternalRowsBatch, *connector.SnapshotEnvelope, error) {
	var legacy ExternalRowsBatch
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, nil, err
	}
	if legacy.ProjectID == "" || legacy.ConnectorID == "" || legacy.Table == "" {
		return nil, nil, fmt.Errorf("invalid legacy connector batch identity")
	}
	return &legacy, nil, nil
}
