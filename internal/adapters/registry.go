package adapters

import "fmt"

// Factory builds an adapter on demand.
type Factory func() (Adapter, error)

// Registry maps adapter names to factories. The engine resolves the adapter a
// worker needs by name from config.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// DefaultRegistry returns a registry with the built-in adapters registered.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(ShellName, func() (Adapter, error) { return NewShell(), nil })
	r.Register(FakeName, func() (Adapter, error) { return NewFakeFromEnv() })
	r.Register(ClaudeCodeName, func() (Adapter, error) { return NewClaudeCodeFromEnv(), nil })
	r.Register(CodexName, func() (Adapter, error) { return NewCodexFromEnv(), nil })
	return r
}

// Register adds or replaces a factory.
func (r *Registry) Register(name string, f Factory) {
	r.factories[name] = f
}

// Get builds the named adapter.
func (r *Registry) Get(name string) (Adapter, error) {
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", name)
	}
	return f()
}

// Names returns the registered adapter names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	return names
}
