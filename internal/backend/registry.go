package backend

import "fmt"

// Registry holds the set of backends this process has been configured with.
// A single backend object owns *all* instances of its type (e.g. the ceph-rbd
// backend holds every Ceph cluster's connection config) and selects the right
// one using the Instance field on each request / volume handle. That keeps the
// registry a flat type -> backend map.
type Registry struct {
	backends map[string]Backend
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

// Register adds a backend, keyed by its Type. It panics on a duplicate type,
// which can only be a programming error during wiring.
func (r *Registry) Register(b Backend) {
	t := b.Type()
	if _, dup := r.backends[t]; dup {
		panic(fmt.Sprintf("backend %q registered twice", t))
	}
	r.backends[t] = b
}

// Get returns the backend for a type, or an error if none is configured.
func (r *Registry) Get(t string) (Backend, error) {
	b, ok := r.backends[t]
	if !ok {
		return nil, fmt.Errorf("no backend configured for type %q", t)
	}
	return b, nil
}

// Types lists the registered backend types (unordered).
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.backends))
	for t := range r.backends {
		out = append(out, t)
	}
	return out
}
