package console

import (
	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/app"
	"github.com/caesium-cloud/caesium/cmd/console/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

const (
	usage   = "console"
	short   = "Open a console session to inspect Caesium"
	long    = "This command starts the interactive Caesium console"
	example = "caesium console"
)

// Cmd is the Cobra command entrypoint.
var Cmd = &cobra.Command{
	Use:        usage,
	Short:      short,
	Long:       long,
	Aliases:    []string{"c"},
	SuggestFor: []string{"tui", "terminal", "ui"},
	Example:    example,
	RunE:       run,
}

func run(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	client := api.New(cfg)
	model := app.New(client)

	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}
