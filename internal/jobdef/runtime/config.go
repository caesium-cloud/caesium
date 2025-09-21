package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/env"
)

// Watch encapsulates the configuration required to start a Git sync watcher.
type Watch struct {
	Source   git.Source
	Interval time.Duration
	Once     bool
}

// BuildSecretResolver constructs the resolver chain based on environment variables.
func BuildSecretResolver(vars env.Environment) (*secret.MultiResolver, error) {
	cfg := secret.Config{EnableEnv: vars.JobdefSecretsEnableEnv}

	if vars.JobdefSecretsEnableKubernetes || vars.JobdefSecretsKubeConfig != "" || vars.JobdefSecretsKubeNamespace != "default" {
		cfg.Kubernetes = &secret.KubernetesConfig{
			KubeConfigPath: vars.JobdefSecretsKubeConfig,
			Namespace:      vars.JobdefSecretsKubeNamespace,
		}
	}

	if strings.TrimSpace(vars.JobdefSecretsVaultAddress) != "" {
		cfg.Vault = &secret.VaultConfig{
			Address:       vars.JobdefSecretsVaultAddress,
			Token:         vars.JobdefSecretsVaultToken,
			Namespace:     vars.JobdefSecretsVaultNamespace,
			CACertPath:    vars.JobdefSecretsVaultCACert,
			TLSSkipVerify: vars.JobdefSecretsVaultSkipVerify,
		}
	}

	return secret.NewConfiguredResolver(cfg)
}

// BuildGitWatches converts the Git environment configuration into watch descriptors.
func BuildGitWatches(vars env.Environment, resolver secret.Resolver) ([]Watch, error) {
	if !vars.JobdefGitEnabled || len(vars.JobdefGitSources) == 0 {
		return nil, nil
	}

	watches := make([]Watch, 0, len(vars.JobdefGitSources))
	for idx, cfg := range vars.JobdefGitSources {
		if cfg.IsZero() {
			return nil, fmt.Errorf("jobdef git source %d missing url", idx)
		}

		interval, err := cfg.IntervalDuration(vars.JobdefGitInterval)
		if err != nil {
			return nil, fmt.Errorf("jobdef git source %d: %w", idx, err)
		}

		source := git.Source{
			URL:      cfg.URL,
			Ref:      cfg.Ref,
			Path:     cfg.Path,
			Globs:    cfg.Globs,
			SourceID: cfg.SourceID,
			LocalDir: cfg.LocalDir,
			Resolver: resolver,
		}

		if cfg.Auth != nil {
			source.Auth = &git.BasicAuth{
				Username:    cfg.Auth.Username,
				Password:    cfg.Auth.Password,
				UsernameRef: cfg.Auth.UsernameRef,
				PasswordRef: cfg.Auth.PasswordRef,
			}
		}

		if cfg.SSH != nil {
			source.SSH = &git.SSHAuth{
				Username:        cfg.SSH.Username,
				UsernameRef:     cfg.SSH.UsernameRef,
				PrivateKey:      cfg.SSH.PrivateKey,
				PrivateKeyRef:   cfg.SSH.PrivateKeyRef,
				Passphrase:      cfg.SSH.Passphrase,
				PassphraseRef:   cfg.SSH.PassphraseRef,
				KnownHosts:      cfg.SSH.KnownHosts,
				KnownHostsRef:   cfg.SSH.KnownHostsRef,
				KnownHostsPath:  cfg.SSH.KnownHostsPath,
				KnownHostsPaths: cfg.SSH.KnownHostsPaths,
			}
		}

		watches = append(watches, Watch{
			Source:   source,
			Interval: interval,
			Once:     cfg.OnceValue(vars.JobdefGitOnce),
		})
	}

	return watches, nil
}
