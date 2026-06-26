package secret

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	slices.Sort(keys)
	return keys
}

// Resolve locates the provider-specific resolver and delegates the call.
func (m *MultiResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := m.ResolveWithIdentity(ctx, ref)
	return value, err
}

// ResolveWithIdentity locates the provider-specific resolver and delegates the call.
func (m *MultiResolver) ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error) {
	resolver, err := m.providerForRef(ref)
	if err != nil {
		return "", Identity{}, err
	}
	return resolver.ResolveWithIdentity(ctx, ref)
}

func (m *MultiResolver) VerifyIdentity(ctx context.Context, ref string, expected Identity) (Identity, error) {
	resolver, err := m.providerForRef(ref)
	if err != nil {
		return Identity{}, err
	}
	verifier, ok := resolver.(IdentityVerifier)
	if !ok {
		return Identity{}, fmt.Errorf("secret provider %q does not support baseline identity verification", expected.Provider)
	}
	return verifier.VerifyIdentity(ctx, ref, expected)
}

func (m *MultiResolver) VerifyResolvedIdentity(ctx context.Context, ref string, expected Identity, resolvedValue string) (Identity, error) {
	resolver, err := m.providerForRef(ref)
	if err != nil {
		return Identity{}, err
	}
	verifier, ok := resolver.(ResolvedIdentityVerifier)
	if !ok {
		return Identity{}, fmt.Errorf("secret provider %q does not support resolved baseline identity verification", expected.Provider)
	}
	return verifier.VerifyResolvedIdentity(ctx, ref, expected, resolvedValue)
}

func (m *MultiResolver) providerForRef(ref string) (Resolver, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("secret reference is empty")
	}
	if m == nil {
		return nil, errors.New("secret resolver is not configured")
	}

	refInfo, err := Parse(ref)
	if err != nil {
		return nil, err
	}

	resolver, ok := m.providers[refInfo.Provider]
	if !ok || resolver == nil {
		return nil, fmt.Errorf("secret provider %q not configured", refInfo.Provider)
	}

	return resolver, nil
}
