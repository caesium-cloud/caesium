package connector

import (
	"fmt"
	"sync"
)

// Adapter is a provider implementation registered behind the neutral contract.
// Identity is opaque: the registry does not inspect Temporal fields.
type Adapter interface {
	Identity() ProviderIdentity
}

// Registry stores adapters by opaque identity name.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Adapter
}

// NewRegistry returns an empty registry. No Temporal adapter is installed.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Adapter{}}
}

// Register adds an adapter. A second adapter with the same identity name is
// refused. The capability set must be non-empty and drawn from the frozen list.
func (r *Registry) Register(adapter Adapter) error {
	if r == nil {
		return fmt.Errorf("connector registry is required")
	}
	if adapter == nil {
		return fmt.Errorf("connector adapter is required")
	}
	identity := adapter.Identity()
	if err := validateIdentity(identity); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[identity.Name]; exists {
		return fmt.Errorf("connector adapter %q is already registered", identity.Name)
	}
	r.byName[identity.Name] = adapter
	return nil
}

// Get returns the adapter registered under name.
func (r *Registry) Get(name string) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.byName[name]
	return adapter, ok
}

func validateIdentity(identity ProviderIdentity) error {
	if !idPattern.MatchString(identity.Name) {
		return fmt.Errorf("connector adapter identity must be a stable token")
	}
	if len(identity.Capabilities) == 0 {
		return fmt.Errorf("connector adapter %q must advertise a capability set", identity.Name)
	}
	seen := make(map[Capability]struct{}, len(identity.Capabilities))
	for _, capability := range identity.Capabilities {
		if _, known := knownCapabilities[capability]; !known {
			return fmt.Errorf("connector adapter %q has unknown capability %q", identity.Name, capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("connector adapter %q repeats capability %q", identity.Name, capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

var knownCapabilities = map[Capability]struct{}{
	CapabilityDiscovery:        {},
	CapabilityInspection:       {},
	CapabilityHistory:          {},
	CapabilityRelationships:    {},
	CapabilityStatusQuery:      {},
	CapabilityActionSubmission: {},
	CapabilityReceiptLookup:    {},
}
