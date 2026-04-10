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
	rotateID          string
	rotateGracePeriod string
	rotateServer      string
	rotateAPIKey      string
)

var keyRotateCmd = &cobra.Command{
	Use:     "rotate",
	Short:   "Rotate an API key with a grace period for the old key",
	Example: `  caesium auth key rotate --id <key-id> --grace-period 24h`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(rotateServer, "/")
		apiKey := resolveAPIKey(cmd, rotateAPIKey)
		url := fmt.Sprintf("%s/v1/auth/keys/%s/rotate", server, rotateID)

		body := map[string]interface{}{}
		if rotateGracePeriod != "" {
			body["grace_period"] = rotateGracePeriod
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, strings.NewReader(string(payload)))
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
			return fmt.Errorf("key rotation failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		var result map[string]interface{}
		if err := json.Unmarshal(respBody, &result); err != nil {
			cmd.Print(string(respBody))
			return nil
		}

		if key, ok := result["key"].(string); ok {
			cmd.Println("New API Key (save this — it will not be shown again):")
			cmd.Println(key)
			cmd.Println()
		}

		if apiKey, ok := result["api_key"].(map[string]interface{}); ok {
			pretty, _ := json.MarshalIndent(apiKey, "", "  ")
			cmd.Println("New key metadata:")
			cmd.Println(string(pretty))
		}

		return nil
	},
}

func init() {
	keyRotateCmd.Flags().StringVar(&rotateID, "id", "", "API key ID to rotate (required)")
	keyRotateCmd.Flags().StringVar(&rotateGracePeriod, "grace-period", "24h", "Grace period before the old key expires")
	keyRotateCmd.Flags().StringVar(&rotateServer, "server", "http://localhost:8080", "Caesium server base URL")
	keyRotateCmd.Flags().StringVar(&rotateAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))
	_ = keyRotateCmd.MarkFlagRequired("id")

	keyCmd.AddCommand(keyRotateCmd)
}
