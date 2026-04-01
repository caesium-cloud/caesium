package cmd

import (
	"github.com/caesium-cloud/caesium/cmd/backfill"
	"github.com/caesium-cloud/caesium/cmd/cache"
	"github.com/caesium-cloud/caesium/cmd/dev"
	"github.com/caesium-cloud/caesium/cmd/job"
	"github.com/caesium-cloud/caesium/cmd/run"
	"github.com/caesium-cloud/caesium/cmd/start"
	"github.com/caesium-cloud/caesium/cmd/test"
	"github.com/spf13/cobra"
)

var cmds = []*cobra.Command{
	backfill.Cmd,
	cache.Cmd,
	dev.Cmd,
	job.Cmd,
	run.Cmd,
	start.Cmd,
	test.Cmd,
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
