package job

import (
	"fmt"
	"sort"
	"strings"

	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/spf13/cobra"
)

var (
	diffPaths []string
)

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Show changes between job definitions and the database",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		desired, err := jobdiff.LoadDefinitions(diffPaths)
		if err != nil {
			return err
		}

		specs, err := jobdiff.LoadDatabaseSpecs(ctx, db.Connection())
		if err != nil {
			return err
		}

		result := jobdiff.Compare(desired, specs)
		printDiff(cmd, result)
		return nil
	},
}

func init() {
	diffCmd.Flags().StringSliceVarP(&diffPaths, "path", "p", nil, "Paths to job definition files or directories")
}

func printDiff(cmd *cobra.Command, diff jobdiff.Diff) {
	out := cmd.OutOrStdout()

	if diff.Empty() {
		fmt.Fprintln(out, "No changes detected.")
		return
	}

	if len(diff.Creates) > 0 {
		fmt.Fprintln(out, "Creates:")
		sort.Slice(diff.Creates, func(i, j int) bool { return diff.Creates[i].Alias < diff.Creates[j].Alias })
		for _, spec := range diff.Creates {
			fmt.Fprintf(out, "  - %s\n", spec.Alias)
		}
		fmt.Fprintln(out)
	}

	if len(diff.Updates) > 0 {
		fmt.Fprintln(out, "Updates:")
		sort.Slice(diff.Updates, func(i, j int) bool { return diff.Updates[i].Alias < diff.Updates[j].Alias })
		for _, upd := range diff.Updates {
			fmt.Fprintf(out, "  - %s\n", upd.Alias)
			diffText := indent(upd.Diff, "    ")
			fmt.Fprintln(out, diffText)
		}
		fmt.Fprintln(out)
	}

	if len(diff.Deletes) > 0 {
		fmt.Fprintln(out, "Deletes:")
		sort.Slice(diff.Deletes, func(i, j int) bool { return diff.Deletes[i].Alias < diff.Deletes[j].Alias })
		for _, spec := range diff.Deletes {
			fmt.Fprintf(out, "  - %s\n", spec.Alias)
		}
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
