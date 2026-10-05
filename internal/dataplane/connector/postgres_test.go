package connector

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPGXResolvedHostnameUsesApprovedDestination(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://probe@localhost:15432/probe?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	policy := &SourcePolicy{Destinations: []SourceDestination{{Host: "localhost", Port: 15432, AllowedIPCIDRs: []string{"127.0.0.0/8", "::1/128"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := admitPostgresConfig(ctx, cfg, policy); err != nil {
		t.Fatal(err)
	}
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	original := cfg.DialFunc
	seen := ""
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		seen = address
		return original(ctx, network, address)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if conn != nil {
		_ = conn.Close(ctx)
	}
	if seen != "127.0.0.1:15432" || err == nil || strings.Contains(err.Error(), "source destination is not approved") {
		t.Fatalf("resolved address=%q error=%v", seen, err)
	}
}

// The DSN embeds the password, so no error surfaced to the UI or persisted on
// the sync row may ever contain it.
func TestSanitizePGErrorNeverLeaksPassword(t *testing.T) {
	err := fmt.Errorf("failed to connect to `host=db.internal user=app password=s3cr3t`: dial tcp: connection refused")
	got := sanitizePGError(err, "s3cr3t")
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("sanitized error leaked the password: %q", got)
	}
	if got != "dial tcp: connection refused" {
		t.Fatalf("sanitized error = %q, want the root cause only", got)
	}
}

func TestSanitizePGErrorTruncates(t *testing.T) {
	got := sanitizePGError(fmt.Errorf("%s", strings.Repeat("x", 1000)), "")
	if len(got) != 300 {
		t.Fatalf("len = %d, want 300-char cap", len(got))
	}
}

func TestSanitizePGErrorDoesNotExposeSourceValues(t *testing.T) {
	err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type bigint: "person@example.com"`}
	got := sanitizePGError(err, "")
	if strings.Contains(got, "person@example.com") || got != "database request failed (SQLSTATE 22P02)" {
		t.Fatalf("unsafe PostgreSQL error = %q", got)
	}
}

func TestAdmitPostgresConfigAssociatesResolvedHostnameWithDial(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://user:pass@localhost:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	policy := &SourcePolicy{Version: SourcePolicyVersion, Destinations: []SourceDestination{{Host: "localhost", Port: 1, AllowedIPCIDRs: []string{"127.0.0.0/8", "::1/128"}}}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := admitPostgresConfig(context.Background(), cfg, policy); err != nil {
		t.Fatal(err)
	}
	addrs, err := cfg.LookupFunc(context.Background(), "localhost")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("lookup addrs=%v err=%v", addrs, err)
	}
	address := net.JoinHostPort(addrs[0], "1")
	conn, err := cfg.DialFunc(context.Background(), "tcp", address)
	if conn != nil {
		conn.Close()
	}
	if err != nil && strings.Contains(err.Error(), "not approved") {
		t.Fatalf("resolved approved hostname was rejected at dial: %v", err)
	}
	if _, err := cfg.DialFunc(context.Background(), "tcp", net.JoinHostPort("192.0.2.1", "1")); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unassociated address was not rejected: %v", err)
	}
}

// A malformed DSN must fail with a generic message: pgx's ParseConfig error
// echoes the raw connection string, which may embed credentials.
func TestOpenPostgresInvalidDSNIsGeneric(t *testing.T) {
	_, err := openPostgres(context.Background(), "postgres://user:hunter2@bad host:not-a-port/db")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked the password: %q", err)
	}
}

func TestQuoteQualified(t *testing.T) {
	got, err := quoteQualified(`billing.inv"oices`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `"billing"."inv""oices"` {
		t.Fatalf("quoted = %s", got)
	}
	if _, err := quoteQualified(""); err == nil {
		t.Fatal("empty table name must be rejected")
	}
}
