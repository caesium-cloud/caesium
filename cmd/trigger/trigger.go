package trigger

import "github.com/spf13/cobra"

var Cmd = &cobra.Command{
	Use:   "trigger",
	Short: "Inspect and operate on triggers",
}

func init() {
	Cmd.AddCommand(eventsCmd)
}
