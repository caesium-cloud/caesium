package auth

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	revokeID     string
	revokeServer string
	revokeAPIKey string
)

var keyRevokeCmd = &cobra.Command{
	Use:     "revoke",
	Short:   "Revoke an API key",
	Example: `  caesium auth key revoke --id <key-id>`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(revokeServer, "/")
		apiKey := resolveAPIKey(cmd, revokeAPIKey)
		url := fmt.Sprintf("%s/v1/auth/keys/%s/revoke", server, revokeID)

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("key revocation failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
