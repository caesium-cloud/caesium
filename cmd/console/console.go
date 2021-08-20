package console

import (
	"os"

	"github.com/c-bata/go-prompt"
	"github.com/caesium-cloud/caesium/pkg/client"
	"github.com/jedib0t/go-pretty/v6/table"
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
	cli := client.Client()

	resp, err := cli.Query(in)
	if err != nil {
		panic(err)
	}

	results := resp.Results[0]

	header := make(table.Row, len(results.Columns))
	for i := range header {
		header[i] = results.Columns[i]
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(header)

	rows := make([]table.Row, len(results.Values))
	for i := range rows {
		rows[i] = results.Values[i]
	}

	t.AppendRows(rows)
	t.Render()
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
