package opcore

import (
	"slices"

	"github.com/labstack/echo/v4"
)

// Access is the authorization class an operation belongs to. It is distinct
// from Operation.Scope (the in-process agent capability scope): Scope decides
// which tools a configured agent may see; Access decides which *credential* may
// invoke the operation over a network adapter (MCP, REST /api/op, CLI).
//
// The vocabulary is deliberately small and stable — it is the contract external
// management credentials are scoped by, so a new class is a product decision,
// not a refactor.
type Access string

const (
	// AccessAnalyticsRead covers read-only analytics: summaries, event reads,
	// insights, funnels, retention, dashboard listing, SDK verification.
	AccessAnalyticsRead Access = "analytics:read"
	// AccessDashboardsWrite covers dashboard/chart authoring and lifecycle.
	AccessDashboardsWrite Access = "dashboards:write"
	// AccessSourcesRead covers source probes and status: test, preview, status.
	AccessSourcesRead Access = "sources:read"
	// AccessSourcesManage covers source lifecycle: create, update, pause, run,
	// cancel. Implies AccessSourcesRead on management credentials.
	AccessSourcesManage Access = "sources:manage"
	// AccessGrowthWrite covers memory and notification writes (remember,
	// send_notification). It is grantable to management credentials but
	// broader than the Plans surface needs — see AccessPlansWrite.
	AccessGrowthWrite Access = "growth:write"
	// AccessPlansWrite covers the Plans write surface: finding/experiment
	// creation (submit_recommendation, propose_test), proposed-state edits
	// (update_test), append-only outcomes (record_outcome) and proposal
	// abandonment (abandon_test). It is the narrow opt-in scope for
	// management credentials; memory/notification writes stay on
	// AccessGrowthWrite.
	AccessPlansWrite Access = "plans:write"
)

// CredentialKind names which credential authenticated the request. The app
// layer resolves it; opcore only consumes it.
type CredentialKind string

const (
	// CredSession is a logged-in user's session cookie; Role carries the
	// workspace role and Grants the resolved access set.
	CredSession CredentialKind = "session"
	// CredLegacy is the historical project API key (projects.api_key) on a
	// project that has NOT opted into the credential split. It keeps exactly
	// the operations it had before the split existed — the registry's frozen
	// legacy allowlist — and gains nothing new.
	CredLegacy CredentialKind = "legacy"
	// CredCapture is the project API key on a split project: capture-only.
	// It may feed collection endpoints and is denied by every op adapter.
	CredCapture CredentialKind = "capture"
	// CredManagement is a scoped, revocable project credential
	// (project_credentials). Grants is the resolved union of its scopes.
	CredManagement CredentialKind = "management"
)

// Principal is the authenticated caller an adapter resolved. ProjectID scopes
// every operation; Kind + Grants decide what it may invoke. A Principal is
// produced only by the app layer's resolver — handlers and adapters never
// construct or mutate one.
type Principal struct {
	ProjectID string
	Kind      CredentialKind
	// Role is the workspace role for session principals ("" for keys).
	Role string
	// UserID is the session user ("" for keys).
	UserID string
	// CredentialID identifies a management credential for audit ("" otherwise).
	CredentialID string
	// Grants is the resolved access set for session and management principals.
	// Legacy and capture principals ignore it — legacy answers from the frozen
	// allowlist, capture is always denied.
	Grants []Access
}

// PrincipalResolver extracts the acting principal from an HTTP request. It
// replaces ProjectResolver: auth — session cookie, capture/legacy key,
// management credential — stays in the app layer; opcore only needs the
// resolved principal to scope and authorize the call.
type PrincipalResolver func(c echo.Context) (Principal, error)

// legacyOps is the registry's frozen allowlist for CredLegacy principals: the
// operation names a project key could invoke before the credential split
// existed, plus the explicitly grandfathered additions the migration approved.
// Set once by the usecase layer at registry build; never grows implicitly.
func (r *Registry) legacyAllows(name string) bool {
	return slices.Contains(r.legacyAllowlist, name)
}

// SetLegacyAllowlist freezes the operation names a legacy (unsplit) project key
// may still invoke. Called once at registry construction; later registrations
// do NOT join it — that is the point of Option A.
func (r *Registry) SetLegacyAllowlist(names []string) {
	r.legacyAllowlist = append([]string(nil), names...)
}

// Authorize reports whether the principal may invoke the named operation.
// Deny-by-default: an operation with no Access class is unreachable over the
// network adapters (in-process agent policy is unaffected — it filters tools
// before this layer).
func (r *Registry) Authorize(p Principal, opName string) bool {
	spec, ok := r.Get(opName)
	if !ok {
		return false
	}
	access := spec.OpAccess()
	if access == "" {
		return false
	}
	switch p.Kind {
	case CredCapture:
		return false
	case CredLegacy:
		return r.legacyAllows(opName)
	case CredManagement:
		return slices.Contains(p.Grants, access)
	case CredSession:
		return slices.Contains(p.Grants, access) && sessionRoleAtLeast(p.Role, spec.OpMinSessionRole())
	default:
		return false
	}
}

// sessionRoleAtLeast applies MinSessionRole: "" admits any member role
// (including viewer), "member" admits member/admin/owner, "admin" admits
// owner/admin only. Unknown roles admit nothing beyond the "" floor — a role
// the vocabulary does not know is read-only, matching writeRoles' direction.
func sessionRoleAtLeast(role, min string) bool {
	switch min {
	case "":
		return true
	case "member":
		return role == "member" || role == "admin" || role == "owner"
	case "admin":
		return role == "admin" || role == "owner"
	default:
		return false
	}
}

// AllowedSpecs returns the registered operations the principal may invoke, in
// registration order — what MCP tools/list advertises.
func (r *Registry) AllowedSpecs(p Principal) []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, s := range r.Specs() {
		if r.Authorize(p, s.OpName()) {
			out = append(out, s)
		}
	}
	return out
}
