// Package dev implements the caesium dev command for local DAG development.
package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/internal/localrun"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	devPaths     []string
	taskTimeout  time.Duration
	runTimeout   time.Duration
	maxParallel  int
	runOnce      bool
)

// Cmd is the top-level dev command.
var Cmd = &cobra.Command{
	Use:   "dev",
	Short: "Watch YAML files and run DAGs locally on change",
	Long:  "Parses job definitions, executes the DAG via the local container runtime, and re-runs on file changes.",
	RunE:  runDev,
}

func init() {
	Cmd.Flags().StringSliceVarP(&devPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	Cmd.Flags().DurationVar(&taskTimeout, "task-timeout", 0, "Per-task timeout (e.g. 5m)")
	Cmd.Flags().DurationVar(&runTimeout, "run-timeout", 0, "Total run timeout (e.g. 30m)")
	Cmd.Flags().IntVar(&maxParallel, "max-parallel", 0, "Maximum parallel tasks (default: CPU count)")
	Cmd.Flags().BoolVar(&runOnce, "once", false, "Run once and exit (no file watching)")
}

func runDev(cmd *cobra.Command, _ []string) error {
	paths := devPaths
	if len(paths) == 0 {
		paths = []string{"."}
	}

	w := cmd.OutOrStdout()

	// Initial run.
	if err := executeRun(cmd.Context(), w, paths); err != nil {
		fmt.Fprintf(w, "Run failed: %v\n", err)
		if runOnce {
			return err
		}
	}

	if runOnce {
		return nil
	}

	// Watch mode.
	yamlFiles, err := resolveYAMLFiles(paths)
	if err != nil {
		return err
	}
	if len(yamlFiles) == 0 {
		return fmt.Errorf("no YAML files found to watch")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	defer watcher.Close()

	// Watch directories containing YAML files (to catch new files too).
	watchedDirs := make(map[string]struct{})
	for _, f := range yamlFiles {
		dir := filepath.Dir(f)
		if _, ok := watchedDirs[dir]; ok {
			continue
		}
		watchedDirs[dir] = struct{}{}
		if err := watcher.Add(dir); err != nil {
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}

	fmt.Fprintf(w, "\nWatching %d file(s) for changes... (Ctrl-C to stop)\n", len(yamlFiles))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(w, "\nStopping.")
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !isYAML(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			debounce.Reset(200 * time.Millisecond)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(w, "Watch error: %v\n", err)
		case <-debounce.C:
			fmt.Fprintf(w, "\nFile changed, re-running...\n\n")
			if err := executeRun(ctx, w, paths); err != nil {
				fmt.Fprintf(w, "Run failed: %v\n", err)
			}
		}
	}
}

func executeRun(ctx context.Context, w io.Writer, paths []string) error {
	defs, err := collectDefinitions(paths)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		fmt.Fprintln(w, "No job definitions found.")
		return nil
	}

	runner := localrun.New(localrun.Config{
		MaxParallel: maxParallel,
		TaskTimeout: taskTimeout,
		RunTimeout:  runTimeout,
	})

	for i := range defs {
		def := &defs[i]
		display := &localrun.Display{Writer: w}
		display.RenderHeader(def.Metadata.Alias, strings.Join(paths, ", "))

		if err := runner.Run(ctx, def); err != nil {
			fmt.Fprintf(w, "  FAIL  %s: %v\n", def.Metadata.Alias, err)
			return err
		}
		fmt.Fprintf(w, "  OK    %s completed successfully\n", def.Metadata.Alias)
	}
	return nil
}

func resolveYAMLFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			_ = filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !d.IsDir() && isYAML(path) {
					files = append(files, path)
				}
				return nil
			})
		} else if isYAML(p) {
			files = append(files, p)
		}
	}
	return files, nil
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
		if err := def.Validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
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
