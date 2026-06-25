package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/container"
)

// ResolvedSecretIdentity is the identity metadata captured for one resolved
// environment secret reference.
type ResolvedSecretIdentity struct {
	EnvKey   string
	Ref      string
	Identity secret.Identity
}

// ResolveContainerSpecSecrets resolves secret:// values in the step-declared
// environment at container-create time. It leaves non-secret values unchanged
// and returns a copied spec so callers do not mutate cached atom specs.
func ResolveContainerSpecSecrets(ctx context.Context, resolver secret.Resolver, spec container.Spec) (container.Spec, error) {
	resolved, _, err := ResolveContainerSpecSecretsWithIdentities(ctx, resolver, spec)
	return resolved, err
}

// ResolveContainerSpecSecretsWithIdentities resolves secret:// values and
// returns the provider identity for every resolved secret reference.
func ResolveContainerSpecSecretsWithIdentities(ctx context.Context, resolver secret.Resolver, spec container.Spec) (container.Spec, []ResolvedSecretIdentity, error) {
	if len(spec.Env) == 0 {
		return spec, nil, nil
	}

	resolved := make(map[string]string, len(spec.Env))
	identities := make([]ResolvedSecretIdentity, 0)
	for key, value := range spec.Env {
		if !strings.HasPrefix(value, "secret://") {
			resolved[key] = value
			continue
		}
		if resolver == nil {
			return container.Spec{}, nil, fmt.Errorf("resolve env %s: no secret resolver configured for %s", key, value)
		}
		secretValue, identity, err := resolver.ResolveWithIdentity(ctx, value)
		if err != nil {
			return container.Spec{}, nil, fmt.Errorf("resolve env %s from %s: %w", key, value, err)
		}
		resolved[key] = secretValue
		identities = append(identities, ResolvedSecretIdentity{
			EnvKey:   key,
			Ref:      value,
			Identity: identity,
		})
	}
	spec.Env = resolved
	return spec, identities, nil
}
