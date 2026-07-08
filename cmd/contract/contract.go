package contract

import (
	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	serverFlag string
	apiKeyFlag string
)

// Cmd is the root `caesium contract` command group.
var Cmd = &cobra.Command{
	Use:   "contract",
	Short: "Inspect contract enforcement graph and findings",
}

func init() {
	Cmd.PersistentFlags().StringVar(&serverFlag, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.AddCommand(graphCmd, checkCmd)
}
