package cmd

import (
	"github.com/caesium-dev/caesium/cmd/start"
	"github.com/spf13/cobra"
)

var cmds = []*cobra.Command{
	start.Cmd,
}

// Execute builds the command tree and executes commands.
func Execute() error {
	command := &cobra.Command{
		Use: "caesium",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Usage()
		},
	}

	for _, c := range cmds {
		command.AddCommand(c)
	}

	return command.Execute()
}
