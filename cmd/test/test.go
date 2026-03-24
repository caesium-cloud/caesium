// Package test implements the caesium test command for dry-run DAG validation.
package test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caesium-cloud/caesium/internal/daganalysis"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	testPaths      []string
	checkImages    bool
	verboseOutput  bool
)

// Cmd is the top-level test command.
var Cmd = &cobra.Command{
	Use:   "test",
	Short: "Dry-run validation of job definitions",
	Long:  "Validates YAML schemas, analyses the DAG topology, and optionally checks local Docker image availability.",
	RunE:  runTest,
}

func init() {
	Cmd.Flags().StringSliceVarP(&testPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	Cmd.Flags().BoolVar(&checkImages, "check-images", false, "Check local Docker image availability")
	Cmd.Flags().BoolVarP(&verboseOutput, "verbose", "v", false, "Show detailed DAG analysis")
}

func runTest(cmd *cobra.Command, _ []string) error {
	defs, err := collectDefinitions(testPaths)
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
			fmt.Fprintf(w, "  FAIL  %s: %v\n", def.Metadata.Alias, err)
			allOK = false
			continue
		}

		analysis, err := daganalysis.Analyze(def)
		if err != nil {
			fmt.Fprintf(w, "  FAIL  %s: DAG analysis error: %v\n", def.Metadata.Alias, err)
			allOK = false
			continue
		}

		fmt.Fprintf(w, "  PASS  %s\n", def.Metadata.Alias)
		printDAGSummary(w, analysis)

		if verboseOutput {
			printVerbose(w, analysis)
		}
	}

	if checkImages {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Image availability:")
		var allImages []string
		for i := range defs {
			allImages = append(allImages, daganalysis.UniqueImages(&defs[i])...)
		}
		allImages = dedup(allImages)
		results := imagecheck.Check(cmd.Context(), allImages)
		for _, r := range results {
			if r.Error != nil {
				fmt.Fprintf(w, "  ?     %s  (error: %v)\n", r.Image, r.Error)
			} else if r.Available {
				fmt.Fprintf(w, "  PASS  %s  (local)\n", r.Image)
			} else {
				fmt.Fprintf(w, "  MISS  %s  (not found locally)\n", r.Image)
			}
		}
	}

	if !allOK {
		return fmt.Errorf("one or more definitions failed validation")
	}
	return nil
}

func printDAGSummary(w io.Writer, a *daganalysis.DAGAnalysis) {
	names := make([]string, len(a.Steps))
	for i, s := range a.Steps {
		names[i] = s.Name
	}
	fmt.Fprintf(w, "         Steps: %s (%d steps, max parallelism: %d)\n",
		strings.Join(formatExecutionOrder(a.ExecutionOrder), " -> "), len(a.Steps), a.MaxParallelism)
}

func printVerbose(w io.Writer, a *daganalysis.DAGAnalysis) {
	fmt.Fprintf(w, "         Roots: %s\n", strings.Join(a.RootSteps, ", "))
	fmt.Fprintf(w, "         Leaves: %s\n", strings.Join(a.LeafSteps, ", "))
	for _, s := range a.Steps {
		deps := "(root)"
		if len(s.DependsOn) > 0 {
			deps = strings.Join(s.DependsOn, ", ")
		}
		fmt.Fprintf(w, "         - %s [%s] depends on: %s\n", s.Name, s.Engine, deps)
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

// collectDefinitions mirrors the pattern from cmd/job/apply.go.
func collectDefinitions(paths []string) ([]schema.Definition, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var defs []schema.Definition
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if err := filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() || !isYAML(path) {
					return nil
				}
				return appendDefinitions(path, &defs)
			}); err != nil {
				return nil, err
			}
		} else {
			if !isYAML(p) {
				return nil, fmt.Errorf("%s is not a YAML file", p)
			}
			if err := appendDefinitions(p, &defs); err != nil {
				return nil, err
			}
		}
	}
	return defs, nil
}

func appendDefinitions(path string, defs *[]schema.Definition) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var def schema.Definition
		if err := dec.Decode(&def); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if isBlankDefinition(&def) {
			continue
		}
		// Don't validate here — we validate in runTest to report per-definition.
		*defs = append(*defs, def)
	}
	return nil
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func isBlankDefinition(def *schema.Definition) bool {
	if def == nil {
		return true
	}
	if strings.TrimSpace(def.Metadata.Alias) != "" {
		return false
	}
	if def.APIVersion != "" || def.Kind != "" {
		return false
	}
	if def.Trigger.Type != "" || len(def.Steps) > 0 || len(def.Callbacks) > 0 {
		return false
	}
	return true
}
