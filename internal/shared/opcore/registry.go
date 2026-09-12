package opcore

// Registry is an ordered set of operations keyed by name. One registry is built
// by the usecase layer and shared by all three adapters, so the agent's tool
// list, the REST surface, and the CLI's command set can never drift apart.
type Registry struct {
	order []string
	specs map[string]Spec
	// legacyAllowlist is the frozen set of operation names a CredLegacy
	// principal may invoke (see auth.go). Set once via SetLegacyAllowlist.
	legacyAllowlist []string
	// classifier maps store/engine sentinel errors onto the OpError taxonomy
	// (see errors.go). Set once via SetErrorClassifier; nil leaves untyped
	// errors untyped.
	classifier func(error) error
	// errMapper translates a handler's typed error into the adapter's error
	// shape (an *echo.HTTPError for HTTP mounts). Set once by the usecase
	// layer, which owns the operations and their error contract; nil means
	// every failure is a plain 400.
	errMapper func(error) error
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{specs: map[string]Spec{}}
}

// Register adds a typed operation. It is a free function rather than a method
// because Go methods cannot introduce their own type parameters.
func Register[I any, O any](r *Registry, op Operation[I, O]) {
	if _, exists := r.specs[op.Name]; !exists {
		r.order = append(r.order, op.Name)
	}
	r.specs[op.Name] = op
}

// Get returns the operation of the given name, if registered.
func (r *Registry) Get(name string) (Spec, bool) {
	s, ok := r.specs[name]
	return s, ok
}

// Specs returns the operations in registration order.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.specs[n])
	}
	return out
}

// SetErrorMapper installs the operation layer's typed-error translation.
// MountHTTP applies it to handler failures so a revision conflict, a missing
// row, or an archived source reaches the client with its real status instead
// of a blanket 400 — the same contract the legacy adapter's opError applies.
func (r *Registry) SetErrorMapper(mapper func(error) error) {
	r.errMapper = mapper
}

// MapError runs the installed mapper, defaulting to the identity. Adapters
// call it on a handler error before framing the response.
func (r *Registry) MapError(err error) error {
	if r.errMapper == nil {
		return err
	}
	return r.errMapper(err)
}
