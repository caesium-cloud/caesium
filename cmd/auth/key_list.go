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
	listServer string
	listAPIKey string
)

var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all API keys",
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(listServer, "/")
		apiKey := resolveAPIKey(cmd, listAPIKey)

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, server+"/v1/auth/keys", nil)
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
			return fmt.Errorf("key list failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
