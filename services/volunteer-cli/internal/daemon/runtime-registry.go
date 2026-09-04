package daemon

import (
	"fmt"
	"strings"
	"sync"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// RuntimeRegistry manages multiple runtimes and selects the appropriate one
// for each work unit based on the work unit's runtime field.
//
// It is read from the fetcher, every execution slot, the disk gate and the
// management API's handlers, and — since TB-59 — written after start-up too,
// when a container engine that was absent at start is detected and its
// runtime registered late. Every method therefore takes the lock.
type RuntimeRegistry struct {
	mu       sync.RWMutex
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtimes[rt.Name()] = rt
}

// SelectRuntime picks the runtime for a work unit based on wu.Runtime.
// Returns an error if no matching runtime is registered or it can't handle the spec.
func (r *RuntimeRegistry) SelectRuntime(wu *runtime.WorkUnit) (runtime.Runtime, error) {
	// SECURITY (BG-12): refuse an empty or unknown runtime UNCONDITIONALLY — never
	// fall back to native even when native is registered. The old empty->native
	// default let a (malicious or buggy) head omit the field to steer a unit onto the
	// least-isolated backend; leaf creation already requires a runtime, so only such
	// a head ever sends "".
	name := strings.ToLower(wu.Runtime)
	if name == "" {
		return nil, fmt.Errorf("work unit has no runtime specified; refusing to run it")
	}
	rt := r.GetRuntime(name)
	if rt == nil {
		return nil, fmt.Errorf("no available runtime for work unit (requires %s)", wu.Runtime)
	}
	if !rt.CanHandle(&wu.ExecutionSpec) {
		return nil, fmt.Errorf("runtime %s cannot handle work unit execution spec", name)
	}
	return rt, nil
}

// GetRuntime returns the runtime registered under the given name, or nil if not found.
func (r *RuntimeRegistry) GetRuntime(name string) runtime.Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runtimes[strings.ToLower(name)]
}

// AvailableRuntimes returns the names of all registered runtimes.
func (r *RuntimeRegistry) AvailableRuntimes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runtimes))
	for name := range r.runtimes {
		names = append(names, name)
	}
	return names
}
