package job

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	exportServer string
	exportAPIKey string
	exportOutput string

	exportHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

var exportCmd = &cobra.Command{
	Use:   "export <job-id-or-alias>",
	Short: "Export a live job back to its authoring manifest",
	Long: "Reconstruct a deployed job's YAML manifest from the server (GET /v1/jobs/:id/manifest) " +
		"and write it to stdout, or to a file with --output. The manifest is re-appliable: " +
		"`caesium job lint` accepts it and `caesium job diff` reports no changes against the " +
		"server it came from.\n\n" +
		"Two things cannot be recovered because the server never stored them: a volume's " +
		"alternative per-engine sources (only the sources the job's own steps resolved are " +
		"persisted) and its optional accessMode. Job-level serviceAccountName/podAnnotations/" +
		"automountServiceAccountToken come back on each kubernetes step, which is equivalent.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(strings.TrimSpace(exportServer), "/")
		apiKey := cliutil.ResolveAPIKey(cmd, exportAPIKey, cliutil.APIKeyEnvVar)

		jobID, err := resolveExportJobID(cmd, server, apiKey, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		manifest, err := exportGet(cmd, apiKey, fmt.Sprintf("%s/v1/jobs/%s/manifest", server, url.PathEscape(jobID)), "job export")
		if err != nil {
			return err
		}

		if exportOutput == "" {
			// stdout carries the manifest and nothing else, so
			// `caesium job export x > job.yaml` is byte-exact.
			_, err := cmd.OutOrStdout().Write(manifest)
			return err
		}
		if err := os.WriteFile(exportOutput, manifest, 0o644); err != nil {
			return fmt.Errorf("write manifest to %s: %w", exportOutput, err)
		}
		// Confirmation goes to stderr so stdout stays either the manifest or
		// empty — never a mix a script would have to strip.
		cmd.PrintErrf("Wrote manifest to %s\n", exportOutput)
		return nil
	},
}

type exportJobSummary struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

// resolveExportJobID accepts a job UUID verbatim and resolves anything else as
// an alias through GET /v1/jobs, matching how `caesium blame <job>` does it.
func resolveExportJobID(cmd *cobra.Command, server, apiKey, idOrAlias string) (string, error) {
	if idOrAlias == "" {
		return "", fmt.Errorf("job id or alias is required")
	}
	if _, err := uuid.Parse(idOrAlias); err == nil {
		return idOrAlias, nil
	}

	body, err := exportGet(cmd, apiKey, server+"/v1/jobs", "job lookup")
	if err != nil {
		return "", err
	}
	var jobs []exportJobSummary
	if err := json.Unmarshal(body, &jobs); err != nil {
		return "", fmt.Errorf("job lookup returned invalid JSON: %w", err)
	}
	for _, entry := range jobs {
		if entry.Alias == idOrAlias {
			return entry.ID, nil
		}
	}
	return "", fmt.Errorf("job alias %q not found", idOrAlias)
}

func exportGet(cmd *cobra.Command, apiKey, reqURL, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := exportHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", label, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s failed (%d): %s", label, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func init() {
	exportCmd.Flags().StringVar(&exportServer, "server", "http://localhost:8080", "Caesium server base URL")
	exportCmd.Flags().StringVar(&exportAPIKey, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	exportCmd.Flags().StringVarP(&exportOutput, "output", "o", "", "Write the manifest to this file (default: stdout)")
	Cmd.AddCommand(exportCmd)
}
