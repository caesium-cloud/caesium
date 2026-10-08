package auth

import (
	"fmt"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

const apiKeyEnvVar = cliutil.APIKeyEnvVar

// Cmd is the parent command for authentication operations.
var Cmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage API authentication and authorization",
}

// key is the parent command for API key operations.
var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage API keys",
}

func init() {
	Cmd.AddCommand(keyCmd)
}

func resolveAPIKey(cmd *cobra.Command, flagValue string) string {
	return cliutil.ResolveAPIKey(cmd, flagValue, cliutil.APIKeyEnvVar)
}

func apiKeyFlagUsage(role string) string {
	return fmt.Sprintf("%s API key for authentication (prefer %s; --api-key is visible in process listings)", role, apiKeyEnvVar)
}
