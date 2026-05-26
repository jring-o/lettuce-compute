package daemon

import (
	"fmt"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// RuntimeRegistry manages multiple runtimes and selects the appropriate one
// for each work unit based on the work unit's runtime field.
type RuntimeRegistry struct {
	runtimes map[string]runtime.Runtime
}

// NewRuntimeRegistry creates an empty runtime registry.
func NewRuntimeRegistry() *RuntimeRegistry {
	return &RuntimeRegistry{
		runtimes: make(map[string]runtime.Runtime),
	}
}

// Register adds a runtime to the registry, keyed by its Name().
func (r *RuntimeRegistry) Register(rt runtime.Runtime) {
	r.runtimes[rt.Name()] = rt
}

// SelectRuntime picks the runtime for a work unit based on wu.Runtime.
// Returns an error if no matching runtime is registered or it can't handle the spec.
func (r *RuntimeRegistry) SelectRuntime(wu *runtime.WorkUnit) (runtime.Runtime, error) {
	name := strings.ToLower(wu.Runtime)
	if name == "" {
		name = "native"
	}
	rt, ok := r.runtimes[name]
	if !ok {
		return nil, fmt.Errorf("no available runtime for work unit (requires %s)", wu.Runtime)
	}
	if !rt.CanHandle(&wu.ExecutionSpec) {
		return nil, fmt.Errorf("runtime %s cannot handle work unit execution spec", name)
	}
	return rt, nil
}

// GetRuntime returns the runtime registered under the given name, or nil if not found.
func (r *RuntimeRegistry) GetRuntime(name string) runtime.Runtime {
	return r.runtimes[strings.ToLower(name)]
}

// AvailableRuntimes returns the names of all registered runtimes.
func (r *RuntimeRegistry) AvailableRuntimes() []string {
	names := make([]string, 0, len(r.runtimes))
	for name := range r.runtimes {
		names = append(names, name)
	}
	return names
}
