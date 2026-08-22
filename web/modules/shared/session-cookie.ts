// The session cookie's name, shared by the server components that need to know
// whether a request already carries one. Set by the Go API
// (`sessionCookieName`, internal/app/auth.go) — httpOnly, host-only, Lax — so
// the value is never readable here and never needs to be: presence is the only
// question a render asks.
export const SESSION_COOKIE = 'agentray_session';
