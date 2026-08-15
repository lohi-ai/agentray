package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/ai"
)

// WorkspaceProvider is the redacted public view of one configured vendor
// (key never returned — only HasKey).
type WorkspaceProvider struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Vendor      string `json:"vendor"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	HasKey      bool   `json:"has_key"`
}

// WorkspaceProviderRecord is the persist/run shape. APIKey is the decrypted
// secret and is never serialized to JSON.
type WorkspaceProviderRecord struct {
	ID          string
	WorkspaceID string
	Vendor      string
	Name        string
	BaseURL     string
	APIKey      string
	HasKey      bool
}

// WorkspaceProviderInput is the mutable subset accepted from an owner/admin.
// APIKey: empty leaves the stored key unchanged (updates only), "-" clears it,
// any other value is stored (encrypted at rest by the Store).
type WorkspaceProviderInput struct {
	Vendor  string
	Name    string
	BaseURL string
	APIKey  string
}

// WorkspaceTierSelection is the 3-tier pointer into configured providers.
// A blank lite/pro provider+model inherits flash at resolve time.
type WorkspaceTierSelection struct {
	FlashProviderID string
	FlashModel      string
	LiteProviderID  string
	LiteModel       string
	ProProviderID   string
	ProModel        string
	ModelFallback   bool
	// Per-tier context-window overrides in tokens; 0 means "derive it from the
	// model id". They sit beside the model rather than on the provider because
	// the window is a property of the model, and one provider serves several.
	FlashContextWindow int
	LiteContextWindow  int
	ProContextWindow   int
}

// LegacyWorkspaceTiers is a pre-upgrade one-row-per-workspace tier record
// (provider+key columns on workspace_model_tiers). Dual-read and backfill
// both go through ApplyLegacyProviders so keys are not wiped.
type LegacyWorkspaceTiers struct {
	Provider      string
	Model         string
	BaseURL       string
	FlashKey      string
	LiteProvider  string
	LiteModel     string
	LiteBaseURL   string
	LiteKey       string
	ProProvider   string
	ProModel      string
	ProBaseURL    string
	ProKey        string
	ModelFallback bool
}

// WorkspaceProviderBook is the workspace's configured providers + 3-tier
// selection. Store methods serialize this to Postgres; persist + resolve
// tests drive the same type so the path under test is the shipped one.
type WorkspaceProviderBook struct {
	WorkspaceID string
	Providers   []WorkspaceProviderRecord
	Sel         WorkspaceTierSelection
}

// Public returns the redacted provider list.
func (b *WorkspaceProviderBook) Public() []WorkspaceProvider {
	return publicProviders(b.Providers)
}

func publicProviders(recs []WorkspaceProviderRecord) []WorkspaceProvider {
	out := make([]WorkspaceProvider, 0, len(recs))
	for _, r := range recs {
		name := r.Name
		if name == "" {
			name = r.Vendor
		}
		out = append(out, WorkspaceProvider{
			ID: r.ID, WorkspaceID: r.WorkspaceID, Vendor: r.Vendor,
			Name: name, BaseURL: r.BaseURL, HasKey: r.HasKey || r.APIKey != "",
		})
	}
	return out
}

func (b *WorkspaceProviderBook) byID() map[string]WorkspaceProviderRecord {
	m := make(map[string]WorkspaceProviderRecord, len(b.Providers))
	for _, p := range b.Providers {
		m[p.ID] = p
	}
	return m
}

// UpsertProvider adds a provider or replaces the record with the same ID.
func (b *WorkspaceProviderBook) UpsertProvider(rec WorkspaceProviderRecord) {
	if rec.WorkspaceID == "" {
		rec.WorkspaceID = b.WorkspaceID
	}
	for i, p := range b.Providers {
		if p.ID == rec.ID {
			b.Providers[i] = rec
			return
		}
	}
	b.Providers = append(b.Providers, rec)
}

// DeleteProvider removes a provider and clears any tier that pointed at it.
func (b *WorkspaceProviderBook) DeleteProvider(id string) {
	kept := b.Providers[:0]
	for _, p := range b.Providers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	b.Providers = kept
	if b.Sel.FlashProviderID == id {
		b.Sel.FlashProviderID = ""
	}
	if b.Sel.LiteProviderID == id {
		b.Sel.LiteProviderID = ""
	}
	if b.Sel.ProProviderID == id {
		b.Sel.ProProviderID = ""
	}
}

// SetTiers records the 3-tier selection. Unknown provider ids are rejected
// (blank is allowed — inherit).
func (b *WorkspaceProviderBook) SetTiers(sel WorkspaceTierSelection) error {
	known := b.byID()
	for _, id := range []string{sel.FlashProviderID, sel.LiteProviderID, sel.ProProviderID} {
		if id == "" {
			continue
		}
		if _, ok := known[id]; !ok {
			return fmt.Errorf("unknown provider %q", id)
		}
	}
	b.Sel = sel
	return nil
}

// Resolve maps the book onto the runtime WorkspaceModelTiers + per-tier keys
// the runner already consumes. Blank lite/pro inherit flash (provider+model+key).
func (b *WorkspaceProviderBook) Resolve() (WorkspaceModelTiers, map[string]string) {
	return ResolveWorkspaceRun(b.Providers, b.Sel)
}

// NewWorkspaceProviderRecord builds a record from user input. id empty → new UUID.
func NewWorkspaceProviderRecord(id, workspaceID string, in WorkspaceProviderInput, existing *WorkspaceProviderRecord) (WorkspaceProviderRecord, error) {
	vendor := ai.NormalizeVendor(in.Vendor)
	if vendor == "" {
		vendor = "openai"
	}
	base := strings.TrimSpace(in.BaseURL)
	if vendor != "openai" && vendor != "anthropic" && vendor != "google" && base == "" {
		return WorkspaceProviderRecord{}, fmt.Errorf("provider %q requires a base URL", vendor)
	}
	rec := WorkspaceProviderRecord{
		ID:          id,
		WorkspaceID: workspaceID,
		Vendor:      vendor,
		Name:        strings.TrimSpace(in.Name),
		BaseURL:     base,
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	if rec.Name == "" {
		rec.Name = vendor
	}
	switch strings.TrimSpace(in.APIKey) {
	case "":
		if existing != nil {
			rec.APIKey = existing.APIKey
			rec.HasKey = existing.HasKey || existing.APIKey != ""
		}
	case "-":
		rec.APIKey = ""
		rec.HasKey = false
	default:
		rec.APIKey = strings.TrimSpace(in.APIKey)
		rec.HasKey = true
	}
	return rec, nil
}

// ApplyLegacyProviders turns a pre-upgrade one-row-per-workspace tier record
// into provider rows + a tier selection. Distinct lite/pro credentials become
// their own providers; a model-only override reuses the flash provider.
func ApplyLegacyProviders(workspaceID string, row LegacyWorkspaceTiers) (*WorkspaceProviderBook, error) {
	book := &WorkspaceProviderBook{WorkspaceID: workspaceID, Sel: WorkspaceTierSelection{ModelFallback: row.ModelFallback}}
	newRec := func(vendor, name, base, key string) WorkspaceProviderRecord {
		if vendor == "" {
			vendor = "openai"
		}
		vendor = ai.NormalizeVendor(vendor)
		if name == "" {
			name = vendor
		}
		return WorkspaceProviderRecord{
			ID: uuid.NewString(), WorkspaceID: workspaceID,
			Vendor: vendor, Name: name, BaseURL: strings.TrimSpace(base),
			APIKey: key, HasKey: key != "",
		}
	}

	hasFlash := strings.TrimSpace(row.Provider) != "" || row.Model != "" || row.BaseURL != "" || row.FlashKey != ""
	if hasFlash {
		flash := newRec(row.Provider, row.Provider, row.BaseURL, row.FlashKey)
		book.Providers = append(book.Providers, flash)
		book.Sel.FlashProviderID = flash.ID
		book.Sel.FlashModel = row.Model
	}

	liteOwn := row.LiteKey != "" ||
		(strings.TrimSpace(row.LiteProvider) != "" && ai.NormalizeVendor(row.LiteProvider) != ai.NormalizeVendor(row.Provider)) ||
		(strings.TrimSpace(row.LiteBaseURL) != "" && strings.TrimSpace(row.LiteBaseURL) != strings.TrimSpace(row.BaseURL))
	if liteOwn {
		lite := newRec(firstNonEmpty(row.LiteProvider, row.Provider), firstNonEmpty(row.LiteProvider, row.Provider), firstNonEmpty(row.LiteBaseURL, row.BaseURL), firstNonEmpty(row.LiteKey, row.FlashKey))
		book.Providers = append(book.Providers, lite)
		book.Sel.LiteProviderID = lite.ID
		book.Sel.LiteModel = row.LiteModel
	} else if row.LiteModel != "" && hasFlash {
		book.Sel.LiteProviderID = book.Sel.FlashProviderID
		book.Sel.LiteModel = row.LiteModel
	}

	proOwn := row.ProKey != "" ||
		(strings.TrimSpace(row.ProProvider) != "" && ai.NormalizeVendor(row.ProProvider) != ai.NormalizeVendor(row.Provider)) ||
		(strings.TrimSpace(row.ProBaseURL) != "" && strings.TrimSpace(row.ProBaseURL) != strings.TrimSpace(row.BaseURL))
	if proOwn {
		pro := newRec(firstNonEmpty(row.ProProvider, row.Provider), firstNonEmpty(row.ProProvider, row.Provider), firstNonEmpty(row.ProBaseURL, row.BaseURL), firstNonEmpty(row.ProKey, row.FlashKey))
		book.Providers = append(book.Providers, pro)
		book.Sel.ProProviderID = pro.ID
		book.Sel.ProModel = row.ProModel
	} else if row.ProModel != "" && hasFlash {
		book.Sel.ProProviderID = book.Sel.FlashProviderID
		book.Sel.ProModel = row.ProModel
	}
	return book, nil
}

// ResolveWorkspaceRun is the shipped run-path mapper: each selected tier
// uses its provider's vendor, base URL, and key. Blank lite/pro inherit flash.
func ResolveWorkspaceRun(providers []WorkspaceProviderRecord, sel WorkspaceTierSelection) (WorkspaceModelTiers, map[string]string) {
	byID := make(map[string]WorkspaceProviderRecord, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}
	pick := func(providerID, model string) (vendor, base, key, mid string, hasKey bool) {
		if providerID == "" && model == "" {
			return "", "", "", "", false
		}
		p, ok := byID[providerID]
		if !ok {
			return "", "", "", model, false
		}
		// HasKey is set from ciphertext presence when the book is loaded
		// redacted (GET). APIKey is set only on the decrypt/run path. Either
		// counts so a BYOK workspace is not treated as empty.
		return p.Vendor, p.BaseURL, p.APIKey, model, p.HasKey || p.APIKey != ""
	}

	fv, fb, fk, fm, fh := pick(sel.FlashProviderID, sel.FlashModel)
	lv, lb, lk, lm, lh := pick(sel.LiteProviderID, sel.LiteModel)
	pv, pb, pk, pm, ph := pick(sel.ProProviderID, sel.ProModel)

	cfg := WorkspaceModelTiers{
		Provider: fv, Model: fm, BaseURL: fb, HasKey: fh, ContextWindow: sel.FlashContextWindow,
		LiteProvider: lv, LiteModel: lm, LiteBaseURL: lb, LiteHasKey: lh, LiteContextWindow: sel.LiteContextWindow,
		ProProvider: pv, ProModel: pm, ProBaseURL: pb, ProHasKey: ph, ProContextWindow: sel.ProContextWindow,
		ModelFallback:   sel.ModelFallback,
		FlashProviderID: sel.FlashProviderID,
		LiteProviderID:  sel.LiteProviderID,
		ProProviderID:   sel.ProProviderID,
		Providers:       publicProviders(providers),
	}
	if cfg.Provider == "" && len(providers) > 0 {
		cfg.Provider = providers[0].Vendor
	}
	keys := map[string]string{}
	if fk != "" {
		keys["flash"] = fk
	}
	if lk != "" {
		keys["lite"] = lk
	}
	if pk != "" {
		keys["pro"] = pk
	}
	return cfg, keys
}

// --- Store persistence -------------------------------------------------------

func (s *Store) backfillWorkspaceProviders(ctx context.Context) error {
	rows, err := s.pg.Query(ctx, `
SELECT t.workspace_id::text, t.provider, t.model, t.base_url, t.api_key_ciphertext,
       t.lite_provider, t.lite_model, t.lite_base_url, t.lite_api_key_ciphertext,
       t.pro_provider, t.pro_model, t.pro_base_url, t.pro_api_key_ciphertext, t.model_fallback
FROM workspace_model_tiers t
WHERE NOT EXISTS (SELECT 1 FROM workspace_providers p WHERE p.workspace_id = t.workspace_id)
  AND (t.api_key_ciphertext <> '' OR t.model <> '' OR t.provider <> '' OR t.lite_model <> '' OR t.pro_model <> '')`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type legacyRow struct {
		wsID string
		leg  LegacyWorkspaceTiers
	}
	var pending []legacyRow
	for rows.Next() {
		var r legacyRow
		var flashC, liteC, proC string
		if err := rows.Scan(&r.wsID, &r.leg.Provider, &r.leg.Model, &r.leg.BaseURL, &flashC,
			&r.leg.LiteProvider, &r.leg.LiteModel, &r.leg.LiteBaseURL, &liteC,
			&r.leg.ProProvider, &r.leg.ProModel, &r.leg.ProBaseURL, &proC, &r.leg.ModelFallback); err != nil {
			return err
		}
		// Keep ciphertext as the "key" for the backfill insert — we re-store it
		// verbatim so existing BYOK keys are not rotated or wiped.
		r.leg.FlashKey = flashC
		r.leg.LiteKey = liteC
		r.leg.ProKey = proC
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range pending {
		if err := s.insertLegacyAsProviders(ctx, r.wsID, r.leg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertLegacyAsProviders(ctx context.Context, wsID string, leg LegacyWorkspaceTiers) error {
	// Ciphertexts are already in the Key fields (see backfill). ApplyLegacy
	// copies them onto each provider's APIKey; we write that value back as
	// ciphertext (no re-encrypt) so existing BYOK keys survive the upgrade.
	book, err := ApplyLegacyProviders(wsID, leg)
	if err != nil {
		return err
	}
	for i := range book.Providers {
		if _, err := s.pg.Exec(ctx, `
INSERT INTO workspace_providers (id, workspace_id, vendor, name, base_url, api_key_ciphertext)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (id) DO NOTHING`,
			book.Providers[i].ID, wsID, book.Providers[i].Vendor, book.Providers[i].Name,
			book.Providers[i].BaseURL, book.Providers[i].APIKey); err != nil {
			return err
		}
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_model_tiers
SET flash_provider_id = NULLIF($2,'')::uuid,
    lite_provider_id  = NULLIF($3,'')::uuid,
    pro_provider_id   = NULLIF($4,'')::uuid,
    model             = $5,
    lite_model        = $6,
    pro_model         = $7
WHERE workspace_id = $1`,
		wsID, book.Sel.FlashProviderID, book.Sel.LiteProviderID, book.Sel.ProProviderID,
		book.Sel.FlashModel, book.Sel.LiteModel, book.Sel.ProModel); err != nil {
		return err
	}
	return nil
}

// LoadWorkspaceBook returns the workspace provider book. decrypt=true is the
// run / list-models path (keys in memory only).
func (s *Store) LoadWorkspaceBook(ctx context.Context, workspaceID string, decrypt bool) (*WorkspaceProviderBook, error) {
	return s.loadBook(ctx, workspaceID, decrypt)
}

// ListWorkspaceProviders returns the redacted provider list for a member.
func (s *Store) ListWorkspaceProviders(ctx context.Context, userID, workspaceID string) ([]WorkspaceProvider, error) {
	member, err := s.userInWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errAgentForbidden
	}
	book, err := s.loadBook(ctx, workspaceID, false)
	if err != nil {
		return nil, err
	}
	return book.Public(), nil
}

// CreateWorkspaceProvider inserts a configured vendor (owner/admin).
func (s *Store) CreateWorkspaceProvider(ctx context.Context, userID, workspaceID string, in WorkspaceProviderInput) (WorkspaceProvider, error) {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return WorkspaceProvider{}, err
	} else if !ok {
		return WorkspaceProvider{}, errAgentForbidden
	}
	rec, err := NewWorkspaceProviderRecord("", workspaceID, in, nil)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	cipher := ""
	if rec.APIKey != "" {
		cipher, err = encryptAgentKey(rec.APIKey)
		if err != nil {
			return WorkspaceProvider{}, err
		}
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO workspace_providers (id, workspace_id, vendor, name, base_url, api_key_ciphertext)
VALUES ($1,$2,$3,$4,$5,$6)`,
		rec.ID, workspaceID, rec.Vendor, rec.Name, rec.BaseURL, cipher); err != nil {
		return WorkspaceProvider{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider.create", "workspace", workspaceID, rec.Vendor, "{}")
	rec.APIKey = ""
	rec.HasKey = cipher != ""
	return publicProviders([]WorkspaceProviderRecord{rec})[0], nil
}

// UpdateWorkspaceProvider patches a configured vendor (owner/admin).
func (s *Store) UpdateWorkspaceProvider(ctx context.Context, userID, workspaceID, providerID string, in WorkspaceProviderInput) (WorkspaceProvider, error) {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return WorkspaceProvider{}, err
	} else if !ok {
		return WorkspaceProvider{}, errAgentForbidden
	}
	existing, err := s.loadProviderRecord(ctx, workspaceID, providerID, false)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	rec, err := NewWorkspaceProviderRecord(providerID, workspaceID, in, &existing)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	cipherArg, err := resolveCipherArg(in.APIKey)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_providers
SET vendor = $3, name = $4, base_url = $5,
    api_key_ciphertext = COALESCE($6, api_key_ciphertext),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2`,
		providerID, workspaceID, rec.Vendor, rec.Name, rec.BaseURL, cipherArg); err != nil {
		return WorkspaceProvider{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider.update", "workspace", workspaceID, rec.Vendor, "{}")
	updated, err := s.loadProviderRecord(ctx, workspaceID, providerID, false)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	return publicProviders([]WorkspaceProviderRecord{updated})[0], nil
}

// DeleteWorkspaceProvider removes a configured vendor and clears tier refs.
func (s *Store) DeleteWorkspaceProvider(ctx context.Context, userID, workspaceID, providerID string) error {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return err
	} else if !ok {
		return errAgentForbidden
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_model_tiers SET
	flash_provider_id = CASE WHEN flash_provider_id::text = $2 THEN NULL ELSE flash_provider_id END,
	lite_provider_id  = CASE WHEN lite_provider_id::text  = $2 THEN NULL ELSE lite_provider_id END,
	pro_provider_id   = CASE WHEN pro_provider_id::text   = $2 THEN NULL ELSE pro_provider_id END
WHERE workspace_id = $1`, workspaceID, providerID); err != nil {
		return err
	}
	tag, err := s.pg.Exec(ctx, `DELETE FROM workspace_providers WHERE id = $1 AND workspace_id = $2`, providerID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_provider.delete", "workspace", workspaceID, providerID, "{}")
	return nil
}

func (s *Store) loadProviderRecord(ctx context.Context, workspaceID, providerID string, decrypt bool) (WorkspaceProviderRecord, error) {
	var rec WorkspaceProviderRecord
	var cipher string
	err := s.pg.QueryRow(ctx, `
SELECT id::text, workspace_id::text, vendor, name, base_url, api_key_ciphertext
FROM workspace_providers WHERE id = $1 AND workspace_id = $2`, providerID, workspaceID).Scan(
		&rec.ID, &rec.WorkspaceID, &rec.Vendor, &rec.Name, &rec.BaseURL, &cipher)
	if err != nil {
		return WorkspaceProviderRecord{}, err
	}
	rec.HasKey = cipher != ""
	if decrypt && cipher != "" {
		plain, decErr := decryptAgentKey(cipher)
		if decErr != nil {
			return WorkspaceProviderRecord{}, decErr
		}
		rec.APIKey = plain
	}
	return rec, nil
}

func (s *Store) loadBook(ctx context.Context, workspaceID string, decrypt bool) (*WorkspaceProviderBook, error) {
	book := &WorkspaceProviderBook{WorkspaceID: workspaceID, Sel: WorkspaceTierSelection{ModelFallback: true}}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, workspace_id::text, vendor, name, base_url, api_key_ciphertext
FROM workspace_providers WHERE workspace_id = $1 ORDER BY created_at ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec WorkspaceProviderRecord
		var cipher string
		if err := rows.Scan(&rec.ID, &rec.WorkspaceID, &rec.Vendor, &rec.Name, &rec.BaseURL, &cipher); err != nil {
			return nil, err
		}
		rec.HasKey = cipher != ""
		if decrypt && cipher != "" {
			plain, decErr := decryptAgentKey(cipher)
			if decErr != nil {
				return nil, decErr
			}
			rec.APIKey = plain
		}
		book.Providers = append(book.Providers, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var flashID, liteID, proID *string
	var flashModel, liteModel, proModel string
	var fallback bool
	var flashWindow, liteWindow, proWindow int
	err = s.pg.QueryRow(ctx, `
SELECT flash_provider_id::text, model, lite_provider_id::text, lite_model,
       pro_provider_id::text, pro_model, model_fallback,
       context_window, lite_context_window, pro_context_window
FROM workspace_model_tiers WHERE workspace_id = $1`, workspaceID).Scan(
		&flashID, &flashModel, &liteID, &liteModel, &proID, &proModel, &fallback,
		&flashWindow, &liteWindow, &proWindow)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		book.Sel.ModelFallback = fallback
		book.Sel.FlashModel = flashModel
		book.Sel.LiteModel = liteModel
		book.Sel.ProModel = proModel
		book.Sel.FlashContextWindow = flashWindow
		book.Sel.LiteContextWindow = liteWindow
		book.Sel.ProContextWindow = proWindow
		if flashID != nil {
			book.Sel.FlashProviderID = *flashID
		}
		if liteID != nil {
			book.Sel.LiteProviderID = *liteID
		}
		if proID != nil {
			book.Sel.ProProviderID = *proID
		}
	}

	if len(book.Providers) == 0 {
		legacy, lerr := s.readLegacyTierRow(ctx, workspaceID, decrypt)
		if lerr != nil {
			return nil, lerr
		}
		if legacy != nil {
			return ApplyLegacyProviders(workspaceID, *legacy)
		}
	}
	return book, nil
}

func (s *Store) readLegacyTierRow(ctx context.Context, workspaceID string, decrypt bool) (*LegacyWorkspaceTiers, error) {
	var leg LegacyWorkspaceTiers
	var flashC, liteC, proC *string
	err := s.pg.QueryRow(ctx, `
SELECT provider, model, base_url, api_key_ciphertext,
       lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
       pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback
FROM workspace_model_tiers WHERE workspace_id = $1`, workspaceID).Scan(
		&leg.Provider, &leg.Model, &leg.BaseURL, &flashC,
		&leg.LiteProvider, &leg.LiteModel, &leg.LiteBaseURL, &liteC,
		&leg.ProProvider, &leg.ProModel, &leg.ProBaseURL, &proC, &leg.ModelFallback)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	decode := func(c *string) string {
		if c == nil || *c == "" {
			return ""
		}
		if !decrypt {
			// Presence-only: a non-empty ciphertext counts as a key for HasKey.
			return "x"
		}
		plain, decErr := decryptAgentKey(*c)
		if decErr != nil {
			return ""
		}
		return plain
	}
	leg.FlashKey = decode(flashC)
	leg.LiteKey = decode(liteC)
	leg.ProKey = decode(proC)
	if leg.Provider == "" && leg.Model == "" && leg.FlashKey == "" && leg.LiteModel == "" && leg.ProModel == "" {
		return nil, nil
	}
	return &leg, nil
}

func (s *Store) saveTierSelection(ctx context.Context, workspaceID string, sel WorkspaceTierSelection) error {
	// Keep denormalized provider/model/base_url columns in sync so GET still
	// exposes the old fields; do not touch key ciphertexts (those live on
	// workspace_providers after the upgrade).
	book, err := s.loadBook(ctx, workspaceID, false)
	if err != nil {
		return err
	}
	byID := book.byID()
	denorm := func(id, model string) (vendor, base, mid string) {
		if id == "" {
			return "", "", model
		}
		p, ok := byID[id]
		if !ok {
			return "", "", model
		}
		return p.Vendor, p.BaseURL, model
	}
	fv, fb, fm := denorm(sel.FlashProviderID, sel.FlashModel)
	lv, lb, lm := denorm(sel.LiteProviderID, sel.LiteModel)
	pv, pb, pm := denorm(sel.ProProviderID, sel.ProModel)
	if fv == "" {
		fv = "openai"
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO workspace_model_tiers (
	workspace_id, provider, model, base_url,
	lite_provider, lite_model, lite_base_url,
	pro_provider, pro_model, pro_base_url,
	model_fallback, flash_provider_id, lite_provider_id, pro_provider_id,
	context_window, lite_context_window, pro_context_window
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::uuid,NULLIF($13,'')::uuid,NULLIF($14,'')::uuid,$15,$16,$17)
ON CONFLICT (workspace_id) DO UPDATE SET
	provider = EXCLUDED.provider,
	model = EXCLUDED.model,
	base_url = EXCLUDED.base_url,
	lite_provider = EXCLUDED.lite_provider,
	lite_model = EXCLUDED.lite_model,
	lite_base_url = EXCLUDED.lite_base_url,
	pro_provider = EXCLUDED.pro_provider,
	pro_model = EXCLUDED.pro_model,
	pro_base_url = EXCLUDED.pro_base_url,
	model_fallback = EXCLUDED.model_fallback,
	flash_provider_id = EXCLUDED.flash_provider_id,
	lite_provider_id = EXCLUDED.lite_provider_id,
	pro_provider_id = EXCLUDED.pro_provider_id,
	context_window = EXCLUDED.context_window,
	lite_context_window = EXCLUDED.lite_context_window,
	pro_context_window = EXCLUDED.pro_context_window,
	updated_at = now()`,
		workspaceID, fv, fm, fb, lv, lm, lb, pv, pm, pb, sel.ModelFallback,
		sel.FlashProviderID, sel.LiteProviderID, sel.ProProviderID,
		sel.FlashContextWindow, sel.LiteContextWindow, sel.ProContextWindow)
	return err
}

// SaveWorkspaceTierSelection writes lite/flash/pro → provider+model (owner/admin).
func (s *Store) SaveWorkspaceTierSelection(ctx context.Context, userID, workspaceID string, sel WorkspaceTierSelection) (WorkspaceModelTiers, error) {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return WorkspaceModelTiers{}, err
	} else if !ok {
		return WorkspaceModelTiers{}, errAgentForbidden
	}
	book, err := s.loadBook(ctx, workspaceID, false)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	if err := book.SetTiers(sel); err != nil {
		return WorkspaceModelTiers{}, err
	}
	if err := s.saveTierSelection(ctx, workspaceID, book.Sel); err != nil {
		return WorkspaceModelTiers{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_tiers.update", "workspace", workspaceID, "", "{}")
	return s.readWorkspaceModelTiers(ctx, workspaceID)
}
