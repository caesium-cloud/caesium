// Package receipt is the `caesium receipt` CLI command group: produce a
// content-addressed, git-committable reproducibility receipt for a run. The
// companion `caesium verify` command (cmd/verify) re-derives a committed
// receipt against a run's persisted state and flags drift.
package receipt

import "github.com/spf13/cobra"

// Cmd is the parent command for receipt operations.
var Cmd = &cobra.Command{
	Use:   "receipt",
	Short: "Produce reproducibility receipts for job runs",
	Long: "Produce a content-addressed, git-committable reproducibility receipt " +
		"for a job run. Commit the receipt alongside your pipeline; later, " +
		"`caesium verify <receipt>` re-derives it from the run's persisted state " +
		"and flags drift (e.g. a moved image tag).",
}
