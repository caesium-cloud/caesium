package dataset

import "github.com/spf13/cobra"

var (
	serverFlag string
	apiKeyFlag string
)

// Cmd is the root `caesium dataset` command group.
var Cmd = &cobra.Command{
	Use:   "dataset",
	Short: "Inspect and advance freshness datasets",
}

func init() {
	Cmd.PersistentFlags().StringVar(&serverFlag, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key for authentication (prefer "+apiKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.AddCommand(listCmd, statusCmd, advanceCmd)
}
