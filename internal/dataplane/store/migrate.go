package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// Migrate applies the shared PostgreSQL schema without opening DuckDB, starting
// ingestion, or serving HTTP. Per-colour DuckDB schema remains local startup work.
func Migrate(ctx context.Context, cfg config.Config) error {
	pgCfg, err := pgxpool.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return err
	}
	pgCfg.MaxConns = 1
	pgCfg.ConnConfig.ConnectTimeout = 15 * time.Second
	if pgCfg.ConnConfig.RuntimeParams == nil {
		pgCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pgCfg.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	pgCfg.ConnConfig.RuntimeParams["statement_timeout"] = "0"
	pg, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		return err
	}
	defer pg.Close()
	store := &Store{pg: pg, hostModel: HostModelDefaultsFromConfig(cfg)}
	return store.migrate(ctx, cfg)
}
