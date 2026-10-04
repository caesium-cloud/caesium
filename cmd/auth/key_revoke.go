package auth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	revokeID     string
	revokeServer string
	revokeAPIKey string
)

var keyRevokeCmd = &cobra.Command{
	Use:     "revoke",
	Args:    cobra.NoArgs,
	Short:   "Revoke an API key",
	Example: `  caesium auth key revoke --id <key-id>`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(revokeServer, "/")
		apiKey := resolveAPIKey(cmd, revokeAPIKey)
		url := fmt.Sprintf("%s/v1/auth/keys/%s/revoke", server, revokeID)

		headers := make(http.Header)
		if apiKey != "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPost, url, nil, headers)
		if status == 0 && readErr != nil {
			return readErr
		}
		if status >= http.StatusBadRequest {
			if readErr != nil {
				return fmt.Errorf("key revocation failed (%d): %s (reading response: %w)", status, strings.TrimSpace(string(body)), readErr)
			}
			return fmt.Errorf("key revocation failed (%d): %s", status, strings.TrimSpace(string(body)))
		}
		if readErr != nil {
			return fmt.Errorf("reading key revocation response: %w", readErr)
		}

		// Machine-readable result → stdout (cobra's Print* goes to stderr).
		return cliutil.WritePrettyJSON(cmd, body, "auth key revoke response")
	},
}

func init() {
	keyRevokeCmd.Flags().StringVar(&revokeID, "id", "", "API key ID to revoke (required)")
	keyRevokeCmd.Flags().StringVar(&revokeServer, "server", "http://localhost:8080", "Caesium server base URL")
	keyRevokeCmd.Flags().StringVar(&revokeAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))
	_ = keyRevokeCmd.MarkFlagRequired("id")

	keyCmd.AddCommand(keyRevokeCmd)
}
