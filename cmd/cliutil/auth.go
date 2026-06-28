package cliutil

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const APIKeyEnvVar = "CAESIUM_API_KEY"

func ResolveAPIKey(cmd *cobra.Command, flagValue, preferredEnvVar string, fallbackEnvVars ...string) string {
	if strings.TrimSpace(flagValue) != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --api-key is visible in process listings; prefer %s\n", preferredEnvVar)
		return strings.TrimSpace(flagValue)
	}

	for _, envVar := range append([]string{preferredEnvVar}, fallbackEnvVars...) {
		if value := strings.TrimSpace(os.Getenv(envVar)); value != "" {
			return value
		}
	}
	return ""
}
