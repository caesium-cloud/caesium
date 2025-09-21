package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"

	vault "github.com/hashicorp/vault/api"
)

const providerVault = "vault"

type vaultLogical interface {
	ReadWithContext(ctx context.Context, path string) (*vault.Secret, error)
}

// VaultConfig describes how to connect to a Vault cluster.
type VaultConfig struct {
	Address       string
	Token         string
	Namespace     string
	CACertPath    string
	TLSSkipVerify bool
}

// VaultResolver reads secrets from HashiCorp Vault logical paths.
type VaultResolver struct {
	logical vaultLogical
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

	return &VaultResolver{logical: client.Logical()}, nil
}

// NewVaultResolverWithLogical constructs a resolver with a preconfigured logical client (useful in tests).
func NewVaultResolverWithLogical(logical vaultLogical) *VaultResolver {
	return &VaultResolver{logical: logical}
}

// Resolve implements the Resolver interface.
func (r *VaultResolver) Resolve(ctx context.Context, ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerVault {
		return "", fmt.Errorf("vault resolver cannot handle provider %q", reference.Provider)
	}

	if r.logical == nil {
		return "", errors.New("vault logical client not configured")
	}

	field := strings.TrimSpace(reference.Query.Get("field"))
	segments := append([]string(nil), reference.Segments...)

	if field == "" && len(segments) >= 2 {
		field = strings.TrimSpace(segments[len(segments)-1])
		segments = segments[:len(segments)-1]
	}

	path := strings.Join(segments, "/")
	if path == "" {
		return "", fmt.Errorf("vault secret %q missing path", ref)
	}
	if field == "" {
		return "", fmt.Errorf("vault secret %q missing field (provide ?field= or include as final path segment)", ref)
	}

	secret, err := r.logical.ReadWithContext(ctx, path)
	if err != nil {
		return "", fmt.Errorf("read vault secret %s: %w", path, err)
	}
	if secret == nil {
		return "", fmt.Errorf("vault secret %s not found", path)
	}

	if value, ok := extractVaultField(secret, field); ok {
		return value, nil
	}

	return "", fmt.Errorf("vault secret %s missing field %s", path, field)
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
