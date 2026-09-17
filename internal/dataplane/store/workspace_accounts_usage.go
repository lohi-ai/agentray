package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpdateProviderAccountUsage stores the parsed vendor usage summary on the
// account row (written by the oauth usage probe). usage is marshaled to the
// JSONB `usage` column; nil clears it to '{}'.
func (s *Store) UpdateProviderAccountUsage(ctx context.Context, accountID string, usage map[string]any) error {
	if usage == nil {
		usage = map[string]any{}
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts
SET usage = $2, updated_at = now()
WHERE id = $1`, accountID, usage)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// TouchProviderAccount stamps last_used_at after a request the account served
// successfully. Deliberately does not bump updated_at — last_used_at is a
// high-frequency liveness stamp, not a record mutation.
func (s *Store) TouchProviderAccount(ctx context.Context, accountID string) error {
	_, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts
SET last_used_at = $2
WHERE id = $1`, accountID, time.Now())
	return err
}
