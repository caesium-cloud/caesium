package secret

import "context"

// Identity describes the provider-specific baseline identity for a resolved
// secret reference. It never contains the secret value.
type Identity struct {
	Provider           string            `json:"provider"`
	Ref                string            `json:"ref"`
	Version            string            `json:"version,omitempty"`
	ResourceVersion    string            `json:"resourceVersion,omitempty"`
	Namespace          string            `json:"namespace,omitempty"`
	Name               string            `json:"name,omitempty"`
	Key                string            `json:"key,omitempty"`
	KeyID              string            `json:"keyId,omitempty"`
	HMACSHA256         string            `json:"hmacSha256,omitempty"`
	Verifiable         bool              `json:"verifiable"`
	UnverifiableReason string            `json:"unverifiableReason,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// Resolver resolves a secret reference into a concrete value.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
	ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error)
}
