package secret

import "context"

// Resolver resolves a secret reference into a concrete value.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}
