package cache

import (
	"net/http"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for cache operations.
var Cmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage task cache",
}

// apiKeyFlag is the --api-key flag every cache subcommand inherits; the
// credential itself comes from cliutil.ResolveAPIKey (flag, then
// CAESIUM_API_KEY, then the stored `caesium login` credential).
var apiKeyFlag string

func init() {
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", cliutil.APIKeyFlagUsage)
}

// authHeaders returns request headers carrying the resolved credential.
func authHeaders(cmd *cobra.Command) http.Header {
	headers := make(http.Header)
	cliutil.SetAuthorization(cmd, headers, apiKeyFlag)
	return headers
}
