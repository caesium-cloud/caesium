package job

import (
	"fmt"

	"github.com/caesium-cloud/caesium/internal/daganalysis"
	"github.com/caesium-cloud/caesium/internal/dagrender"
	"github.com/spf13/cobra"
)

var previewPaths []string

var previewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Render DAG visualization in the terminal",
	RunE: func(cmd *cobra.Command, _ []string) error {
		defs, err := collectDefinitions(previewPaths)
		if err != nil {
			return err
		}
		if len(defs) == 0 {
			return writeCmdOut(cmd, "No job definitions found.\n")
		}

		w := cmd.OutOrStdout()
		for i := range defs {
			def := &defs[i]
			if err := def.Validate(); err != nil {
				return fmt.Errorf("definition %s: %w", def.Metadata.Alias, err)
			}

			analysis, err := daganalysis.Analyze(def)
			if err != nil {
				return fmt.Errorf("definition %s: %w", def.Metadata.Alias, err)
			}

			if len(defs) > 1 {
				_, _ = fmt.Fprintf(w, "--- %s ---\n", def.Metadata.Alias)
			}

			if err := dagrender.Render(analysis, w); err != nil {
				return err
			}

			if len(defs) > 1 && i < len(defs)-1 {
				_, _ = fmt.Fprintln(w)
			}
		}
		return nil
	},
}

func init() {
	previewCmd.Flags().StringSliceVarP(&previewPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	Cmd.AddCommand(previewCmd)
}
