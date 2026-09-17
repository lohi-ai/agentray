package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoUsableAccount is returned by AcquireProviderAccount when a provider's
// pool has no account that can serve right now — none signed in, or every
// account disabled or still inside its blocked_until window.
var ErrNoUsableAccount = errors.New("no usable provider account")

// WorkspaceProviderAccount is the redacted public view of one pooled OAuth
// login (tokens never leave the store).
type WorkspaceProviderAccount struct {
	ID           string         `json:"id"`
	ProviderID   string         `json:"provider_id"`
	Email        string         `json:"email"`
	AccountID    string         `json:"account_id"`
	OrgID        string         `json:"org_id"`
	OrgName      string         `json:"org_name"`
	ProjectID    string         `json:"project_id"`
	Plan         string         `json:"plan"`
	Status       string         `json:"status"`
	BlockedUntil *time.Time     `json:"blocked_until"`
	LastUsedAt   *time.Time     `json:"last_used_at"`
	Usage        map[string]any `json:"usage"`
	CreatedAt    time.Time      `json:"created_at"`
}

// WorkspaceProviderAccountRecord is the persist/run shape: the public fields
// plus the decrypted tokens and the vendor/expiry the pool needs to refresh.
// AccessToken and RefreshToken are never serialized to JSON.
type WorkspaceProviderAccountRecord struct {
	ID            string
	ProviderID    string
	WorkspaceID   string
	Vendor        string
	Email         string
	AccountID     string
	OrgID         string
	OrgName       string
	ProjectID     string
	Plan          string
	AccessToken   string `json:"-"`
	RefreshToken  string `json:"-"`
	ExpiresAt     time.Time
	Status        string
	DisabledCause string
	BlockedUntil  *time.Time
	LastUsedAt    *time.Time
	Usage         map[string]any
	CreatedAt     time.Time
}

// Public returns the redacted account view.
func (r WorkspaceProviderAccountRecord) Public() WorkspaceProviderAccount {
	return WorkspaceProviderAccount{
		ID: r.ID, ProviderID: r.ProviderID, Email: r.Email,
		AccountID: r.AccountID, OrgID: r.OrgID, OrgName: r.OrgName,
		ProjectID: r.ProjectID, Plan: r.Plan, Status: r.Status,
		BlockedUntil: r.BlockedUntil, LastUsedAt: r.LastUsedAt,
		Usage: r.Usage, CreatedAt: r.CreatedAt,
	}
}

// ProviderAccountInput is the mutable subset accepted when a login completes.
// AccessToken/RefreshToken are stored encrypted at rest by the Store.
type ProviderAccountInput struct {
	Email        string
	AccountID    string
	OrgID        string
	OrgName      string
	ProjectID    string
	Plan         string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// accountSelect is the shared SELECT list for account rows; the vendor join
// feeds WorkspaceProviderAccountRecord.Vendor. accountFrom is the FROM/JOIN
// half; accountColumns is both for plain SELECTs, while AcquireProviderAccount
// reuses the select list over an updating CTE.
const accountSelect = `
SELECT a.id::text, a.provider_id::text, a.workspace_id::text, p.vendor,
       a.email, a.account_id, a.org_id, a.org_name, a.project_id, a.plan,
       a.access_token_ciphertext, a.refresh_token_ciphertext, a.expires_at,
       a.status, a.disabled_cause, a.blocked_until, a.last_used_at, a.usage, a.created_at
`

const accountFrom = `
FROM workspace_provider_accounts a
JOIN workspace_providers p ON p.id = a.provider_id`

const accountColumns = accountSelect + accountFrom

// scanAccountRecord reads one account row. decrypt=true unwraps the token
// ciphertexts (run path); false leaves them out (member-facing list).
func scanAccountRecord(scan func(dest ...any) error, decrypt bool) (WorkspaceProviderAccountRecord, error) {
	var rec WorkspaceProviderAccountRecord
	var accessCipher, refreshCipher string
	var expiresAt *time.Time
	err := scan(&rec.ID, &rec.ProviderID, &rec.WorkspaceID, &rec.Vendor,
		&rec.Email, &rec.AccountID, &rec.OrgID, &rec.OrgName, &rec.ProjectID, &rec.Plan,
		&accessCipher, &refreshCipher, &expiresAt,
		&rec.Status, &rec.DisabledCause, &rec.BlockedUntil, &rec.LastUsedAt, &rec.Usage, &rec.CreatedAt)
	if err != nil {
		return WorkspaceProviderAccountRecord{}, err
	}
	if expiresAt != nil {
		rec.ExpiresAt = *expiresAt
	}
	if decrypt && accessCipher != "" {
		plain, decErr := decryptAgentKey(accessCipher)
		if decErr != nil {
			return WorkspaceProviderAccountRecord{}, fmt.Errorf("account %s access token: %w", rec.ID, decErr)
		}
		rec.AccessToken = plain
	}
	if decrypt && refreshCipher != "" {
		plain, decErr := decryptAgentKey(refreshCipher)
		if decErr != nil {
			return WorkspaceProviderAccountRecord{}, fmt.Errorf("account %s refresh token: %w", rec.ID, decErr)
		}
		rec.RefreshToken = plain
	}
	return rec, nil
}

// ListProviderAccounts returns the redacted account pool of one provider for a
// workspace member.
func (s *Store) ListProviderAccounts(ctx context.Context, userID, workspaceID, providerID string) ([]WorkspaceProviderAccount, error) {
	member, err := s.userInWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrAgentForbidden
	}
	rows, err := s.pg.Query(ctx, accountColumns+`
WHERE a.provider_id = $1 AND a.workspace_id = $2
ORDER BY a.created_at ASC`, providerID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceProviderAccount
	for rows.Next() {
		rec, err := scanAccountRecord(rows.Scan, false)
		if err != nil {
			return nil, err
		}
		out = append(out, rec.Public())
	}
	return out, rows.Err()
}

// CreateProviderAccount adds a signed-in account to a provider's pool
// (owner/admin). Tokens are encrypted at rest.
func (s *Store) CreateProviderAccount(ctx context.Context, userID, workspaceID, providerID string, in ProviderAccountInput) (WorkspaceProviderAccount, error) {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return WorkspaceProviderAccount{}, err
	} else if !ok {
		return WorkspaceProviderAccount{}, ErrAgentForbidden
	}
	// The FK only proves the provider exists — pin it to this workspace so an
	// admin of workspace A cannot attach accounts to a provider owned by B.
	var wsID string
	err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM workspace_providers WHERE id = $1`, providerID).Scan(&wsID)
	if err != nil {
		return WorkspaceProviderAccount{}, err
	}
	if wsID != workspaceID {
		return WorkspaceProviderAccount{}, pgx.ErrNoRows
	}
	accessCipher := ""
	if in.AccessToken != "" {
		accessCipher, err = encryptAgentKey(in.AccessToken)
		if err != nil {
			return WorkspaceProviderAccount{}, err
		}
	}
	refreshCipher := ""
	if in.RefreshToken != "" {
		refreshCipher, err = encryptAgentKey(in.RefreshToken)
		if err != nil {
			return WorkspaceProviderAccount{}, err
		}
	}
	var expiresAt *time.Time
	if !in.ExpiresAt.IsZero() {
		expiresAt = &in.ExpiresAt
	}
	rec := WorkspaceProviderAccountRecord{
		ID: uuid.NewString(), ProviderID: providerID, WorkspaceID: workspaceID,
		Email: in.Email, AccountID: in.AccountID, OrgID: in.OrgID, OrgName: in.OrgName,
		ProjectID: in.ProjectID, Plan: in.Plan, Status: "active",
	}
	// Re-login is an upsert, not a second row: the same vendor account signing
	// in again must refresh the existing row's tokens — a duplicate would keep
	// the old (soon-rotated) refresh token, fail its next refresh, and sit in
	// the pool as a permanently disabled zombie. Match on the vendor account
	// id when the vendor supplies one, else on the email.
	var existingID string
	err = s.pg.QueryRow(ctx, `
SELECT id::text FROM workspace_provider_accounts
WHERE provider_id = $1 AND workspace_id = $2
  AND (($3 <> '' AND account_id = $3) OR ($3 = '' AND email <> '' AND email = $4))
LIMIT 1`, providerID, workspaceID, rec.AccountID, rec.Email).Scan(&existingID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceProviderAccount{}, err
	}
	if existingID != "" {
		rec.ID = existingID
		_, err = s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts SET
	email = $3, account_id = $4, org_id = $5, org_name = $6, project_id = $7, plan = $8,
	access_token_ciphertext = $9, refresh_token_ciphertext = $10, expires_at = $11,
	status = 'active', disabled_cause = '', blocked_until = NULL, updated_at = now()
WHERE id = $1 AND provider_id = $2`,
			existingID, providerID, rec.Email, rec.AccountID, rec.OrgID, rec.OrgName,
			rec.ProjectID, rec.Plan, accessCipher, refreshCipher, expiresAt)
		if err != nil {
			return WorkspaceProviderAccount{}, err
		}
		_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider_account.relogin",
			"workspace", workspaceID, firstNonEmpty(in.Email, in.AccountID, providerID), "{}")
		return rec.Public(), nil
	}
	err = s.pg.QueryRow(ctx, `
INSERT INTO workspace_provider_accounts (
	id, provider_id, workspace_id, email, account_id, org_id, org_name,
	project_id, plan, access_token_ciphertext, refresh_token_ciphertext, expires_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
RETURNING created_at`,
		rec.ID, providerID, workspaceID, rec.Email, rec.AccountID, rec.OrgID, rec.OrgName,
		rec.ProjectID, rec.Plan, accessCipher, refreshCipher, expiresAt).Scan(&rec.CreatedAt)
	if err != nil {
		return WorkspaceProviderAccount{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider_account.create",
		"workspace", workspaceID, firstNonEmpty(in.Email, in.AccountID, providerID), "{}")
	return rec.Public(), nil
}

// DeleteProviderAccount removes one account from the pool (owner/admin).
func (s *Store) DeleteProviderAccount(ctx context.Context, userID, workspaceID, providerID, accountID string) error {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return err
	} else if !ok {
		return ErrAgentForbidden
	}
	tag, err := s.pg.Exec(ctx, `
DELETE FROM workspace_provider_accounts
WHERE id = $1 AND provider_id = $2 AND workspace_id = $3`, accountID, providerID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider_account.delete",
		"workspace", workspaceID, accountID, "{}")
	return nil
}

// SetProviderAccountStatus flips an account between 'active' and 'disabled'
// (owner/admin). Re-activating clears the disable cause and any rate-limit
// block so the account re-enters rotation immediately.
func (s *Store) SetProviderAccountStatus(ctx context.Context, userID, workspaceID, providerID, accountID, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("status must be active or disabled")
	}
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return err
	} else if !ok {
		return ErrAgentForbidden
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts
SET status = $4,
    disabled_cause = CASE WHEN $4 = 'active' THEN ''
                          WHEN disabled_cause = '' THEN 'manual'
                          ELSE disabled_cause END,
    blocked_until = CASE WHEN $4 = 'active' THEN NULL ELSE blocked_until END,
    updated_at = now()
WHERE id = $1 AND provider_id = $2 AND workspace_id = $3`,
		accountID, providerID, workspaceID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider_account.status",
		"workspace", workspaceID, accountID, fmt.Sprintf(`{"status":%q}`, status))
	return nil
}

// AcquireProviderAccount picks the least-recently-used account that can serve
// right now — active and not inside a blocked_until window — stamps it used,
// and returns it with decrypted tokens. The stamp rides the same statement
// (FOR UPDATE SKIP LOCKED): without it every concurrent acquire lands on the
// same LRU head, and an account that keeps failing is re-picked forever
// because nothing ever advances its cursor. Run-path only; no membership
// gate (the caller already resolved the workspace). ErrNoUsableAccount when
// the pool is dry.
func (s *Store) AcquireProviderAccount(ctx context.Context, providerID string) (WorkspaceProviderAccountRecord, error) {
	rec, err := scanAccountRecord(s.pg.QueryRow(ctx, `
WITH picked AS (
	UPDATE workspace_provider_accounts SET last_used_at = now()
	WHERE id = (
		SELECT id FROM workspace_provider_accounts
		WHERE provider_id = $1 AND status = 'active'
		  AND (blocked_until IS NULL OR blocked_until < now())
		ORDER BY last_used_at ASC NULLS FIRST
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	RETURNING *
)`+accountSelect+`
FROM picked a
JOIN workspace_providers p ON p.id = a.provider_id`, providerID).Scan, true)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceProviderAccountRecord{}, ErrNoUsableAccount
		}
		return WorkspaceProviderAccountRecord{}, err
	}
	return rec, nil
}

// LoadProviderAccountRecord returns one account with decrypted tokens, keyed
// by account row id. Used by the usage probe and refresh paths that already
// hold the account id; no membership gate.
func (s *Store) LoadProviderAccountRecord(ctx context.Context, accountID string) (WorkspaceProviderAccountRecord, error) {
	return scanAccountRecord(s.pg.QueryRow(ctx, accountColumns+`
WHERE a.id = $1`, accountID).Scan, true)
}

// UpdateProviderAccountTokens stores refreshed tokens (re-encrypted at rest).
// Empty access/refresh keep the stored value; a zero expiresAt keeps the
// stored expiry — the refresh path always has a full new grant.
func (s *Store) UpdateProviderAccountTokens(ctx context.Context, accountID, access, refresh string, expiresAt time.Time) error {
	var accessCipher, refreshCipher any
	if access != "" {
		ct, err := encryptAgentKey(access)
		if err != nil {
			return err
		}
		accessCipher = ct
	}
	if refresh != "" {
		ct, err := encryptAgentKey(refresh)
		if err != nil {
			return err
		}
		refreshCipher = ct
	}
	var expiresArg any
	if !expiresAt.IsZero() {
		expiresArg = expiresAt
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts
SET access_token_ciphertext = COALESCE($2, access_token_ciphertext),
    refresh_token_ciphertext = COALESCE($3, refresh_token_ciphertext),
    expires_at = COALESCE($4, expires_at),
    updated_at = now()
WHERE id = $1`, accountID, accessCipher, refreshCipher, expiresArg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// BlockProviderAccount marks an account rate-limited until the given time;
// Acquire skips it until then. Run-path only.
func (s *Store) BlockProviderAccount(ctx context.Context, accountID string, until time.Time) error {
	tag, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts SET blocked_until = $2, updated_at = now()
WHERE id = $1`, accountID, until)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DisableProviderAccount removes an account from rotation permanently (until
// re-enabled via SetProviderAccountStatus), recording why. Run-path only.
func (s *Store) DisableProviderAccount(ctx context.Context, accountID, cause string) error {
	tag, err := s.pg.Exec(ctx, `
UPDATE workspace_provider_accounts
SET status = 'disabled', disabled_cause = $2, updated_at = now()
WHERE id = $1`, accountID, cause)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
