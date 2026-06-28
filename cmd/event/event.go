package event

import "github.com/spf13/cobra"

var Cmd = &cobra.Command{
	Use:   "event",
	Short: "Push and inspect event-trigger events",
}

func init() {
	Cmd.AddCommand(pushCmd)
}
