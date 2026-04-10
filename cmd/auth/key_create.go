package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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

		body := map[string]interface{}{
			"role":        createRole,
			"description": createDescription,
		}
		if createExpiresIn != "" {
			body["expires_in"] = createExpiresIn
		}
		if len(createScopeJobs) > 0 {
			body["scope"] = map[string]interface{}{
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

		var result map[string]interface{}
		if err := json.Unmarshal(respBody, &result); err != nil {
			cmd.Print(string(respBody))
			return nil
		}

		if key, ok := result["key"].(string); ok {
			cmd.Println("API Key (save this — it will not be shown again):")
			cmd.Println(key)
			cmd.Println()
		}

		if apiKey, ok := result["api_key"].(map[string]interface{}); ok {
			pretty, _ := json.MarshalIndent(apiKey, "", "  ")
			cmd.Println("Key metadata:")
			cmd.Println(string(pretty))
		}

		return nil
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
