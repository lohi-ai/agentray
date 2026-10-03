package connector

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
