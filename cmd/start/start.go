package start

import "github.com/spf13/cobra"

const (
	usage   = "start"
	short   = "Start a caesium scheduling instance"
	long    = "This command starts a caesium scheduling instance"
	example = "caesium start"
)

var (
	// Cmd is the start command.
	Cmd = &cobra.Command{
		Use:        usage,
		Short:      short,
		Long:       long,
		Aliases:    []string{"s"},
		SuggestFor: []string{"boot", "up", "run", "begin"},
		Example:    example,
		RunE:       start,
	}
)

func start(cmd *cobra.Command, args []string) error {
	// TODO: implement
	return nil
}
