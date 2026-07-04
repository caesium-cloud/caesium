package dataset

import "github.com/spf13/cobra"

var (
	serverFlag    string
	apiKeyFlag    string
	namespaceFlag string
)

// Cmd is the root `caesium dataset` command group.
var Cmd = &cobra.Command{
	Use:   "dataset",
	Short: "Inspect and advance freshness datasets",
}

func init() {
	Cmd.PersistentFlags().StringVar(&serverFlag, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key for authentication (prefer "+apiKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.PersistentFlags().StringVar(&namespaceFlag, "namespace", "", "Dataset namespace (defaults to the empty namespace used in v1)")
	Cmd.AddCommand(listCmd, statusCmd, advanceCmd)
}
