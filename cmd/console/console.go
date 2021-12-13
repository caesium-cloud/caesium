package console

import (
	"fmt"

	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
)

const (
	usage   = "console"
	short   = "Open a console session to query Caesium"
	long    = "This command initiates an interactive console session to query Caesium"
	example = "caesium console"
)

var (
	// Cmd is the start command.
	Cmd = &cobra.Command{
		Use:        usage,
		Short:      short,
		Long:       long,
		Aliases:    []string{"c"},
		SuggestFor: []string{"query", "terminal", "connect", "exec"},
		Example:    example,
		RunE:       console,
	}

	suggestions = []prompt.Suggest{}
)

func executor(in string) {
	fmt.Println("console under construction")
}

func completer(in prompt.Document) []prompt.Suggest {
	w := in.GetWordBeforeCursor()
	if w == "" {
		return []prompt.Suggest{}
	}
	return prompt.FilterHasPrefix(suggestions, w, true)
}

func console(cmd *cobra.Command, args []string) error {
	prompt.New(
		executor,
		completer,
		prompt.OptionPrefix("caesium> "),
		prompt.OptionTitle("caesium"),
	).Run()

	return nil
}
