package job

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	lintpkg "github.com/caesium-cloud/caesium/internal/jobdef/lint"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
)

var (
	lintPaths            []string
	lintCheckSecrets     bool
	lintEnableKubernetes bool
	lintKubeConfig       string
	lintKubeNamespace    string
	lintVaultAddress     string
	lintVaultToken       string
	lintVaultNamespace   string
	lintVaultCACert      string
	lintVaultSkipVerify  bool
)

var lintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Validate job definition manifests",
	RunE: func(cmd *cobra.Command, args []string) error {
		defs, err := collectDefinitions(lintPaths)
		if err != nil {
			return err
		}
		if len(defs) == 0 {
			if err := writeCmdOut(cmd, "No job definitions found.\n"); err != nil {
				return err
			}
			return nil
		}

		for _, def := range defs {
			if err := def.Validate(); err != nil {
				return fmt.Errorf("definition %s: %w", def.Metadata.Alias, err)
			}
		}

		if !lintCheckSecrets {
			if err := writeCmdOut(cmd, "Validated %d job definition(s)\n", len(defs)); err != nil {
				return err
			}
			for _, def := range defs {
				if summary := contractSummary(def.Steps); summary != "" {
					if err := writeCmdOut(cmd, "  %s: %s\n", def.Metadata.Alias, summary); err != nil {
						return err
					}
				}
			}
			return nil
		}

		resolver, err := buildLintResolver()
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		errs := lintpkg.CheckSecrets(ctx, resolver, defs)
		if len(errs) > 0 {
			return fmt.Errorf("secret checks failed:\n%s", strings.Join(errs, "\n"))
		}

		if err := writeCmdOut(cmd, "Validated %d job definition(s) with secrets\n", len(defs)); err != nil {
			return err
		}
		return nil
	},
}

func init() {
	lintCmd.Flags().StringSliceVarP(&lintPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	lintCmd.Flags().BoolVar(&lintCheckSecrets, "check-secrets", false, "Resolve secret:// references using configured providers")
	lintCmd.Flags().BoolVar(&lintEnableKubernetes, "enable-kubernetes", envBool("CAESIUM_LINT_ENABLE_K8S", false), "Enable Kubernetes secret resolver during --check-secrets")
	lintCmd.Flags().StringVar(&lintKubeConfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig for resolving Kubernetes secrets")
	lintCmd.Flags().StringVar(&lintKubeNamespace, "kube-namespace", firstNonEmpty(os.Getenv("KUBERNETES_NAMESPACE"), "default"), "Default Kubernetes namespace for secrets")
	lintCmd.Flags().StringVar(&lintVaultAddress, "vault-address", os.Getenv("VAULT_ADDR"), "Vault server address for secret resolution")
	lintCmd.Flags().StringVar(&lintVaultToken, "vault-token", os.Getenv("VAULT_TOKEN"), "Vault token for secret resolution")
	lintCmd.Flags().StringVar(&lintVaultNamespace, "vault-namespace", os.Getenv("VAULT_NAMESPACE"), "Vault namespace for secret resolution")
	lintCmd.Flags().StringVar(&lintVaultCACert, "vault-ca-cert", os.Getenv("VAULT_CACERT"), "Vault CA certificate path")
	lintCmd.Flags().BoolVar(&lintVaultSkipVerify, "vault-skip-verify", envBool("VAULT_SKIP_VERIFY", false), "Disable TLS verification when connecting to Vault")

	Cmd.AddCommand(lintCmd)
}

func buildLintResolver() (*secret.MultiResolver, error) {
	cfg := secret.Config{EnableEnv: true}

	enableK8s := lintEnableKubernetes
	if !enableK8s {
		if lintKubeConfig != "" || os.Getenv("KUBECONFIG") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			enableK8s = true
		}
	}
	if enableK8s {
		cfg.Kubernetes = &secret.KubernetesConfig{
			KubeConfigPath: firstNonEmpty(lintKubeConfig, os.Getenv("KUBECONFIG")),
			Namespace:      lintKubeNamespace,
		}
	}

	vaultAddr := firstNonEmpty(lintVaultAddress, os.Getenv("VAULT_ADDR"))
	if vaultAddr != "" {
		cfg.Vault = &secret.VaultConfig{
			Address:       vaultAddr,
			Token:         firstNonEmpty(lintVaultToken, os.Getenv("VAULT_TOKEN")),
			Namespace:     firstNonEmpty(lintVaultNamespace, os.Getenv("VAULT_NAMESPACE")),
			CACertPath:    firstNonEmpty(lintVaultCACert, os.Getenv("VAULT_CACERT")),
			TLSSkipVerify: lintVaultSkipVerify || envBool("VAULT_SKIP_VERIFY", false),
		}
	}

	return secret.NewConfiguredResolver(cfg)
}

func envBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// contractSummary returns a human-readable description of data contracts in a step list,
// or "" if there are none.
func contractSummary(steps []jobdef.Step) string {
	type contract struct {
		producer string
		consumer string
		keys     []string
	}
	var contracts []contract

	for _, step := range steps {
		if step.InputSchema == nil {
			continue
		}
		for producerName, schema := range step.InputSchema {
			var keys []string
			if req, ok := schema["required"].([]any); ok {
				for _, k := range req {
					if s, ok := k.(string); ok {
						keys = append(keys, s)
					}
				}
			}
			sort.Strings(keys)
			contracts = append(contracts, contract{
				producer: producerName,
				consumer: step.Name,
				keys:     keys,
			})
		}
	}

	if len(contracts) == 0 {
		return ""
	}

	parts := make([]string, 0, len(contracts))
	for _, c := range contracts {
		if len(c.keys) > 0 {
			parts = append(parts, fmt.Sprintf("%s \u2192 %s: %s", c.producer, c.consumer, strings.Join(c.keys, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("%s \u2192 %s", c.producer, c.consumer))
		}
	}

	n := len(contracts)
	noun := "data contract"
	if n != 1 {
		noun = "data contracts"
	}
	return fmt.Sprintf("%d %s (%s)", n, noun, strings.Join(parts, "; "))
}
