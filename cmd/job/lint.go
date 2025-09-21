package job

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	lintpkg "github.com/caesium-cloud/caesium/internal/jobdef/lint"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
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
			fmt.Fprintln(cmd.OutOrStdout(), "No job definitions found.")
			return nil
		}

		for _, def := range defs {
			if err := def.Validate(); err != nil {
				return fmt.Errorf("definition %s: %w", def.Metadata.Alias, err)
			}
		}

		if !lintCheckSecrets {
			fmt.Fprintf(cmd.OutOrStdout(), "Validated %d job definition(s)\n", len(defs))
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

		fmt.Fprintf(cmd.OutOrStdout(), "Validated %d job definition(s) with secrets\n", len(defs))
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
