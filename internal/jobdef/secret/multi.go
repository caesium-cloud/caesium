package secret

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const scheme = "secret"

// MultiResolver dispatches references to provider-specific resolvers based on the URI host.
type MultiResolver struct {
	providers map[string]Resolver
}

// NewMultiResolver constructs a MultiResolver seeded with the provided map.
func NewMultiResolver(providers map[string]Resolver) *MultiResolver {
	copy := make(map[string]Resolver, len(providers))
	for k, v := range providers {
		copy[strings.ToLower(k)] = v
	}
	return &MultiResolver{providers: copy}
}

// Register associates a provider name with a resolver. Existing providers are replaced.
func (m *MultiResolver) Register(provider string, resolver Resolver) {
	if m.providers == nil {
		m.providers = make(map[string]Resolver)
	}
	m.providers[strings.ToLower(strings.TrimSpace(provider))] = resolver
}

// Providers returns the sorted list of registered provider keys.
func (m *MultiResolver) Providers() []string {
	if len(m.providers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m.providers))
	for k := range m.providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Resolve locates the provider-specific resolver and delegates the call.
func (m *MultiResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("secret reference is empty")
	}
	if m == nil {
		return "", errors.New("secret resolver is not configured")
	}

	refInfo, err := Parse(ref)
	if err != nil {
		return "", err
	}

	resolver, ok := m.providers[refInfo.Provider]
	if !ok || resolver == nil {
		return "", fmt.Errorf("secret provider %q not configured", refInfo.Provider)
	}

	return resolver.Resolve(ctx, ref)
}
