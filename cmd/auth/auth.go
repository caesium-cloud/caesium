package auth

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const apiKeyEnvVar = "CAESIUM_API_KEY"

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
	if strings.TrimSpace(flagValue) != "" {
		cmd.PrintErrln(fmt.Sprintf("warning: --api-key is visible in process listings; prefer %s", apiKeyEnvVar))
		return strings.TrimSpace(flagValue)
	}
	return strings.TrimSpace(os.Getenv(apiKeyEnvVar))
}

func apiKeyFlagUsage(role string) string {
	return fmt.Sprintf("%s API key for authentication (prefer %s; --api-key is visible in process listings)", role, apiKeyEnvVar)
}
