package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	SourcePolicyVersion         = 1
	RelationKindView            = "view"
	RelationKindLegacyTable     = "legacy_table"
	KeyStabilityImmutableUnique = "immutable_unique_non_null"
)

var ErrSourcePolicyDenied = errors.New("source access is not approved by operator policy")
var ErrSnapshotKeyInvalid = errors.New("snapshot key or export schema is invalid")

// SourcePolicy is immutable process configuration. It names network
// destinations separately from relation bindings so neither a stored DSN nor
// a caller-controlled identifier can widen access.
type SourcePolicy struct {
	Version      int                 `json:"version"`
	Destinations []SourceDestination `json:"destinations"`
	Bindings     []SourceBinding     `json:"bindings"`
}

type SourceDestination struct {
	Host           string   `json:"host"`
	Port           uint16   `json:"port"`
	AllowedIPCIDRs []string `json:"allowed_ip_cidrs"`
}

type SourceBinding struct {
	ProjectID            string               `json:"project_id"`
	ConnectorID          string               `json:"connector_id"`
	Schema               string               `json:"schema"`
	Relation             string               `json:"relation"`
	RelationKind         string               `json:"relation_kind"`
	Columns              []SourcePolicyColumn `json:"columns"`
	KeyColumn            string               `json:"key_column"`
	KeyStability         string               `json:"key_stability"`
	AllowedCursorColumns []string             `json:"allowed_cursor_columns"`
	LegacySyncIDs        []string             `json:"legacy_sync_ids"`
}

type SourcePolicyColumn struct {
	Name   string `json:"name"`
	PGType string `json:"pg_type"`
}

func LoadSourcePolicy(path string) (*SourcePolicy, error) {
	if strings.TrimSpace(path) == "" {
		return &SourcePolicy{Version: SourcePolicyVersion}, nil
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("source policy: path must be absolute")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("source policy: read file: %w", err)
	}
	var p SourcePolicy
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("source policy: invalid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("source policy: invalid trailing JSON")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *SourcePolicy) Validate() error {
	if p == nil || p.Version != SourcePolicyVersion {
		return fmt.Errorf("source policy: version must be %d", SourcePolicyVersion)
	}
	seenDest := map[string]struct{}{}
	for i, d := range p.Destinations {
		d.Host = strings.TrimSpace(strings.ToLower(d.Host))
		if d.Host == "" || d.Port == 0 || strings.HasPrefix(d.Host, "/") {
			return fmt.Errorf("source policy: destination %d must have an exact host and port", i)
		}
		if len(d.AllowedIPCIDRs) == 0 {
			return fmt.Errorf("source policy: destination %s:%d has no allowed IP ranges", d.Host, d.Port)
		}
		for _, raw := range d.AllowedIPCIDRs {
			if _, _, err := net.ParseCIDR(raw); err != nil {
				return fmt.Errorf("source policy: destination %s:%d has invalid CIDR", d.Host, d.Port)
			}
		}
		key := net.JoinHostPort(d.Host, strconv.Itoa(int(d.Port)))
		if _, dup := seenDest[key]; dup {
			return fmt.Errorf("source policy: duplicate destination %s", key)
		}
		seenDest[key] = struct{}{}
		p.Destinations[i] = d
	}
	seenBinding := map[string]struct{}{}
	for i := range p.Bindings {
		b := &p.Bindings[i]
		if strings.TrimSpace(b.ProjectID) == "" || strings.TrimSpace(b.ConnectorID) == "" ||
			strings.TrimSpace(b.Schema) == "" || strings.TrimSpace(b.Relation) == "" {
			return fmt.Errorf("source policy: binding %d has an empty identity", i)
		}
		if b.RelationKind != RelationKindView && b.RelationKind != RelationKindLegacyTable {
			return fmt.Errorf("source policy: binding %d has unsupported relation_kind", i)
		}
		if b.KeyStability != KeyStabilityImmutableUnique {
			return fmt.Errorf("source policy: binding %d must attest immutable_unique_non_null keys", i)
		}
		if len(b.Columns) == 0 || strings.TrimSpace(b.KeyColumn) == "" {
			return fmt.Errorf("source policy: binding %d must allow columns and a key", i)
		}
		cols := map[string]struct{}{}
		for _, c := range b.Columns {
			if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.PGType) == "" {
				return fmt.Errorf("source policy: binding %d has an incomplete column", i)
			}
			if !allowedSourcePGType(c.PGType) {
				return fmt.Errorf("source policy: binding %d column %q has an unsupported type", i, c.Name)
			}
			if _, dup := cols[c.Name]; dup {
				return fmt.Errorf("source policy: binding %d repeats column %q", i, c.Name)
			}
			cols[c.Name] = struct{}{}
		}
		if _, ok := cols[b.KeyColumn]; !ok {
			return fmt.Errorf("source policy: binding %d key is not allowlisted", i)
		}
		for _, c := range b.AllowedCursorColumns {
			if _, ok := cols[c]; !ok {
				return fmt.Errorf("source policy: binding %d cursor %q is not allowlisted", i, c)
			}
		}
		key := b.ProjectID + "\x00" + b.ConnectorID + "\x00" + b.Schema + "\x00" + b.Relation
		if _, dup := seenBinding[key]; dup {
			return fmt.Errorf("source policy: duplicate relation binding")
		}
		seenBinding[key] = struct{}{}
	}
	return nil
}

func allowedSourcePGType(raw string) bool {
	t := strings.ToLower(strings.TrimSpace(raw))
	for _, prefix := range []string{"character varying(", "character(", "numeric(", "decimal(", "timestamp(", "time("} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	switch t {
	case "smallint", "integer", "bigint", "real", "double precision", "numeric", "decimal", "boolean", "text", "character varying", "character", "uuid", "date", "timestamp without time zone", "timestamp with time zone", "time without time zone", "time with time zone", "json", "jsonb", "bytea":
		return true
	}
	return false
}

func (p *SourcePolicy) Binding(projectID, connectorID string) (*SourceBinding, error) {
	bindings, err := p.BindingsForConnector(projectID, connectorID)
	if err != nil {
		return nil, err
	}
	if len(bindings) != 1 {
		return nil, fmt.Errorf("%w: source relation is required", ErrSourcePolicyDenied)
	}
	return &bindings[0], nil
}

// BindingsForConnector returns every explicitly approved relation for a
// connector. A connector can legitimately own several existing table syncs;
// approval is therefore scoped by relation rather than connector alone.
func (p *SourcePolicy) BindingsForConnector(projectID, connectorID string) ([]SourceBinding, error) {
	if p == nil {
		return nil, ErrSourcePolicyDenied
	}
	var out []SourceBinding
	for i := range p.Bindings {
		if p.Bindings[i].ProjectID == projectID && p.Bindings[i].ConnectorID == connectorID {
			out = append(out, p.Bindings[i])
		}
	}
	if len(out) == 0 {
		return nil, ErrSourcePolicyDenied
	}
	return out, nil
}

func (p *SourcePolicy) BindingForRelation(projectID, connectorID, relation string) (*SourceBinding, error) {
	bindings, err := p.BindingsForConnector(projectID, connectorID)
	if err != nil {
		return nil, err
	}
	for i := range bindings {
		if bindings[i].MatchesRelation(relation) {
			return &bindings[i], nil
		}
	}
	return nil, ErrSourcePolicyDenied
}

func (p *SourcePolicy) Destination(host string, port uint16) (*SourceDestination, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for i := range p.Destinations {
		d := p.Destinations[i]
		if strings.TrimSuffix(strings.ToLower(d.Host), ".") == host && d.Port == port {
			return &d, nil
		}
	}
	return nil, ErrSourcePolicyDenied
}

func (b SourceBinding) QualifiedRelation() string { return b.Schema + "." + b.Relation }

// MatchesRelation preserves the pre-policy PostgreSQL identity for public
// tables. Legacy sync rows historically stored "users", while governed view
// bindings and non-public relations use the qualified "schema.relation" form.
func (b SourceBinding) MatchesRelation(name string) bool {
	if name == b.QualifiedRelation() {
		return true
	}
	return b.RelationKind == RelationKindLegacyTable && b.Schema == "public" && name == b.Relation
}

func (b SourceBinding) DisplayRelation() string {
	if b.RelationKind == RelationKindLegacyTable && b.Schema == "public" {
		return b.Relation
	}
	return b.QualifiedRelation()
}

func (b SourceBinding) AllowsColumn(name string) bool {
	for _, c := range b.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (b SourceBinding) AllowsCursor(name string) bool {
	for _, c := range b.AllowedCursorColumns {
		if c == name {
			return true
		}
	}
	return false
}

func (b SourceBinding) AllowsSync(syncID string) bool {
	if b.RelationKind == RelationKindView {
		return true
	}
	for _, id := range b.LegacySyncIDs {
		if id == syncID {
			return true
		}
	}
	return false
}

// Digest excludes destinations and secrets. It is stable across JSON field
// ordering and binds a generation to the immutable relation/key/mode shape.
func (b SourceBinding) Digest(mode string) string {
	type digestShape struct {
		ProjectID, ConnectorID, Schema, Relation, RelationKind, KeyColumn, KeyStability, Mode string
		Columns                                                                               []SourcePolicyColumn
		Cursors                                                                               []string
	}
	cols := append([]SourcePolicyColumn(nil), b.Columns...)
	cursors := append([]string(nil), b.AllowedCursorColumns...)
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	sort.Strings(cursors)
	raw, _ := json.Marshal(digestShape{b.ProjectID, b.ConnectorID, b.Schema, b.Relation, b.RelationKind, b.KeyColumn, b.KeyStability, mode, cols, cursors})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
