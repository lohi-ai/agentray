package storage

import (
	"context"
	"github.com/lohi-ai/agentray/internal/shared/config"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateDoesNotOpenDuckDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist.duckdb")
	err := Migrate(context.Background(), config.Config{PostgresURL: "://invalid", DuckDBPath: path})
	if err == nil {
		t.Fatal("invalid PostgreSQL config must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("migration touched instance database")
	}
}
