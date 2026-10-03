package secret

import (
	"context"
	"errors"
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

	name, err := EnvVarName(reference)
	if err != nil {
		return "", Identity{}, err
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

// EnvVarName returns the variable EnvResolver reads for reference.
// It does not look the variable up.
func EnvVarName(reference *Reference) (string, error) {
	if reference == nil {
		return "", errors.New("env secret reference requires a name")
	}
	name := ""
	if reference.Query != nil {
		name = reference.Query.Get("name")
	}
	if name == "" {
		name = strings.Join(reference.Segments, "_")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("env secret %q requires a name", reference.Raw)
	}
	return name, nil
}
