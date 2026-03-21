package backfill

import "github.com/spf13/cobra"

// Cmd is the parent command for backfill operations.
var Cmd = &cobra.Command{
	Use:   "backfill",
	Short: "Manage job backfills",
}
