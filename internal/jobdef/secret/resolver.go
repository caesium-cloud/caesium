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

// IdentityVerifier verifies a previously captured identity without relying on
// the provider's current/latest version. Replay gates use it when a provider can
// pin the baseline version and recompute the recorded identity proof.
type IdentityVerifier interface {
	VerifyIdentity(ctx context.Context, ref string, expected Identity) (Identity, error)
}

// ResolvedIdentityVerifier verifies a captured identity against a value already
// returned by ResolveWithIdentity. Replay uses it to avoid re-reading a pinned
// provider version when the latest version is the baseline version.
type ResolvedIdentityVerifier interface {
	VerifyResolvedIdentity(ctx context.Context, ref string, expected Identity, resolvedValue string) (Identity, error)
}
