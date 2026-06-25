package secret

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const providerEnv = "env"

// EnvResolver resolves secrets from process environment variables.
type EnvResolver struct{}

// NewEnvResolver returns an EnvResolver instance.
func NewEnvResolver() *EnvResolver {
	return &EnvResolver{}
}

// Resolve implements the Resolver interface.
func (r *EnvResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := r.ResolveWithIdentity(ctx, ref)
	return value, err
}

// ResolveWithIdentity implements the Resolver interface.
func (r *EnvResolver) ResolveWithIdentity(_ context.Context, ref string) (string, Identity, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", Identity{}, err
	}
	if reference.Provider != providerEnv {
		return "", Identity{}, fmt.Errorf("env resolver cannot handle provider %q", reference.Provider)
	}

	name := reference.Query.Get("name")
	if name == "" {
		name = strings.Join(reference.Segments, "_")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", Identity{}, fmt.Errorf("env secret %q requires a name", ref)
	}

	value, ok := os.LookupEnv(name)
	if !ok {
		return "", Identity{}, fmt.Errorf("environment variable %s not set", name)
	}

	return value, Identity{
		Provider:           providerEnv,
		Ref:                ref,
		Name:               name,
		Verifiable:         false,
		UnverifiableReason: "environment variables have no provider version identity",
	}, nil
}
