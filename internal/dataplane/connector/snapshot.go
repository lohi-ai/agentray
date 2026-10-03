package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	SnapshotProtocolV1   = "agentray.connector.snapshot.v1"
	SnapshotKindBatch    = "batch"
	SnapshotKindComplete = "complete"
)

// SnapshotEnvelope is the frozen C1 wire contract. Batch and completion-only
// fields intentionally share one envelope so a protocol discriminator is
// required before any legacy decode can occur.
type SnapshotEnvelope struct {
	Protocol            string        `json:"protocol"`
	ProjectID           string        `json:"project_id"`
	ConnectorID         string        `json:"connector_id"`
	Table               string        `json:"table"`
	SyncID              string        `json:"sync_id"`
	Generation          string        `json:"generation"`
	GenerationSeq       int64         `json:"generation_seq"`
	BindingDigest       string        `json:"binding_digest"`
	CaptureStartedAt    time.Time     `json:"capture_started_at"`
	Kind                string        `json:"kind"`
	RunID               string        `json:"run_id"`
	BatchID             string        `json:"batch_id,omitempty"`
	BatchIndex          int64         `json:"batch_index,omitempty"`
	PayloadSHA256       string        `json:"payload_sha256,omitempty"`
	CaptureFinishedAt   *time.Time    `json:"capture_finished_at"`
	Rows                []SnapshotRow `json:"rows,omitempty"`
	ExpectedBatches     int64         `json:"expected_batches,omitempty"`
	ExpectedRows        int64         `json:"expected_rows,omitempty"`
	BatchManifestSHA256 string        `json:"batch_manifest_sha256,omitempty"`
}

type SnapshotRow struct {
	Key    string          `json:"key"`
	Cursor string          `json:"cursor"`
	Data   json.RawMessage `json:"data"`
}

type SnapshotManifestEntry struct {
	Index         int64
	BatchID       string
	PayloadSHA256 string
	RowCount      int
}

type SnapshotGeneration struct {
	ProjectID         string     `json:"project_id"`
	ConnectorID       string     `json:"connector_id"`
	Table             string     `json:"table"`
	SyncID            string     `json:"sync_id"`
	Generation        string     `json:"generation"`
	GenerationSeq     int64      `json:"generation_seq"`
	BindingDigest     string     `json:"binding_digest"`
	State             string     `json:"state"`
	KeyPosition       string     `json:"key_position,omitempty"`
	NextBatchIndex    int64      `json:"next_batch_index"`
	Rows              int64      `json:"rows"`
	CaptureStartedAt  time.Time  `json:"capture_started_at"`
	CaptureFinishedAt *time.Time `json:"capture_finished_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	TerminalAt        *time.Time `json:"terminal_at,omitempty"`
	RunID             string     `json:"run_id,omitempty"`
	Owner             string     `json:"-"`
	LeaseEpoch        int64      `json:"-"`
}

type SnapshotOutbox struct {
	ID         string
	Generation string
	BatchID    string
	BatchIndex int64
	Kind       string
	Payload    []byte
	LowerKey   string
	UpperKey   string
	Rows       int
	Published  bool
}

type SnapshotPromotion struct {
	ProjectID           string    `json:"project_id"`
	ConnectorID         string    `json:"connector_id"`
	Table               string    `json:"table"`
	SyncID              string    `json:"sync_id"`
	Generation          string    `json:"generation"`
	GenerationSeq       int64     `json:"generation_seq"`
	BindingDigest       string    `json:"binding_digest"`
	CaptureStartedAt    time.Time `json:"capture_started_at"`
	CaptureFinishedAt   time.Time `json:"capture_finished_at"`
	PromotedAt          time.Time `json:"promoted_at"`
	ExpectedBatches     int64     `json:"expected_batches"`
	ExpectedRows        int64     `json:"expected_rows"`
	BatchManifestSHA256 string    `json:"batch_manifest_sha256"`
}

type SnapshotGenerationDescriptor struct {
	SnapshotGeneration
	Resumable            bool `json:"resumable"`
	IsActiveOnThisStore  bool `json:"is_active_on_this_store"`
	HasUnpublishedOutbox bool `json:"has_unpublished_outbox"`
}

func SnapshotRows(rows []LandedRow) ([]SnapshotRow, error) {
	out := make([]SnapshotRow, 0, len(rows))
	for _, row := range rows {
		canon, err := canonicalJSON([]byte(row.DataJSON))
		if err != nil {
			return nil, fmt.Errorf("snapshot row has invalid JSON")
		}
		out = append(out, SnapshotRow{Key: row.Key, Cursor: row.Cursor, Data: canon})
	}
	return out, nil
}

// CanonicalSnapshotRows returns compact JSON with fixed outer field order and
// recursively sorted object keys in data. encoding/json provides stable map
// key ordering after canonicalJSON decodes the raw source object.
func CanonicalSnapshotRows(rows []SnapshotRow) ([]byte, error) {
	canonical := make([]SnapshotRow, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.Key) == "" || row.Cursor != "" {
			return nil, fmt.Errorf("snapshot batch rows require a non-empty key and empty cursor")
		}
		data, err := canonicalJSON(row.Data)
		if err != nil {
			return nil, fmt.Errorf("snapshot row has invalid JSON")
		}
		canonical = append(canonical, SnapshotRow{Key: row.Key, Cursor: "", Data: data})
	}
	return json.Marshal(canonical)
}

func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return json.Marshal(v)
}

// MarshalJSON keeps required zero-valued batch/completion fields on the wire
// while omitting fields that belong to the other envelope kind.
func (e SnapshotEnvelope) MarshalJSON() ([]byte, error) {
	type common struct {
		Protocol         string    `json:"protocol"`
		ProjectID        string    `json:"project_id"`
		ConnectorID      string    `json:"connector_id"`
		Table            string    `json:"table"`
		SyncID           string    `json:"sync_id"`
		Generation       string    `json:"generation"`
		GenerationSeq    int64     `json:"generation_seq"`
		BindingDigest    string    `json:"binding_digest"`
		CaptureStartedAt time.Time `json:"capture_started_at"`
		Kind             string    `json:"kind"`
		RunID            string    `json:"run_id"`
	}
	c := common{e.Protocol, e.ProjectID, e.ConnectorID, e.Table, e.SyncID, e.Generation, e.GenerationSeq, e.BindingDigest, e.CaptureStartedAt, e.Kind, e.RunID}
	if e.Kind == SnapshotKindBatch {
		return json.Marshal(struct {
			common
			BatchID           string        `json:"batch_id"`
			BatchIndex        int64         `json:"batch_index"`
			PayloadSHA256     string        `json:"payload_sha256"`
			CaptureFinishedAt *time.Time    `json:"capture_finished_at"`
			Rows              []SnapshotRow `json:"rows"`
		}{c, e.BatchID, e.BatchIndex, e.PayloadSHA256, e.CaptureFinishedAt, e.Rows})
	}
	return json.Marshal(struct {
		common
		ExpectedBatches     int64      `json:"expected_batches"`
		ExpectedRows        int64      `json:"expected_rows"`
		BatchManifestSHA256 string     `json:"batch_manifest_sha256"`
		CaptureFinishedAt   *time.Time `json:"capture_finished_at"`
	}{c, e.ExpectedBatches, e.ExpectedRows, e.BatchManifestSHA256, e.CaptureFinishedAt})
}

func SnapshotPayloadDigest(rows []SnapshotRow) (string, error) {
	raw, err := CanonicalSnapshotRows(rows)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func SnapshotManifestDigest(entries []SnapshotManifestEntry) (string, error) {
	entries = append([]SnapshotManifestEntry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
	var b strings.Builder
	for i, entry := range entries {
		if entry.Index != int64(i) || entry.BatchID == "" || !isSHA256(entry.PayloadSHA256) || entry.RowCount < 0 {
			return "", fmt.Errorf("snapshot manifest is not contiguous")
		}
		fmt.Fprintf(&b, "%d:%s:%s:%d\n", entry.Index, entry.BatchID, entry.PayloadSHA256, entry.RowCount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func (e SnapshotEnvelope) Validate() error {
	if e.Protocol != SnapshotProtocolV1 || e.ProjectID == "" || e.ConnectorID == "" || e.Table == "" ||
		e.SyncID == "" || e.RunID == "" || e.Generation == "" || e.GenerationSeq <= 0 ||
		!isSHA256(e.BindingDigest) || e.CaptureStartedAt.IsZero() {
		return fmt.Errorf("invalid snapshot identity")
	}
	switch e.Kind {
	case SnapshotKindBatch:
		if e.BatchID == "" || e.BatchIndex < 0 || !isSHA256(e.PayloadSHA256) || e.CaptureFinishedAt != nil || len(e.Rows) == 0 ||
			e.ExpectedBatches != 0 || e.ExpectedRows != 0 || e.BatchManifestSHA256 != "" {
			return fmt.Errorf("invalid snapshot batch")
		}
		keys := make(map[string]struct{}, len(e.Rows))
		for _, row := range e.Rows {
			if _, duplicate := keys[row.Key]; duplicate {
				return fmt.Errorf("snapshot batch repeats a row key")
			}
			keys[row.Key] = struct{}{}
		}
		digest, err := SnapshotPayloadDigest(e.Rows)
		if err != nil || digest != e.PayloadSHA256 {
			return fmt.Errorf("snapshot batch payload digest mismatch")
		}
	case SnapshotKindComplete:
		if e.CaptureFinishedAt == nil || e.CaptureFinishedAt.Before(e.CaptureStartedAt) || e.ExpectedBatches < 0 || e.ExpectedRows < 0 ||
			!isSHA256(e.BatchManifestSHA256) || len(e.Rows) != 0 || e.BatchID != "" || e.BatchIndex != 0 || e.PayloadSHA256 != "" {
			return fmt.Errorf("invalid snapshot completion")
		}
	default:
		return fmt.Errorf("unknown snapshot kind")
	}
	return nil
}

func isSHA256(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil && raw == strings.ToLower(raw)
}

func MarshalSnapshotEnvelope(e SnapshotEnvelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

func ParseSnapshotEnvelope(raw []byte) (SnapshotEnvelope, error) {
	var env SnapshotEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, err
	}
	if err := env.Validate(); err != nil {
		return env, err
	}
	return env, nil
}
