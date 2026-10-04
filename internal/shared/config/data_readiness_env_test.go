package config

import (
	"testing"
	"time"
)

func TestDataReadinessEnv(t *testing.T) {
	t.Setenv("SOURCE_FRESHNESS_MAX_AGE", "90m")
	t.Setenv("SOURCE_STAGING_TTL_DAYS", "3")
	t.Setenv("DATA_DISK_RESERVE_BYTES", "12345")
	t.Setenv("INGEST_STREAM_MAX_BYTES", "67890")
	cfg := FromEnv()
	if cfg.SourceFreshnessMaxAge != 90*time.Minute {
		t.Fatalf("freshness = %v", cfg.SourceFreshnessMaxAge)
	}
	if cfg.SourceStagingTTL != 72*time.Hour {
		t.Fatalf("staging ttl = %v", cfg.SourceStagingTTL)
	}
	if cfg.DataDiskReserveBytes != 12345 {
		t.Fatalf("reserve = %d", cfg.DataDiskReserveBytes)
	}
	if cfg.IngestStreamMaxBytes != 67890 {
		t.Fatalf("stream max = %d", cfg.IngestStreamMaxBytes)
	}
}

func TestDataReadinessEnvDefaults(t *testing.T) {
	for _, key := range []string{"SOURCE_FRESHNESS_MAX_AGE", "SOURCE_STAGING_TTL_DAYS", "DATA_DISK_RESERVE_BYTES", "INGEST_STREAM_MAX_BYTES"} {
		t.Setenv(key, "")
	}
	cfg := FromEnv()
	if cfg.SourceFreshnessMaxAge != 0 || cfg.SourceStagingTTL != 7*24*time.Hour || cfg.DataDiskReserveBytes != 4<<30 || cfg.IngestStreamMaxBytes != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
}
