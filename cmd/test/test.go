// Package test implements the caesium test command for dry-run DAG validation.
package test

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/dag"
	"github.com/caesium-cloud/caesium/internal/harness"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
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
	Long:  "Validates YAML schemas, analyses the DAG topology, optionally gates on local Docker image availability, and can execute harness scenarios.",
	RunE:  runTest,
}

func init() {
	Cmd.Flags().StringSliceVarP(&testPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	Cmd.Flags().StringSliceVar(&scenarioPaths, "scenario", nil, "Paths to harness scenario files or directories")
	Cmd.Flags().BoolVar(&checkImages, "check-images", false, "Require every referenced image in the local Docker daemon (no pull or remote-runtime check)")
	Cmd.Flags().BoolVarP(&verboseOutput, "verbose", "v", false, "Show detailed DAG analysis")
}

func runTest(cmd *cobra.Command, _ []string) error {
	if len(scenarioPaths) > 0 {
		if checkImages {
			return fmt.Errorf("--check-images requires job definitions and cannot be used with --scenario")
		}
		return runScenarios(cmd)
	}

	// Don't validate during collection — we validate per-definition below
	// to report errors individually.
	defs, err := jobdef.CollectDefinitions(testPaths, false)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		return fmt.Errorf("no job definitions selected")
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
		_, _ = fmt.Fprintln(w, "Image availability (local Docker daemon only; no registry pull is attempted):")
		targets := imageCheckTargets(defs)
		images := make([]string, len(targets))
		for i := range targets {
			images[i] = targets[i].image
		}
		results := imagecheck.Check(cmd.Context(), images)
		for i, r := range results {
			scope := targets[i].scope()
			switch {
			case r.Error != nil:
				_, _ = fmt.Fprintf(w, "  FAIL  %s  (%s; error: %v)\n", r.Image, scope, r.Error)
				allOK = false
			case r.Available:
				_, _ = fmt.Fprintf(w, "  PASS  %s  (%s)\n", r.Image, scope)
			default:
				_, _ = fmt.Fprintf(w, "  MISS  %s  (not found in local Docker daemon; %s)\n", r.Image, scope)
				allOK = false
			}
		}
	}

	if !allOK {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

type imageCheckTarget struct {
	image   string
	engines []string
}

// imageCheckTargets keeps the definition order while recording where an image
// is used. --check-images deliberately asks only the local Docker daemon, so
// an image referenced by another engine must retain that caveat in its result.
func imageCheckTargets(defs []schema.Definition) []imageCheckTarget {
	index := make(map[string]int)
	var targets []imageCheckTarget
	for i := range defs {
		for _, step := range defs[i].Steps {
			image := strings.TrimSpace(step.Image)
			if image == "" {
				continue
			}
			engine := strings.TrimSpace(step.Engine)
			if engine == "" {
				engine = schema.EngineDocker
			}
			if targetIndex, ok := index[image]; ok {
				if !slices.Contains(targets[targetIndex].engines, engine) {
					targets[targetIndex].engines = append(targets[targetIndex].engines, engine)
				}
				continue
			}
			index[image] = len(targets)
			targets = append(targets, imageCheckTarget{image: image, engines: []string{engine}})
		}
	}
	return targets
}

func (t imageCheckTarget) scope() string {
	scope := "local Docker daemon"
	var caveats []string
	if slices.Contains(t.engines, schema.EnginePodman) {
		caveats = append(caveats, "Podman runtime availability is not checked")
	}
	if slices.Contains(t.engines, schema.EngineKubernetes) {
		caveats = append(caveats, "Kubernetes target availability is not checked")
	}
	if len(caveats) == 0 {
		return scope
	}
	return scope + "; " + strings.Join(caveats, "; ")
}

func runScenarios(cmd *cobra.Command) error {
	scenarios, err := harness.CollectScenarios(scenarioPaths)
	if err != nil {
		return err
	}
	if len(scenarios) == 0 {
		return fmt.Errorf("no harness scenarios selected")
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
