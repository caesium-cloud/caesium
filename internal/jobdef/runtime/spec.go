package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/container"
)

// ResolveContainerSpecSecrets resolves secret:// values in the step-declared
// environment at container-create time. It leaves non-secret values unchanged
// and returns a copied spec so callers do not mutate cached atom specs.
func ResolveContainerSpecSecrets(ctx context.Context, resolver secret.Resolver, spec container.Spec) (container.Spec, error) {
	if len(spec.Env) == 0 {
		return spec, nil
	}

	resolved := make(map[string]string, len(spec.Env))
	for key, value := range spec.Env {
		if !strings.HasPrefix(value, "secret://") {
			resolved[key] = value
			continue
		}
		if resolver == nil {
			return container.Spec{}, fmt.Errorf("resolve env %s: no secret resolver configured for %s", key, value)
		}
		secretValue, err := resolver.Resolve(ctx, value)
		if err != nil {
			return container.Spec{}, fmt.Errorf("resolve env %s from %s: %w", key, value, err)
		}
		resolved[key] = secretValue
	}
	spec.Env = resolved
	return spec, nil
}
