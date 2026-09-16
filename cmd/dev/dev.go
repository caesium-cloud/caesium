// Package dev implements the caesium dev command for local DAG development.
package dev

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

var (
	devPaths    []string
	taskTimeout time.Duration
	runTimeout  time.Duration
	maxParallel int
	runOnce     bool
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

	// Cancel any in-flight run — and stop/remove the container(s) it owns —
	// on SIGINT/SIGTERM. This is established BEFORE the very first run (not
	// just before the watch loop) so `--once`, which has no watch loop and
	// therefore no other chance to observe an interrupt, gets the same
	// graceful-cancellation behaviour watch mode's re-run loop already had.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initial run.
	if err := executeRun(ctx, w, paths); err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(w, "\nInterrupted, stopping...")
		}
		_, _ = fmt.Fprintf(w, "Run failed: %v\n", err)
		if runOnce {
			return err
		}
	}

	if runOnce {
		return nil
	}

	// Watch mode.
	yamlFiles, err := jobdef.ResolveYAMLFiles(paths)
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
	defer func() { _ = watcher.Close() }()

	// Recursively watch every directory under the requested paths — not just
	// ones that happen to contain YAML at startup — so a subdirectory
	// created later (and files saved into it) is seen. A path naming a
	// single file gets its containing directory watched, matching the
	// single-file behaviour this replaces.
	watchedDirs := make(map[string]struct{})
	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			return fmt.Errorf("stat %s: %w", p, statErr)
		}
		root := p
		if !info.IsDir() {
			root = filepath.Dir(p)
		}
		// filepath.WalkDir Lstats its root and does not follow a symlink
		// there (only os.Stat, used above, follows it) — so a symlinked
		// directory, or an explicit file's symlinked parent, would silently
		// get zero watches. Resolve to the real path first.
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", root, err)
		}
		if _, err := addRecursiveWatch(watcher, resolvedRoot, watchedDirs); err != nil {
			return fmt.Errorf("watch %s: %w", resolvedRoot, err)
		}
	}

	_, _ = fmt.Fprintf(w, "\nWatching %d file(s) for changes... (Ctrl-C to stop)\n", len(yamlFiles))

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}

	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(w, "\nStopping.")
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					// A new (or recreated) subdirectory: watch it — and any
					// subdirectories already nested inside it — then rescan
					// the tree for YAML written before the watch was
					// established (the create-dir-then-write-file race).
					foundYAML, err := addRecursiveWatch(watcher, event.Name, watchedDirs)
					if err != nil {
						_, _ = fmt.Fprintf(w, "Watch error: %v\n", err)
					}
					if foundYAML {
						debounce.Reset(200 * time.Millisecond)
					}
					continue
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// Drop the whole former subtree, not just event.Name: moving
				// or removing a directory generates exactly one event, for
				// that directory itself — a descendant watched under its own
				// path (from the initial recursive walk) gets no event of
				// its own when an ancestor moves. Leaving a descendant's old
				// path in watchedDirs would make a later recreation at that
				// same path look "already watched" and skip re-adding a real
				// watch for its new inode. If the same tree is recreated
				// later, the Create branch above re-adds it from scratch
				// (the parent directory's watch reports that Create
				// regardless).
				removeWatchedSubtree(watcher, event.Name, watchedDirs)
			}
			if !jobdef.IsYAML(event.Name) {
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
			_, _ = fmt.Fprintf(w, "Watch error: %v\n", err)
		case <-debounce.C:
			_, _ = fmt.Fprintf(w, "\nFile changed, re-running...\n\n")
			if err := executeRun(ctx, w, paths); err != nil {
				_, _ = fmt.Fprintf(w, "Run failed: %v\n", err)
			}
		}
	}
}

// addRecursiveWatch adds an fsnotify watch for dir and every subdirectory
// nested beneath it, recording each newly watched directory in watchedDirs
// (already-watched directories are skipped). It reports whether any YAML
// file was found during the walk, which callers use to detect a file that
// landed in a brand new directory before its watch was established.
func addRecursiveWatch(watcher *fsnotify.Watcher, dir string, watchedDirs map[string]struct{}) (foundYAML bool, err error) {
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// The path may have been removed mid-walk (e.g. a transient
			// temp directory or a racing delete) — skip it rather than
			// failing the whole scan.
			return nil
		}
		if d.IsDir() {
			if _, ok := watchedDirs[path]; ok {
				return nil
			}
			if err := watcher.Add(path); err != nil {
				return err
			}
			watchedDirs[path] = struct{}{}
			return nil
		}
		if jobdef.IsYAML(path) {
			foundYAML = true
		}
		return nil
	})
	return foundYAML, err
}

// removeWatchedSubtree drops root and every directory tracked in watchedDirs
// underneath it (its whole former subtree), best-effort removing the
// underlying fsnotify watch for each. A single Remove/Rename event only
// names the directory that moved or was removed — not any descendants
// tracked under their own paths from an earlier recursive walk — so without
// this a descendant's stale entry survives and a later recreation at that
// same path is wrongly treated as already watched.
func removeWatchedSubtree(watcher *fsnotify.Watcher, root string, watchedDirs map[string]struct{}) {
	prefix := root + string(filepath.Separator)
	for dir := range watchedDirs {
		if dir == root || strings.HasPrefix(dir, prefix) {
			_ = watcher.Remove(dir)
			delete(watchedDirs, dir)
		}
	}
}

func executeRun(ctx context.Context, w io.Writer, paths []string) error {
	defs, err := jobdef.CollectDefinitions(paths, true)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		return fmt.Errorf("no job definitions selected")
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
			_, _ = fmt.Fprintf(w, "  FAIL  %s: %v\n", def.Metadata.Alias, err)
			return err
		}
		_, _ = fmt.Fprintf(w, "  OK    %s completed successfully\n", def.Metadata.Alias)
	}
	return nil
}
