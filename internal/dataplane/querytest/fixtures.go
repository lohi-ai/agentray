// Package querytest provides deterministic, production-independent fixtures
// shared by query adapter tests. It opens no resources and is imported only by
// tests; the JSON is deliberately generic rather than a copy of customer data.
package querytest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed testdata/canonical.json
var canonicalJSON []byte

type Event struct {
	ProjectID  string `json:"project_id"`
	EventID    string `json:"event_id"`
	DistinctID string `json:"distinct_id"`
	EventName  string `json:"event_name"`
	Timestamp  string `json:"timestamp"`
	Properties string `json:"properties"`
	InsertID   string `json:"insert_id,omitempty"`
	Platform   string `json:"platform,omitempty"`
	UTMSource  string `json:"utm_source,omitempty"`
}

type Alias struct {
	ProjectID   string `json:"project_id"`
	AnonymousID string `json:"anonymous_id"`
	CanonicalID string `json:"canonical_id"`
}

type ExternalRow struct {
	ProjectID   string `json:"project_id"`
	ConnectorID string `json:"connector_id"`
	TableName   string `json:"table_name"`
	RowKey      string `json:"row_key"`
	Data        string `json:"data"`
}

type Query struct {
	Name     string           `json:"name"`
	SQL      string           `json:"sql"`
	Expected []map[string]any `json:"expected"`
}

type Fixture struct {
	ProjectID      string            `json:"project_id"`
	OtherProjectID string            `json:"other_project_id"`
	Connectors     map[string]string `json:"connectors"`
	Events         []Event           `json:"events"`
	Aliases        []Alias           `json:"aliases"`
	ExternalRows   []ExternalRow     `json:"external_rows"`
	Queries        []Query           `json:"queries"`
}

func Canonical() (Fixture, error) {
	var fixture Fixture
	decoder := json.NewDecoder(bytes.NewReader(canonicalJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode canonical query fixture: %w", err)
	}
	return fixture, nil
}

//go:embed testdata/lohi_evidence_v1.json
var lohiEvidenceJSON []byte

type LohiAssertion struct {
	Date       string `json:"date"`
	Series     string `json:"series"`
	Absent     bool   `json:"absent,omitempty"`
	Value      any    `json:"value,omitempty"`
	State      string `json:"state,omitempty"`
	ReasonLike string `json:"reason_like,omitempty"`
	Eligible   any    `json:"eligible,omitempty"`
	Converted  any    `json:"converted,omitempty"`
}

type LohiEvidenceFixture struct {
	Fixture
	Assertions map[string][]LohiAssertion `json:"assertions"`
	Cases      []string                   `json:"cases"`
}

func LohiEvidenceV1() (LohiEvidenceFixture, error) {
	var fixture LohiEvidenceFixture
	decoder := json.NewDecoder(bytes.NewReader(lohiEvidenceJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		return LohiEvidenceFixture{}, fmt.Errorf("decode lohi evidence fixture: %w", err)
	}
	return fixture, nil
}
