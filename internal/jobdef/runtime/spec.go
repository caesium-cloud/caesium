package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
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
			if errors.Is(err, secret.ErrSecretDenied) {
				// The denial already names the redacted reference; never
				// re-attach the raw one.
				return container.Spec{}, nil, fmt.Errorf("resolve env %s: %w", key, err)
			}
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

// ProcessSecretResolver is the resolver chain this process's environment
// configures, built once on first use. Server lint, the apply preview and the
// replay constructor only canonicalise references with it (no secret is read)
// for the namespace allow-check, so they match exactly what a run of the same
// job would resolve.
var ProcessSecretResolver = sync.OnceValues(func() (secret.Resolver, error) {
	resolver, err := BuildSecretResolver(env.Variables())
	if err != nil {
		return nil, err
	}
	return resolver, nil
})

// CheckContainerSpecSecretsAllowed runs the namespace allow-check (parse,
// canonicalise, match; nothing is resolved) over every secret:// value in the
// step environment, in env-key order. Executors call it before the cache
// lookup so a reference the namespace no longer allows can never be satisfied
// from a cached result. A namespace without secret rules returns at once.
func CheckContainerSpecSecretsAllowed(scope *secret.ScopedResolver, spec container.Spec) error {
	if scope.AllowsAll() || len(spec.Env) == 0 {
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(spec.Env)) {
		value := spec.Env[key]
		if !strings.HasPrefix(value, "secret://") {
			continue
		}
		if err := scope.Allow(value); err != nil {
			return fmt.Errorf("env %s: %w", key, err)
		}
	}
	return nil
}

// DecodeFanOutConfig unmarshals the catalog Task.FanOutConfig JSON onto the
// job-definition FanOut struct. Nil/empty raw returns (nil, nil).
func DecodeFanOutConfig(raw []byte) (*jobdefschema.FanOut, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fo jobdefschema.FanOut
	if err := json.Unmarshal(raw, &fo); err != nil {
		return nil, fmt.Errorf("decode fanOut config: %w", err)
	}
	return &fo, nil
}
