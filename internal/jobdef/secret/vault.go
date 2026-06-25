package secret

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	vault "github.com/hashicorp/vault/api"
)

const providerVault = "vault"

type vaultLogical interface {
	ReadWithContext(ctx context.Context, path string) (*vault.Secret, error)
	ReadWithDataWithContext(ctx context.Context, path string, data map[string][]string) (*vault.Secret, error)
}

// VaultConfig describes how to connect to a Vault cluster.
type VaultConfig struct {
	Address         string
	Token           string
	Namespace       string
	CACertPath      string
	TLSSkipVerify   bool
	IdentityKeyring *IdentityKeyring
}

// VaultResolver reads secrets from HashiCorp Vault logical paths.
type VaultResolver struct {
	logical         vaultLogical
	identityKeyring *IdentityKeyring
}

// NewVaultResolver builds a resolver using the provided configuration.
func NewVaultResolver(cfg VaultConfig) (*VaultResolver, error) {
	address := strings.TrimSpace(cfg.Address)
	if address == "" {
		return nil, errors.New("vault address is required")
	}

	clientConfig := &vault.Config{Address: address}
	if cfg.CACertPath != "" || cfg.TLSSkipVerify {
		if err := clientConfig.ConfigureTLS(&vault.TLSConfig{CACert: cfg.CACertPath, Insecure: cfg.TLSSkipVerify}); err != nil {
			return nil, fmt.Errorf("configure vault tls: %w", err)
		}
	}

	client, err := vault.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("create vault client: %w", err)
	}

	if token := strings.TrimSpace(cfg.Token); token != "" {
		client.SetToken(token)
	}

	if ns := strings.TrimSpace(cfg.Namespace); ns != "" {
		client.SetNamespace(ns)
	}

	return &VaultResolver{logical: client.Logical(), identityKeyring: cfg.IdentityKeyring}, nil
}

// NewVaultResolverWithLogical constructs a resolver with a preconfigured logical client (useful in tests).
func NewVaultResolverWithLogical(logical vaultLogical) *VaultResolver {
	return &VaultResolver{logical: logical}
}

// NewVaultResolverWithLogicalAndKeyring constructs a test resolver with an
// identity keyring.
func NewVaultResolverWithLogicalAndKeyring(logical vaultLogical, keyring *IdentityKeyring) *VaultResolver {
	return &VaultResolver{logical: logical, identityKeyring: keyring}
}

// Resolve implements the Resolver interface.
func (r *VaultResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := r.ResolveWithIdentity(ctx, ref)
	return value, err
}

// ResolveWithIdentity implements the Resolver interface.
func (r *VaultResolver) ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", Identity{}, err
	}
	if reference.Provider != providerVault {
		return "", Identity{}, fmt.Errorf("vault resolver cannot handle provider %q", reference.Provider)
	}

	if r.logical == nil {
		return "", Identity{}, errors.New("vault logical client not configured")
	}

	path, field, err := parseVaultPathField(reference)
	if err != nil {
		return "", Identity{}, err
	}

	secret, err := r.logical.ReadWithContext(ctx, path)
	if err != nil {
		return "", Identity{}, fmt.Errorf("read vault secret %s: %w", path, err)
	}
	if secret == nil {
		return "", Identity{}, fmt.Errorf("vault secret %s not found", path)
	}

	version := vaultKVv2Version(secret)
	if version == "" {
		if value, ok := extractVaultField(secret, field); ok {
			return value, Identity{
				Provider:           providerVault,
				Ref:                ref,
				Verifiable:         false,
				UnverifiableReason: "vault secret response has no KV-v2 metadata.version",
				Metadata:           map[string]string{"path": path, "field": field},
			}, nil
		}
		return "", Identity{}, fmt.Errorf("vault secret %s missing field %s", path, field)
	}

	versionedSecret, err := r.logical.ReadWithDataWithContext(ctx, path, map[string][]string{"version": {version}})
	if err != nil {
		return "", Identity{}, fmt.Errorf("read vault secret %s version %s: %w", path, version, err)
	}
	if versionedSecret == nil {
		return "", Identity{}, fmt.Errorf("vault secret %s version %s not found", path, version)
	}
	value, ok := extractVaultField(versionedSecret, field)
	if !ok {
		return "", Identity{}, fmt.Errorf("vault secret %s version %s missing field %s", path, version, field)
	}

	identity := Identity{
		Provider: providerVault,
		Ref:      ref,
		Version:  version,
		Metadata: map[string]string{
			"path":  path,
			"field": field,
		},
	}
	if keyID, digest, ok := r.identityKeyring.CurrentHMAC([]byte(value)); ok {
		identity.KeyID = keyID
		identity.HMACSHA256 = digest
		identity.Verifiable = true
	} else {
		identity.Verifiable = false
		identity.UnverifiableReason = "vault identity HMAC keyring is not configured"
	}
	return value, identity, nil
}

func parseVaultPathField(reference *Reference) (path, field string, err error) {
	field = strings.TrimSpace(reference.Query.Get("field"))
	segments := slices.Clone(reference.Segments)

	if field == "" && len(segments) >= 2 {
		field = strings.TrimSpace(segments[len(segments)-1])
		segments = segments[:len(segments)-1]
	}

	path = strings.Join(segments, "/")
	if path == "" {
		return "", "", fmt.Errorf("vault secret %q missing path", reference.Raw)
	}
	if field == "" {
		return "", "", fmt.Errorf("vault secret %q missing field (provide ?field= or include as final path segment)", reference.Raw)
	}
	return path, field, nil
}

func extractVaultField(secret *vault.Secret, field string) (string, bool) {
	if secret == nil {
		return "", false
	}

	if secret.Data != nil {
		if nested, ok := secret.Data["data"].(map[string]any); ok {
			if val, ok := nested[field]; ok {
				return fmt.Sprintf("%v", val), true
			}
		}
		if val, ok := secret.Data[field]; ok {
			return fmt.Sprintf("%v", val), true
		}
	}

	return "", false
}

func vaultKVv2Version(secret *vault.Secret) string {
	if secret == nil || secret.Data == nil {
		return ""
	}
	metadata, ok := secret.Data["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	if version, ok := metadata["version"]; ok {
		return strings.TrimSpace(fmt.Sprintf("%v", version))
	}
	return ""
}
