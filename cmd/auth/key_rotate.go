package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
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
	Args:    cobra.NoArgs,
	Short:   "Rotate an API key with a grace period for the old key",
	Example: `  caesium auth key rotate --id <key-id> --grace-period 24h`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(rotateServer, "/")
		apiKey := resolveAPIKey(cmd, rotateAPIKey)
		url := fmt.Sprintf("%s/v1/auth/keys/%s/rotate", server, rotateID)

		body := map[string]any{}
		if rotateGracePeriod != "" {
			body["grace_period"] = rotateGracePeriod
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}

		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		if apiKey != "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
		respBody, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPost, url, strings.NewReader(string(payload)), headers)
		if status == 0 && readErr != nil {
			return readErr
		}
		if status >= http.StatusBadRequest {
			if readErr != nil {
				return fmt.Errorf("key rotation failed (%d): %s (reading response: %w)", status, strings.TrimSpace(string(respBody)), readErr)
			}
			return fmt.Errorf("key rotation failed (%d): %s", status, strings.TrimSpace(string(respBody)))
		}
		if readErr != nil {
			return fmt.Errorf("reading key rotation response: %w", readErr)
		}

		// See key_create.go: stdout is the new key's record and nothing else,
		// prose on stderr.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"New API Key issued — the plaintext is the `key` field on stdout and will not be shown again.")
		return cliutil.WritePrettyJSON(cmd, respBody, "auth key rotate response")
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
