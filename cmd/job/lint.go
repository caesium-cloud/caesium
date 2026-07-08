package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
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
	lintServer           string
	lintAPIKey           string
	lintJSON             bool

	lintHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

var lintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Validate job definition manifests",
	RunE: func(cmd *cobra.Command, args []string) error {
		defs, err := collectDefinitions(lintPaths)
		if err != nil {
			return err
		}
		serverMode := cmd.Flags().Changed("server")
		if lintJSON && !serverMode {
			return fmt.Errorf("--json is only supported with --server")
		}
		if len(defs) == 0 {
			if err := writeCmdOut(cmd, "No job definitions found.\n"); err != nil {
				return err
			}
			return nil
		}

		if serverMode {
			return runServerLint(cmd, defs)
		}

		for _, def := range defs {
			if err := def.Validate(); err != nil {
				return fmt.Errorf("definition %s: %w", def.Metadata.Alias, err)
			}
		}
		if err := internaljobdef.ValidateTriggerChains(cmd.Context(), nil, defs); err != nil {
			return err
		}
		if err := internaljobdef.ValidateDatasetGraph(cmd.Context(), nil, defs); err != nil {
			return err
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
			if err := writeLintTriggerScopeNote(cmd); err != nil {
				return err
			}
			if err := writeLintRemediationScopeNote(cmd, defs); err != nil {
				return err
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
		if err := writeLintTriggerScopeNote(cmd); err != nil {
			return err
		}
		if err := writeLintRemediationScopeNote(cmd, defs); err != nil {
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
	lintCmd.Flags().StringVar(&lintServer, "server", "http://localhost:8080", "POST definitions to the Caesium server for persisted-world linting; pass without a value to use http://localhost:8080")
	if flag := lintCmd.Flags().Lookup("server"); flag != nil {
		flag.NoOptDefVal = "http://localhost:8080"
	}
	lintCmd.Flags().StringVar(&lintAPIKey, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	lintCmd.Flags().BoolVar(&lintJSON, "json", false, "Print server lint JSON (requires --server)")

	Cmd.AddCommand(lintCmd)
}

// LoadDefinitions exposes the same manifest loader used by `caesium job lint`
// for sibling CLI groups that need to lint the exact local path set.
func LoadDefinitions(paths []string) ([]jobdef.Definition, error) {
	return collectDefinitions(paths)
}

type ServerLintRequest struct {
	Definitions []jobdef.Definition `json:"definitions"`
}

type ServerLintMessage struct {
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
}

type ServerLintSummary struct {
	Steps     int    `json:"steps"`
	Contracts string `json:"contracts,omitempty"`
}

type ServerLintResponse struct {
	Errors    []ServerLintMessage    `json:"errors"`
	Warnings  []ServerLintMessage    `json:"warnings"`
	Summary   ServerLintSummary      `json:"summary"`
	Contracts *ServerContractSummary `json:"contracts,omitempty"`
}

type ServerContractSummary struct {
	Breaking []ServerContractFinding `json:"breaking"`
	Warnings []ServerContractFinding `json:"warnings"`
	Edges    int                     `json:"edges"`
}

type ServerContractFinding struct {
	EdgeID    string            `json:"edgeId,omitempty"`
	EdgeClass string            `json:"edgeClass,omitempty"`
	From      string            `json:"from,omitempty"`
	To        string            `json:"to,omitempty"`
	Dataset   *ServerDatasetRef `json:"dataset,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Path      string            `json:"path,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Verdict   string            `json:"verdict"`
}

type ServerDatasetRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func runServerLint(cmd *cobra.Command, defs []jobdef.Definition) error {
	server := strings.TrimSuffix(lintServer, "/")
	apiKey := cliutil.ResolveAPIKey(cmd, lintAPIKey, cliutil.APIKeyEnvVar)
	if err := ensureContractLintEnabled(cmd.Context(), server, apiKey); err != nil {
		return err
	}

	resp, body, err := PostServerLint(cmd.Context(), server, apiKey, defs)
	if err != nil {
		return err
	}
	if lintJSON {
		if err := cliutil.WritePrettyJSON(cmd, body, "job lint response"); err != nil {
			return err
		}
	} else if err := renderServerLintResponse(cmd, resp); err != nil {
		return err
	}

	if len(resp.Errors) > 0 {
		return fmt.Errorf("server lint failed: %s", joinServerLintMessages(resp.Errors))
	}
	if resp.Contracts == nil {
		return fmt.Errorf("server lint did not include contract findings; set CAESIUM_CONTRACT_ENFORCEMENT=fail (or warn) on the server")
	}
	if len(resp.Contracts.Breaking) > 0 {
		return fmt.Errorf("server lint found %d breaking contract finding(s)", len(resp.Contracts.Breaking))
	}
	return nil
}

func PostServerLint(ctx context.Context, server, apiKey string, defs []jobdef.Definition) (*ServerLintResponse, []byte, error) {
	payload, err := json.Marshal(ServerLintRequest{Definitions: defs})
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(server, "/")+"/v1/jobdefs/lint", bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := lintHTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading job lint response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, nil, fmt.Errorf("job lint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var lintResp ServerLintResponse
	if err := json.Unmarshal(body, &lintResp); err != nil {
		return nil, nil, fmt.Errorf("job lint response was not valid JSON: %w", err)
	}
	return &lintResp, body, nil
}

type serverFeaturesResponse struct {
	ContractEnforcementEnabled bool `json:"contract_enforcement_enabled"`
}

func ensureContractLintEnabled(ctx context.Context, server, apiKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(server, "/")+"/v1/system/features", nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := lintHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading system features response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("contract enforcement feature status is unavailable; set CAESIUM_CONTRACT_ENFORCEMENT=fail (or warn) on the Caesium server and restart it")
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("system features failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var features serverFeaturesResponse
	if err := json.Unmarshal(body, &features); err != nil {
		return fmt.Errorf("system features response was not valid JSON: %w", err)
	}
	if !features.ContractEnforcementEnabled {
		return fmt.Errorf("contract enforcement is disabled on the server; set CAESIUM_CONTRACT_ENFORCEMENT=fail (or warn) and restart it")
	}
	return nil
}

func renderServerLintResponse(cmd *cobra.Command, resp *ServerLintResponse) error {
	if len(resp.Errors) > 0 {
		if err := writeCmdOut(cmd, "Errors:\n"); err != nil {
			return err
		}
		for _, msg := range resp.Errors {
			if err := writeCmdOut(cmd, "  - %s\n", msg.Message); err != nil {
				return err
			}
		}
	}
	if len(resp.Warnings) > 0 {
		if err := writeCmdOut(cmd, "Warnings:\n"); err != nil {
			return err
		}
		for _, msg := range resp.Warnings {
			if err := writeCmdOut(cmd, "  - %s\n", msg.Message); err != nil {
				return err
			}
		}
	}
	if len(resp.Errors) == 0 {
		if err := writeCmdOut(cmd, "Validated %d step(s) against server state\n", resp.Summary.Steps); err != nil {
			return err
		}
		if resp.Summary.Contracts != "" {
			if err := writeCmdOut(cmd, "Declared data contracts: %s\n", resp.Summary.Contracts); err != nil {
				return err
			}
		}
	}
	return renderServerContractSummary(cmd, resp.Contracts)
}

func renderServerContractSummary(cmd *cobra.Command, summary *ServerContractSummary) error {
	if summary == nil {
		return writeCmdOut(cmd, "Contracts: not reported; set CAESIUM_CONTRACT_ENFORCEMENT=fail (or warn) on the server.\n")
	}
	if err := writeCmdOut(cmd, "Contracts: %d edge(s), %d breaking, %d warning(s)\n", summary.Edges, len(summary.Breaking), len(summary.Warnings)); err != nil {
		return err
	}
	if len(summary.Breaking) > 0 {
		if err := writeCmdOut(cmd, "Breaking:\n"); err != nil {
			return err
		}
		if err := writeServerContractFindings(cmd, summary.Breaking); err != nil {
			return err
		}
	}
	if len(summary.Warnings) > 0 {
		if err := writeCmdOut(cmd, "Warnings:\n"); err != nil {
			return err
		}
		if err := writeServerContractFindings(cmd, summary.Warnings); err != nil {
			return err
		}
	}
	return nil
}

func writeServerContractFindings(cmd *cobra.Command, findings []ServerContractFinding) error {
	for _, finding := range findings {
		if err := writeCmdOut(cmd, "  - %s -> %s [%s] %s %s: %s\n",
			dashIfEmpty(finding.From),
			dashIfEmpty(finding.To),
			dashIfEmpty(finding.EdgeClass),
			dashIfEmpty(finding.Kind),
			dashIfEmpty(finding.Path),
			dashIfEmpty(finding.Detail),
		); err != nil {
			return err
		}
	}
	return nil
}

func joinServerLintMessages(messages []ServerLintMessage) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		parts = append(parts, msg.Message)
	}
	return strings.Join(parts, "; ")
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func writeLintTriggerScopeNote(cmd *cobra.Command) error {
	return writeCmdOut(cmd, "Note: trigger-cycle lint is file-scoped; cross-job cycles against persisted triggers are validated at apply.\n")
}

// writeLintRemediationScopeNote prints the offline-lint scope gap for
// metadata.remediation: profile (an AgentProfile reference) and
// escalation.channel (a NotificationChannel reference) name server-side
// resources this offline pass has no database connection to verify. Only
// emitted when at least one definition actually declares a remediation
// block, so jobs that don't use the feature see no extra noise.
func writeLintRemediationScopeNote(cmd *cobra.Command, defs []jobdef.Definition) error {
	for i := range defs {
		if defs[i].Metadata.Remediation != nil {
			return writeCmdOut(cmd, "Note: metadata.remediation.profile / escalation.channel references are unverified offline; verified by server-side lint (POST /v1/jobdefs/lint) and at apply.\n")
		}
	}
	return nil
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

	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].producer != contracts[j].producer {
			return contracts[i].producer < contracts[j].producer
		}
		if contracts[i].consumer != contracts[j].consumer {
			return contracts[i].consumer < contracts[j].consumer
		}
		return strings.Join(contracts[i].keys, "\x00") < strings.Join(contracts[j].keys, "\x00")
	})

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
