package run

import "github.com/spf13/cobra"

// Cmd is the parent command for run operations.
var Cmd = &cobra.Command{
	Use:   "run",
	Short: "Manage job runs",
}
