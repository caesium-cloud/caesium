// Package test implements the caesium test command for dry-run DAG validation.
package test

import (
	"fmt"
	"io"
	"strings"

	"github.com/caesium-cloud/caesium/internal/dag"
	"github.com/caesium-cloud/caesium/internal/harness"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/spf13/cobra"
)

var (
	testPaths     []string
	scenarioPaths []string
	checkImages   bool
	verboseOutput bool
)

// Cmd is the top-level test command.
var Cmd = &cobra.Command{
	Use:   "test",
	Short: "Dry-run validation of job definitions",
	Long:  "Validates YAML schemas, analyses the DAG topology, optionally checks local Docker image availability, and can execute harness scenarios.",
	RunE:  runTest,
}

func init() {
	Cmd.Flags().StringSliceVarP(&testPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	Cmd.Flags().StringSliceVar(&scenarioPaths, "scenario", nil, "Paths to harness scenario files or directories")
	Cmd.Flags().BoolVar(&checkImages, "check-images", false, "Check local Docker image availability")
	Cmd.Flags().BoolVarP(&verboseOutput, "verbose", "v", false, "Show detailed DAG analysis")
}

func runTest(cmd *cobra.Command, _ []string) error {
	if len(scenarioPaths) > 0 {
		return runScenarios(cmd)
	}

	// Don't validate during collection — we validate per-definition below
	// to report errors individually.
	defs, err := jobdef.CollectDefinitions(testPaths, false)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No job definitions found.")
		return err
	}

	w := cmd.OutOrStdout()
	allOK := true

	for i := range defs {
		def := &defs[i]
		if err := def.Validate(); err != nil {
			_, _ = fmt.Fprintf(w, "  FAIL  %s: %v\n", def.Metadata.Alias, err)
			allOK = false
			continue
		}

		analysis, err := dag.Analyze(def)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  FAIL  %s: DAG analysis error: %v\n", def.Metadata.Alias, err)
			allOK = false
			continue
		}

		_, _ = fmt.Fprintf(w, "  PASS  %s\n", def.Metadata.Alias)
		printDAGSummary(w, analysis)

		if verboseOutput {
			printVerbose(w, analysis)
		}
	}

	if checkImages {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Image availability:")
		var allImages []string
		for i := range defs {
			allImages = append(allImages, dag.UniqueImages(&defs[i])...)
		}
		allImages = dedup(allImages)
		results := imagecheck.Check(cmd.Context(), allImages)
		for _, r := range results {
			switch {
			case r.Error != nil:
				_, _ = fmt.Fprintf(w, "  FAIL  %s  (error: %v)\n", r.Image, r.Error)
				allOK = false
			case r.Available:
				_, _ = fmt.Fprintf(w, "  PASS  %s  (local)\n", r.Image)
			default:
				_, _ = fmt.Fprintf(w, "  MISS  %s  (not found locally)\n", r.Image)
			}
		}
	}

	if !allOK {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

func runScenarios(cmd *cobra.Command) error {
	scenarios, err := harness.CollectScenarios(scenarioPaths)
	if err != nil {
		return err
	}
	if len(scenarios) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No harness scenarios found.")
		return err
	}

	w := cmd.OutOrStdout()
	allOK := true

	for _, scenario := range scenarios {
		result, err := harness.Execute(cmd.Context(), scenario)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  FAIL  %s: %v\n", scenario.Scenario.Name, err)
			allOK = false
			continue
		}

		if result.Passed() {
			_, _ = fmt.Fprintf(w, "  PASS  %s\n", scenario.Scenario.Name)
			if verboseOutput {
				_, _ = fmt.Fprintf(w, "         Run: %s (%s)\n", result.Run.Alias, result.Run.Status)
				for _, task := range result.Run.Tasks {
					_, _ = fmt.Fprintf(w, "         - %s: %s\n", task.Name, task.Status)
				}
			}
			continue
		}

		allOK = false
		_, _ = fmt.Fprintf(w, "  FAIL  %s\n", scenario.Scenario.Name)
		for _, failure := range result.Failures {
			_, _ = fmt.Fprintf(w, "         - %s\n", failure)
		}
	}

	if !allOK {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

func printDAGSummary(w io.Writer, a *dag.Analysis) {
	_, _ = fmt.Fprintf(w, "         Steps: %s (%d steps, max parallelism: %d)\n",
		strings.Join(formatExecutionOrder(a.ExecutionOrder), " -> "), len(a.Steps), a.MaxParallelism)
}

func printVerbose(w io.Writer, a *dag.Analysis) {
	_, _ = fmt.Fprintf(w, "         Roots: %s\n", strings.Join(a.RootSteps, ", "))
	_, _ = fmt.Fprintf(w, "         Leaves: %s\n", strings.Join(a.LeafSteps, ", "))
	for _, s := range a.Steps {
		deps := "(root)"
		if len(s.DependsOn) > 0 {
			deps = strings.Join(s.DependsOn, ", ")
		}
		_, _ = fmt.Fprintf(w, "         - %s [%s] depends on: %s\n", s.Name, s.Engine, deps)
	}
}

func formatExecutionOrder(layers [][]string) []string {
	result := make([]string, len(layers))
	for i, layer := range layers {
		if len(layer) == 1 {
			result[i] = layer[0]
		} else {
			result[i] = "[" + strings.Join(layer, ", ") + "]"
		}
	}
	return result
}

func dedup(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	var out []string
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
