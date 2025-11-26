package cmd

import (
	"github.com/caesium-cloud/caesium/cmd/console"
	"github.com/caesium-cloud/caesium/cmd/job"
	runcmd "github.com/caesium-cloud/caesium/cmd/run"
	"github.com/caesium-cloud/caesium/cmd/start"
	"github.com/spf13/cobra"
)

var cmds = []*cobra.Command{
	start.Cmd,
	console.Cmd,
	job.Cmd,
	runcmd.Cmd,
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
