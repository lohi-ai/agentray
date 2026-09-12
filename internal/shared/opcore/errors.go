package opcore

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorKind is the machine-readable class of an operation failure — the one
// taxonomy every adapter preserves instead of reclassifying per transport.
// The vocabulary is deliberately small: it is the contract external consumers
// (REST body `code`, MCP `_meta.error_code`, CLI error text, web APIError)
// branch on, so a new kind is a product decision, not a refactor.
type ErrorKind string

const (
	// ErrNotFound: the named resource does not exist in this project (or the
	// caller may not see it — cross-project reads are not-found, never a leak).
	ErrNotFound ErrorKind = "not_found"
	// ErrConflict: the request contradicts current state — stale revision,
	// idempotency-key reuse with a different payload, a paused sync. The
	// client should re-read and decide, not blind-retry.
	ErrConflict ErrorKind = "conflict"
	// ErrRetryable: a transient refusal — engine at capacity, dependency
	// unavailable. The same request may succeed later; idempotent operations
	// replay safely under their key.
	ErrRetryable ErrorKind = "retryable"
)

// OpError is a handler failure carrying a stable Kind. Handlers return it (or
// a sentinel the registry's classifier maps to it); adapters translate Kind to
// their native channel — HTTP status + body code, MCP _meta, CLI error text —
// so a not-found is a 404 over REST and a "not_found" everywhere else.
type OpError struct {
	Kind    ErrorKind
	Message string
	Err     error // optional wrapped cause
}

func (e *OpError) Error() string {
	// The kind prefixes the message so transports with only a text channel —
	// the in-process tool result the model reads, the CLI's stderr — still
	// carry the machine-readable class. Adapters with a typed channel (HTTP
	// body, MCP _meta) use Message directly and never show the prefix twice.
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *OpError) Unwrap() error { return e.Err }

// NotFound/Conflict/Retryable construct the typed failures handlers return.
// The formatted message is what every consumer displays.
func NotFound(format string, args ...any) *OpError {
	return &OpError{Kind: ErrNotFound, Message: fmt.Sprintf(format, args...)}
}

func Conflict(format string, args ...any) *OpError {
	return &OpError{Kind: ErrConflict, Message: fmt.Sprintf(format, args...)}
}

func Retryable(format string, args ...any) *OpError {
	return &OpError{Kind: ErrRetryable, Message: fmt.Sprintf(format, args...)}
}

// KindOf extracts the error kind an adapter should translate, unwrapping
// through fmt.Errorf chains. "" means untyped — adapters fall back to their
// generic error shape (HTTP 400, plain isError text).
func KindOf(err error) ErrorKind {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Kind
	}
	return ""
}

// opErrorBody is the JSON failure shape every /api/op error returns:
// `error` is the human message, `code` the ErrorKind (omitted when untyped).
// The web client reads `code` into APIError; `error` keeps the message under
// the key the client's existing parser already prefers.
type opErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// statusForKind maps the taxonomy onto HTTP. Retryable is 503 (server-side
// capacity, not client throttling — 429 would tell a rate-limited client to
// back off when nothing is wrong with its rate).
func statusForKind(k ErrorKind) int {
	switch k {
	case ErrNotFound:
		return http.StatusNotFound
	case ErrConflict:
		return http.StatusConflict
	case ErrRetryable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// classifyError runs the registry's classifier over a handler error. An error
// already carrying a Kind passes through untouched — explicit OpErrors always
// win over sentinel mapping.
func (r *Registry) classifyError(err error) error {
	if err == nil || KindOf(err) != "" {
		return err
	}
	if r.classifier == nil {
		return err
	}
	return r.classifier(err)
}

// SetErrorClassifier installs the one sentinel→kind mapping for every
// operation in the registry. The usecase layer owns it — opcore cannot import
// the storage/connector packages whose sentinels need mapping — and it runs
// at the adapter boundary, so REST, MCP, the in-process tool, and the CLI all
// classify identically without a per-handler edit.
func (r *Registry) SetErrorClassifier(fn func(error) error) {
	r.classifier = fn
}
