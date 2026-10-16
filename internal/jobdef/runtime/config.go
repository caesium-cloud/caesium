package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// Watch encapsulates the configuration required to start a Git sync watcher.
type Watch struct {
	Source   git.Source
	Interval time.Duration
	Once     bool
}

// BuildSecretResolver constructs the resolver chain based on environment variables.
func BuildSecretResolver(vars env.Environment) (*secret.MultiResolver, error) {
	cfg := secret.Config{
		EnableEnv:           vars.JobdefSecretsEnableEnv,
		IdentityHMACKeys:    vars.JobdefSecretsIdentityHMACKeys,
		IdentityHMACCurrent: vars.JobdefSecretsIdentityHMACKeyID,
	}

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

		namespaces, err := gitSourceNamespaces(cfg.Namespaces)
		if err != nil {
			return nil, fmt.Errorf("jobdef git source %d: %w", idx, err)
		}
		if namespaces != nil && strings.TrimSpace(cfg.SourceID) == "" {
			return nil, fmt.Errorf("jobdef git source %d: namespaces requires a non-empty source_id", idx)
		}

		source := git.Source{
			URL:        cfg.URL,
			Ref:        cfg.Ref,
			Path:       cfg.Path,
			Globs:      cfg.Globs,
			SourceID:   cfg.SourceID,
			LocalDir:   cfg.LocalDir,
			Resolver:   resolver,
			Namespaces: namespaces,
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

	if err := rejectSharedRestrictedSourceIDs(vars.JobdefGitSources); err != nil {
		return nil, err
	}
	return watches, nil
}

// gitSourceNamespaces validates a source's namespace allowlist. nil means the
// source is unrestricted; any other value must name at least one valid
// namespace and comes back sorted, de-duplicated and non-nil.
// B2: also reject namespaces the policy file does not declare.
func gitSourceNamespaces(namespaces []string) ([]string, error) {
	if namespaces == nil {
		return nil, nil
	}
	if len(namespaces) == 0 {
		return nil, errors.New("namespaces must list at least one namespace; omit the field for an unrestricted source")
	}
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns == "" || ns == "*" || jobdefschema.ValidateNamespace(ns) != nil {
			return nil, fmt.Errorf("namespaces entry %q must be a DNS label (1-63 lowercase letters, digits or hyphens, beginning and ending with a letter or digit); omit the field for an unrestricted source", ns)
		}
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out, nil
}

// rejectSharedRestrictedSourceIDs refuses a source_id shared by two sources when
// either has namespaces: the sibling would prune the restricted source's jobs
// outside its allowlist and pass the provenance check against them. Shared ids
// between unrestricted sources stay accepted (pre-existing behaviour).
func rejectSharedRestrictedSourceIDs(sources env.GitSources) error {
	var order []string
	groups := make(map[string][]int, len(sources))
	for idx, cfg := range sources {
		id := strings.TrimSpace(cfg.SourceID)
		if id == "" {
			continue
		}
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], idx)
	}
	for _, id := range order {
		members := groups[id]
		if len(members) < 2 || !slices.ContainsFunc(members, func(idx int) bool { return sources[idx].Namespaces != nil }) {
			continue
		}
		return fmt.Errorf("jobdef git sources %d and %d share source_id %q; a source with namespaces needs a unique source_id", members[0], members[1], id)
	}
	return nil
}
