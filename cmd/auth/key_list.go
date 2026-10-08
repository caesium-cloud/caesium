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
	listServer string
	listAPIKey string
)

var keyListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List all API keys",
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(listServer, "/")
		apiKey := resolveAPIKey(cmd, listAPIKey)

		headers := make(http.Header)
		if apiKey != "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodGet, server+"/v1/auth/keys", nil, headers)
		if status == 0 && readErr != nil {
			return readErr
		}
		if status >= http.StatusBadRequest {
			if readErr != nil {
				return fmt.Errorf("key list failed (%d): %s (reading response: %w)", status, strings.TrimSpace(string(body)), readErr)
			}
			return fmt.Errorf("key list failed (%d): %s", status, strings.TrimSpace(string(body)))
		}
		if readErr != nil {
			return fmt.Errorf("reading key list response: %w", readErr)
		}

		// Machine-readable listing → stdout, nothing else on it. cobra's Print*
		// helpers write to OutOrStderr, which made `caesium auth key list | jq`
		// read an empty stream.
		return cliutil.WritePrettyJSON(cmd, body, "auth key list response")
	},
}

func init() {
	keyListCmd.Flags().StringVar(&listServer, "server", "http://localhost:8080", "Caesium server base URL")
	keyListCmd.Flags().StringVar(&listAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))

	keyCmd.AddCommand(keyListCmd)
}
