package opcore

// Registry is an ordered set of operations keyed by name. One registry is built
// by the usecase layer and shared by all three adapters, so the agent's tool
// list, the REST surface, and the CLI's command set can never drift apart.
type Registry struct {
	order []string
	specs map[string]Spec
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
