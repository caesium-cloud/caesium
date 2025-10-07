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
func (r *EnvResolver) Resolve(_ context.Context, ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerEnv {
		return "", fmt.Errorf("env resolver cannot handle provider %q", reference.Provider)
	}

	name := reference.Query.Get("name")
	if name == "" {
		name = strings.Join(reference.Segments, "_")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("env secret %q requires a name", ref)
	}

	value, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("environment variable %s not set", name)
	}

	return value, nil
}
