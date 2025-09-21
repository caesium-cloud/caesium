package job

import "github.com/spf13/cobra"

// Cmd is the parent command for job operations.
var Cmd = &cobra.Command{
	Use:   "job",
	Short: "Manage job definitions",
}

func init() {
	Cmd.AddCommand(diffCmd)
}
