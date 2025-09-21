package job

import (
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/jobdef/report"
	"github.com/spf13/cobra"
)

type schemaOptions struct {
	paths    []string
	doc      bool
	summary  bool
	markdown bool
}

func newSchemaCommand() *cobra.Command {
	opts := &schemaOptions{}
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Inspect the job definition schema and conformance",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !opts.doc && !opts.summary {
				opts.doc = true
			}

			out := cmd.OutOrStdout()

			if opts.doc {
				if opts.summary {
					fmt.Fprintln(out, "# Schema Overview")
				}
				fmt.Fprintln(out, report.Markdown())
			}

			if opts.summary {
				defs, err := collectDefinitions(opts.paths)
				if err != nil {
					return err
				}
				if len(defs) == 0 {
					fmt.Fprintln(out, "No job definitions found for summary.")
					return nil
				}

				summary := report.Analyze(defs)
				if opts.markdown {
					fmt.Fprintln(out, report.RenderSummaryMarkdown(summary))
					return nil
				}

				fmt.Fprintln(out, renderPlainSummary(summary))
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&opts.paths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	cmd.Flags().BoolVar(&opts.doc, "doc", false, "Print the schema reference (Markdown)")
	cmd.Flags().BoolVar(&opts.summary, "summary", false, "Analyze definitions and output a conformance report")
	cmd.Flags().BoolVar(&opts.markdown, "markdown", false, "Render summary output as Markdown")

	return cmd
}

func init() {
	Cmd.AddCommand(newSchemaCommand())
}

func renderPlainSummary(summary report.Summary) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Total definitions: %d\n", summary.Total))

	if len(summary.MissingAliases) > 0 {
		slices := append([]string(nil), summary.MissingAliases...)
		sort.Strings(slices)
		b.WriteString("Missing aliases:\n")
		for _, entry := range slices {
			b.WriteString(fmt.Sprintf("  - %s\n", entry))
		}
	}

	if len(summary.TriggerTypes) > 0 {
		b.WriteString("Trigger types:\n")
		writePlainCounts(&b, summary.TriggerTypes)
	}

	if len(summary.Engines) > 0 {
		b.WriteString("Step engines:\n")
		writePlainCounts(&b, summary.Engines)
	}

	if len(summary.CallbackTypes) > 0 {
		b.WriteString("Callback types:\n")
		writePlainCounts(&b, summary.CallbackTypes)
	}

	return strings.TrimSpace(b.String())
}

func writePlainCounts(b *strings.Builder, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString(fmt.Sprintf("  - %s: %d\n", key, counts[key]))
	}
}
