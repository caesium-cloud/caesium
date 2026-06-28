package cliutil

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func WritePrettyJSON(cmd *cobra.Command, body []byte, label string) error {
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("%s was not valid JSON: %w", label, err)
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("re-encoding %s: %w", label, err)
	}
	_, _ = cmd.OutOrStdout().Write(pretty)
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	return nil
}
