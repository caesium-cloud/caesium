package secret

// Config defines which providers should be available in a MultiResolver.
type Config struct {
	EnableEnv  bool
	Kubernetes *KubernetesConfig
	Vault      *VaultConfig
}

// NewConfiguredResolver builds a MultiResolver using the supplied provider configuration.
func NewConfiguredResolver(cfg Config) (*MultiResolver, error) {
	providers := map[string]Resolver{}

	if cfg.EnableEnv {
		providers[providerEnv] = NewEnvResolver()
	}

	if cfg.Kubernetes != nil {
		kubeResolver := NewKubernetesResolver(*cfg.Kubernetes)
		providers[providerKubernetes] = kubeResolver
		// Support both k8s:// and kubernetes:// for clarity.
		providers["kubernetes"] = kubeResolver
	}

	if cfg.Vault != nil {
		resolver, err := NewVaultResolver(*cfg.Vault)
		if err != nil {
			return nil, err
		}
		providers[providerVault] = resolver
	}

	return NewMultiResolver(providers), nil
}
