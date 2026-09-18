package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

// WorkspaceProvider is the redacted public view of one configured vendor
// (key never returned — only HasKey).
type WorkspaceProvider struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	Vendor       string `json:"vendor"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	HasKey       bool   `json:"has_key"`
	AuthType     string `json:"auth_type"`
	AccountCount int    `json:"account_count"`
}

// WorkspaceProviderRecord is the persist/run shape. APIKey is the decrypted
// secret and is never serialized to JSON.
type WorkspaceProviderRecord struct {
	ID           string
	WorkspaceID  string
	Vendor       string
	Name         string
	BaseURL      string
	APIKey       string
	HasKey       bool
	AuthType     string
	AccountCount int
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
// A blank lite/pro provider+model inherits flash at resolve time. Each tier's
// fallback is a (provider, model) pair the run retries on when the primary
// model fails: FallbackProviderID blank means "the tier's own provider"
// (in-tier fallback); set means the rung runs on that provider row — the
// cross-provider leg of the escalation ladder. Still in-tier in the other
// direction: a failed call never climbs to another tier.
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
	FlashCapabilities  agentcore.ModelCapabilities
	LiteContextWindow  int
	LiteCapabilities   agentcore.ModelCapabilities
	ProContextWindow   int
	ProCapabilities    agentcore.ModelCapabilities
	// Per-tier fallbacks — a (provider, model) pair. A blank provider id keeps
	// the fallback on the tier's own provider; a set one crosses providers.
	FlashFallbackModel        string
	LiteFallbackModel         string
	ProFallbackModel          string
	FlashFallbackProviderID   string
	FlashFallbackCapabilities agentcore.ModelCapabilities
	LiteFallbackProviderID    string
	LiteFallbackCapabilities  agentcore.ModelCapabilities
	ProFallbackProviderID     string
	ProFallbackCapabilities   agentcore.ModelCapabilities
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
		authType := r.AuthType
		if authType == "" {
			authType = providerAuthType(r.Vendor)
		}
		hasKey := r.HasKey || r.APIKey != ""
		if authType == "oauth" {
			// Pooled providers hold no static key — a live account is what
			// makes them usable, so the pool size is the "has key" signal.
			hasKey = r.AccountCount > 0
		}
		out = append(out, WorkspaceProvider{
			ID: r.ID, WorkspaceID: r.WorkspaceID, Vendor: r.Vendor,
			Name: name, BaseURL: r.BaseURL, HasKey: hasKey,
			AuthType: authType, AccountCount: r.AccountCount,
		})
	}
	return out
}

// providerAuthType maps a vendor onto its credential shape: "oauth" for the
// subscription vendors backed by an account pool, "optional" for local engines
// that may run without auth, and "key" for API-key vendors.
func providerAuthType(vendor string) string {
	if ai.IsOAuthVendor(vendor) {
		return "oauth"
	}
	if ai.APIKeyOptional(vendor) {
		return "optional"
	}
	return "key"
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
	if b.Sel.FlashFallbackProviderID == id {
		b.Sel.FlashFallbackProviderID = ""
	}
	if b.Sel.LiteFallbackProviderID == id {
		b.Sel.LiteFallbackProviderID = ""
	}
	if b.Sel.ProFallbackProviderID == id {
		b.Sel.ProFallbackProviderID = ""
	}
	if b.Sel.LiteProviderID == id {
		b.Sel.LiteProviderID = ""
	}
	if b.Sel.ProProviderID == id {
		b.Sel.ProProviderID = ""
	}
}

// SetTiers records the 3-tier selection. Unknown provider ids are rejected
// (blank is allowed — inherit), and a fallback provider id without its model
// is rejected: the pair is the rung, half of it is meaningless.
func (b *WorkspaceProviderBook) SetTiers(sel WorkspaceTierSelection) error {
	known := b.byID()
	for _, id := range []string{sel.FlashProviderID, sel.LiteProviderID, sel.ProProviderID,
		sel.FlashFallbackProviderID, sel.LiteFallbackProviderID, sel.ProFallbackProviderID} {
		if id == "" {
			continue
		}
		if _, ok := known[id]; !ok {
			return fmt.Errorf("unknown provider %q", id)
		}
	}
	for _, fb := range []struct{ id, model string }{
		{sel.FlashFallbackProviderID, sel.FlashFallbackModel},
		{sel.LiteFallbackProviderID, sel.LiteFallbackModel},
		{sel.ProFallbackProviderID, sel.ProFallbackModel},
	} {
		if fb.id != "" && fb.model == "" {
			return fmt.Errorf("fallback provider %q without a fallback model", fb.id)
		}
	}
	for _, caps := range []agentcore.ModelCapabilities{
		sel.FlashCapabilities, sel.LiteCapabilities, sel.ProCapabilities,
		sel.FlashFallbackCapabilities, sel.LiteFallbackCapabilities, sel.ProFallbackCapabilities,
	} {
		if err := caps.Validate(); err != nil {
			return fmt.Errorf("invalid model capabilities: %w", err)
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
	oauth := ai.IsOAuthVendor(vendor)
	if !oauth && vendor != "openai" && vendor != ai.VendorOpenAIResponses && vendor != "anthropic" && vendor != "google" && base == "" {
		return WorkspaceProviderRecord{}, fmt.Errorf("provider %q requires a base URL", vendor)
	}
	rec := WorkspaceProviderRecord{
		ID:          id,
		WorkspaceID: workspaceID,
		Vendor:      vendor,
		Name:        strings.TrimSpace(in.Name),
		BaseURL:     base,
		AuthType:    providerAuthType(vendor),
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	if rec.Name == "" {
		rec.Name = vendor
	}
	// OAuth vendors hold no static key — their credential is the account pool.
	// An API key on an OAuth-vendor alias ("codex", "chatgpt", "antigravity")
	// means the caller wanted a custom endpoint, not the subscription flow —
	// silently dropping the key would strand them on a provider that can never
	// authenticate. Say so instead.
	if oauth && strings.TrimSpace(in.APIKey) != "" && in.APIKey != "-" {
		return WorkspaceProviderRecord{}, fmt.Errorf(
			"provider %q is a subscription sign-in vendor and takes no API key — for a custom OpenAI-compatible endpoint use vendor \"openai-compat\"", in.Vendor)
	}
	if !oauth {
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
			APIKey: key, HasKey: key != "", AuthType: providerAuthType(vendor),
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
		// OAuth vendors hold no static key: a live account in the pool is what
		// makes the provider usable, and the wire client pulls the real token
		// from its TokenSource per request. The pool sentinel keeps the run
		// path's "key configured" gate satisfied without a real key.
		if ai.IsOAuthVendor(p.Vendor) {
			if p.AccountCount > 0 {
				return p.Vendor, p.BaseURL, ai.OAuthPoolKey, model, true
			}
			return p.Vendor, p.BaseURL, "", model, false
		}
		// HasKey is set from ciphertext presence when the book is loaded
		// redacted (GET). APIKey is set only on the decrypt/run path. Either
		// counts so a BYOK workspace is not treated as empty. Explicit local
		// engines are also ready without a credential.
		return p.Vendor, p.BaseURL, p.APIKey, model,
			p.HasKey || p.APIKey != "" || ai.APIKeyOptional(p.Vendor)
	}

	fv, fb, fk, fm, fh := pick(sel.FlashProviderID, sel.FlashModel)
	lv, lb, lk, lm, lh := pick(sel.LiteProviderID, sel.LiteModel)
	pv, pb, pk, pm, ph := pick(sel.ProProviderID, sel.ProModel)

	// Fallback rungs: a fallback that names a provider row resolves to that
	// row's vendor/base/key — the cross-provider leg. A blank provider id
	// keeps the fallback on the tier's own provider and resolves to nothing
	// extra here (the runtime reuses the tier's provider).
	ffv, ffb, ffk, _, ffh := pick(sel.FlashFallbackProviderID, sel.FlashFallbackModel)
	lfv, lfb, lfk, _, lfh := pick(sel.LiteFallbackProviderID, sel.LiteFallbackModel)
	pfv, pfb, pfk, _, pfh := pick(sel.ProFallbackProviderID, sel.ProFallbackModel)

	cfg := WorkspaceModelTiers{
		Provider: fv, Model: fm, BaseURL: fb, HasKey: fh, ContextWindow: sel.FlashContextWindow, Capabilities: sel.FlashCapabilities,
		FallbackModel: sel.FlashFallbackModel, FallbackProviderID: sel.FlashFallbackProviderID,
		FallbackProvider: ffv, FallbackBaseURL: ffb, FallbackHasKey: ffh, FallbackCapabilities: sel.FlashFallbackCapabilities,
		LiteProvider: lv, LiteModel: lm, LiteBaseURL: lb, LiteHasKey: lh, LiteContextWindow: sel.LiteContextWindow, LiteCapabilities: sel.LiteCapabilities,
		LiteFallbackModel: sel.LiteFallbackModel, LiteFallbackProviderID: sel.LiteFallbackProviderID,
		LiteFallbackProvider: lfv, LiteFallbackBaseURL: lfb, LiteFallbackHasKey: lfh, LiteFallbackCapabilities: sel.LiteFallbackCapabilities,
		ProProvider: pv, ProModel: pm, ProBaseURL: pb, ProHasKey: ph, ProContextWindow: sel.ProContextWindow, ProCapabilities: sel.ProCapabilities,
		ProFallbackModel: sel.ProFallbackModel, ProFallbackProviderID: sel.ProFallbackProviderID,
		ProFallbackProvider: pfv, ProFallbackBaseURL: pfb, ProFallbackHasKey: pfh, ProFallbackCapabilities: sel.ProFallbackCapabilities,
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
	// Fallback providers get their own key slots — a cross-provider rung
	// authenticates with its own row's credential, never the tier's.
	if ffk != "" {
		keys["flash_fallback"] = ffk
	}
	if lfk != "" {
		keys["lite_fallback"] = lfk
	}
	if pfk != "" {
		keys["pro_fallback"] = pfk
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
		return nil, ErrAgentForbidden
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
		return WorkspaceProvider{}, ErrAgentForbidden
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
		return WorkspaceProvider{}, ErrAgentForbidden
	}
	existing, err := s.loadProviderRecord(ctx, workspaceID, providerID, false)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	rec, err := NewWorkspaceProviderRecord(providerID, workspaceID, in, &existing)
	if err != nil {
		return WorkspaceProvider{}, err
	}
	var cipherArg any
	if ai.IsOAuthVendor(rec.Vendor) {
		// An OAuth row never reads api_key_ciphertext — but COALESCE($6, …)
		// keeps whatever a previous API-key vendor stored, and loadBook would
		// then report HasKey for a provider that has no usable credential,
		// suppressing the hosted-model fallback. Clear it instead.
		cipherArg = ""
	} else {
		cipherArg, err = resolveCipherArg(in.APIKey)
		if err != nil {
			return WorkspaceProvider{}, err
		}
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
		return ErrAgentForbidden
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_model_tiers SET
	flash_provider_id = CASE WHEN flash_provider_id::text = $2 THEN NULL ELSE flash_provider_id END,
	lite_provider_id  = CASE WHEN lite_provider_id::text  = $2 THEN NULL ELSE lite_provider_id END,
	pro_provider_id   = CASE WHEN pro_provider_id::text   = $2 THEN NULL ELSE pro_provider_id END,
	fallback_provider_id      = CASE WHEN fallback_provider_id::text      = $2 THEN NULL ELSE fallback_provider_id END,
	lite_fallback_provider_id = CASE WHEN lite_fallback_provider_id::text = $2 THEN NULL ELSE lite_fallback_provider_id END,
	pro_fallback_provider_id  = CASE WHEN pro_fallback_provider_id::text  = $2 THEN NULL ELSE pro_fallback_provider_id END
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
	rec.AuthType = providerAuthType(rec.Vendor)
	if rec.AuthType == "oauth" {
		if err := s.pg.QueryRow(ctx, `
SELECT count(*)::int FROM workspace_provider_accounts
WHERE provider_id = $1 AND status = 'active'`, providerID).Scan(&rec.AccountCount); err != nil {
			return WorkspaceProviderRecord{}, err
		}
	}
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
		// An OAuth vendor's credential is its account pool, never the stored
		// key — a leftover ciphertext (a row written before the vendor switch,
		// or a legacy backfill) must not count as "has a key", or
		// providerBookHasKey suppresses the hosted-model fallback for a
		// provider that cannot actually authenticate.
		rec.HasKey = cipher != "" && !ai.IsOAuthVendor(rec.Vendor)
		rec.AuthType = providerAuthType(rec.Vendor)
		if decrypt && cipher != "" && !ai.IsOAuthVendor(rec.Vendor) {
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
	var flashFallback, liteFallback, proFallback string
	var flashFbID, liteFbID, proFbID *string
	var flashCaps, liteCaps, proCaps []byte
	var flashFbCaps, liteFbCaps, proFbCaps []byte
	var fallback bool
	var flashWindow, liteWindow, proWindow int
	err = s.pg.QueryRow(ctx, `
SELECT flash_provider_id::text, model, lite_provider_id::text, lite_model,
       pro_provider_id::text, pro_model, model_fallback,
       context_window, lite_context_window, pro_context_window,
	       fallback_model, lite_fallback_model, pro_fallback_model,
	       fallback_provider_id::text, lite_fallback_provider_id::text, pro_fallback_provider_id::text,
	       capabilities, lite_capabilities, pro_capabilities,
	       fallback_capabilities, lite_fallback_capabilities, pro_fallback_capabilities
FROM workspace_model_tiers WHERE workspace_id = $1`, workspaceID).Scan(
		&flashID, &flashModel, &liteID, &liteModel, &proID, &proModel, &fallback,
		&flashWindow, &liteWindow, &proWindow,
		&flashFallback, &liteFallback, &proFallback,
		&flashFbID, &liteFbID, &proFbID,
		&flashCaps, &liteCaps, &proCaps, &flashFbCaps, &liteFbCaps, &proFbCaps)
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
		book.Sel.FlashFallbackModel = flashFallback
		book.Sel.LiteFallbackModel = liteFallback
		book.Sel.ProFallbackModel = proFallback
		book.Sel.FlashCapabilities = decodeModelCapabilities(flashCaps)
		book.Sel.LiteCapabilities = decodeModelCapabilities(liteCaps)
		book.Sel.ProCapabilities = decodeModelCapabilities(proCaps)
		book.Sel.FlashFallbackCapabilities = decodeModelCapabilities(flashFbCaps)
		book.Sel.LiteFallbackCapabilities = decodeModelCapabilities(liteFbCaps)
		book.Sel.ProFallbackCapabilities = decodeModelCapabilities(proFbCaps)
		if flashID != nil {
			book.Sel.FlashProviderID = *flashID
		}
		if liteID != nil {
			book.Sel.LiteProviderID = *liteID
		}
		if proID != nil {
			book.Sel.ProProviderID = *proID
		}
		if flashFbID != nil {
			book.Sel.FlashFallbackProviderID = *flashFbID
		}
		if liteFbID != nil {
			book.Sel.LiteFallbackProviderID = *liteFbID
		}
		if proFbID != nil {
			book.Sel.ProFallbackProviderID = *proFbID
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
	// Active-account counts per provider — one grouped scan rather than a
	// per-row subquery. Only OAuth vendors consume it, but filling it for all
	// keeps the public view honest.
	if len(book.Providers) > 0 {
		ids := make([]string, len(book.Providers))
		for i, p := range book.Providers {
			ids[i] = p.ID
		}
		countRows, err := s.pg.Query(ctx, `
SELECT provider_id::text, count(*) FILTER (WHERE status = 'active')::int
FROM workspace_provider_accounts WHERE provider_id = ANY($1::uuid[])
GROUP BY provider_id`, ids)
		if err != nil {
			return nil, err
		}
		counts := make(map[string]int, len(ids))
		for countRows.Next() {
			var pid string
			var n int
			if err := countRows.Scan(&pid, &n); err != nil {
				countRows.Close()
				return nil, err
			}
			counts[pid] = n
		}
		countRows.Close()
		if err := countRows.Err(); err != nil {
			return nil, err
		}
		for i := range book.Providers {
			book.Providers[i].AccountCount = counts[book.Providers[i].ID]
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
	context_window, lite_context_window, pro_context_window,
		fallback_model, lite_fallback_model, pro_fallback_model,
		fallback_provider_id, lite_fallback_provider_id, pro_fallback_provider_id,
		capabilities, lite_capabilities, pro_capabilities,
		fallback_capabilities, lite_fallback_capabilities, pro_fallback_capabilities
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::uuid,NULLIF($13,'')::uuid,NULLIF($14,'')::uuid,$15,$16,$17,$18,$19,$20,NULLIF($21,'')::uuid,NULLIF($22,'')::uuid,NULLIF($23,'')::uuid,$24::jsonb,$25::jsonb,$26::jsonb,$27::jsonb,$28::jsonb,$29::jsonb)
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
	fallback_model = EXCLUDED.fallback_model,
	lite_fallback_model = EXCLUDED.lite_fallback_model,
	pro_fallback_model = EXCLUDED.pro_fallback_model,
	fallback_provider_id = EXCLUDED.fallback_provider_id,
	lite_fallback_provider_id = EXCLUDED.lite_fallback_provider_id,
	pro_fallback_provider_id = EXCLUDED.pro_fallback_provider_id,
	capabilities = EXCLUDED.capabilities,
	lite_capabilities = EXCLUDED.lite_capabilities,
	pro_capabilities = EXCLUDED.pro_capabilities,
	fallback_capabilities = EXCLUDED.fallback_capabilities,
	lite_fallback_capabilities = EXCLUDED.lite_fallback_capabilities,
	pro_fallback_capabilities = EXCLUDED.pro_fallback_capabilities,
	updated_at = now()`,
		workspaceID, fv, fm, fb, lv, lm, lb, pv, pm, pb, sel.ModelFallback,
		sel.FlashProviderID, sel.LiteProviderID, sel.ProProviderID,
		sel.FlashContextWindow, sel.LiteContextWindow, sel.ProContextWindow,
		sel.FlashFallbackModel, sel.LiteFallbackModel, sel.ProFallbackModel,
		sel.FlashFallbackProviderID, sel.LiteFallbackProviderID, sel.ProFallbackProviderID,
		encodeModelCapabilities(sel.FlashCapabilities), encodeModelCapabilities(sel.LiteCapabilities), encodeModelCapabilities(sel.ProCapabilities),
		encodeModelCapabilities(sel.FlashFallbackCapabilities), encodeModelCapabilities(sel.LiteFallbackCapabilities), encodeModelCapabilities(sel.ProFallbackCapabilities))
	return err
}

func encodeModelCapabilities(c agentcore.ModelCapabilities) []byte {
	raw, _ := json.Marshal(c)
	return raw
}

func decodeModelCapabilities(raw []byte) agentcore.ModelCapabilities {
	var c agentcore.ModelCapabilities
	_ = json.Unmarshal(raw, &c)
	return c
}

// SaveWorkspaceTierSelection writes lite/flash/pro → provider+model (owner/admin).
func (s *Store) SaveWorkspaceTierSelection(ctx context.Context, userID, workspaceID string, sel WorkspaceTierSelection) (WorkspaceModelTiers, error) {
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil {
		return WorkspaceModelTiers{}, err
	} else if !ok {
		return WorkspaceModelTiers{}, ErrAgentForbidden
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
