package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	createRole        string
	createDescription string
	createExpiresIn   string
	createServer      string
	createAPIKey      string
	createScopeJobs   []string
)

var keyCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new API key",
	Example: `  caesium auth key create --role operator --description "CI deploy key" --expires 90d
  caesium auth key create --role runner --description "ETL runner" --scope-jobs etl-daily,etl-hourly`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(createServer, "/")
		apiKey := resolveAPIKey(cmd, createAPIKey)

		body := map[string]any{
			"role":        createRole,
			"description": createDescription,
		}
		if createExpiresIn != "" {
			body["expires_in"] = createExpiresIn
		}
		if len(createScopeJobs) > 0 {
			body["scope"] = map[string]any{
				"jobs": createScopeJobs,
			}
		}

		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, server+"/v1/auth/keys", strings.NewReader(string(payload)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("key creation failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		// stdout is the created-key record and NOTHING else — the plaintext is
		// its `key` field, the metadata its `api_key` field — so
		// `caesium auth key create … | jq -r .key` works and
		// `… > key.json` is a usable file. The prose that used to be
		// interleaved into stdout goes to stderr, the same split
		// `caesium receipt get` uses (cmd/receipt/get.go).
		//
		// Two bugs lived here: cobra's Print* helpers write to OutOrStderr, so
		// the ONE-TIME plaintext key went to stderr and `> key.txt` produced an
		// empty file; and the labels made stdout unparseable even once that was
		// corrected.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"API Key issued — the plaintext is the `key` field on stdout and will not be shown again.")
		return cliutil.WritePrettyJSON(cmd, respBody, "auth key create response")
	},
}

func init() {
	keyCreateCmd.Flags().StringVar(&createRole, "role", "", "Key role: admin, operator, runner, viewer (required)")
	keyCreateCmd.Flags().StringVar(&createDescription, "description", "", "Human-readable description for the key")
	keyCreateCmd.Flags().StringVar(&createExpiresIn, "expires", "", "Expiration duration (e.g. 90d, 24h)")
	keyCreateCmd.Flags().StringVar(&createServer, "server", "http://localhost:8080", "Caesium server base URL")
	keyCreateCmd.Flags().StringVar(&createAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))
	keyCreateCmd.Flags().StringSliceVar(&createScopeJobs, "scope-jobs", nil, "Restrict key to specific job aliases (comma-separated)")
	_ = keyCreateCmd.MarkFlagRequired("role")

	keyCmd.AddCommand(keyCreateCmd)
}
