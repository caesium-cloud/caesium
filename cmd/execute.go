package cmd

import (
	"github.com/caesium-cloud/caesium/cmd/agentprofile"
	"github.com/caesium-cloud/caesium/cmd/auth"
	"github.com/caesium-cloud/caesium/cmd/backfill"
	"github.com/caesium-cloud/caesium/cmd/blame"
	"github.com/caesium-cloud/caesium/cmd/cache"
	"github.com/caesium-cloud/caesium/cmd/contract"
	"github.com/caesium-cloud/caesium/cmd/dataset"
	"github.com/caesium-cloud/caesium/cmd/dev"
	"github.com/caesium-cloud/caesium/cmd/event"
	"github.com/caesium-cloud/caesium/cmd/incident"
	"github.com/caesium-cloud/caesium/cmd/job"
	"github.com/caesium-cloud/caesium/cmd/receipt"
	"github.com/caesium-cloud/caesium/cmd/reproduce"
	"github.com/caesium-cloud/caesium/cmd/run"
	"github.com/caesium-cloud/caesium/cmd/start"
	"github.com/caesium-cloud/caesium/cmd/test"
	"github.com/caesium-cloud/caesium/cmd/trigger"
	"github.com/caesium-cloud/caesium/cmd/verify"
	"github.com/caesium-cloud/caesium/cmd/why"
	"github.com/spf13/cobra"
)

var cmds = []*cobra.Command{
	agentprofile.Cmd,
	auth.Cmd,
	backfill.Cmd,
	blame.Cmd,
	cache.Cmd,
	contract.Cmd,
	reproduce.Cmd,
	dataset.Cmd,
	dev.Cmd,
	event.Cmd,
	incident.Cmd,
	job.Cmd,
	receipt.Cmd,
	run.Cmd,
	start.Cmd,
	test.Cmd,
	trigger.Cmd,
	verify.Cmd,
	why.Cmd,
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
