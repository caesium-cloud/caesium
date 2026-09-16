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

	// Resolve every DIRECTORY path to its real, symlink-free form ONCE, up
	// front, before discovery or watching ever sees it. jobdef.CollectDefinitions
	// and jobdef.ResolveYAMLFiles both os.Stat then filepath.WalkDir a
	// directory path, and WalkDir Lstats its root without following a
	// symlink there — so a symlinked directory (e.g. `--path jobs` with
	// `jobs` a symlink) silently discovered zero definitions on the very
	// first run, before watch setup's own (now redundant) resolution ever
	// ran. Resolving here, into the SAME paths slice executeRun (every run,
	// not just the first) and watch setup both read, fixes discovery and
	// watching identically and keeps them from ever disagreeing.
	//
	// An explicit FILE argument is deliberately left unresolved: os.ReadFile
	// (what CollectDefinitions and watcher.Add both rely on) already follows
	// symlinks in every path component transparently, so nothing was ever
	// broken there. Resolving it anyway would permanently pin whatever it
	// happened to point to at startup — an atomic repoint or replacement of
	// that exact file (e.g. `current.yaml -> templates/v1.yaml`, later
	// repointed to v2) would then produce no rerun, ever, and a selected
	// .yaml symlink whose target lacks a YAML extension would be rejected
	// outright even though the SELECTED path is perfectly valid.
	resolvedPaths, err := resolveSymlinks(paths)
	if err != nil {
		return err
	}
	paths = resolvedPaths

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

	// A directory --path argument is a RECURSIVE root: the whole subtree is
	// watched now and dynamically extended as new subdirectories appear
	// later (#515). An explicit FILE argument gets only its containing
	// directory watched, NON-recursively — matching the pre-#515 behaviour
	// exactly. A step's own run can write output files into a subdirectory
	// of that same directory; recursively watching (and dynamically
	// expanding into) that subtree would let the job's own output write
	// retrigger the run that produced it, forever. setupWatches keeps that
	// distinction and the event loop's Create handling below respects it.
	watchedDirs, err := setupWatches(watcher, paths)
	if err != nil {
		return err
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
					// A new (or recreated) subdirectory. handleDirectoryCreated
					// only expands it (and rescans for YAML written before the
					// watch was established — the create-dir-then-write-file
					// race) when it lands inside a RECURSIVELY managed tree; a
					// directory appearing under a lone file's non-recursive
					// parent watch is intentionally left unwatched.
					foundYAML, err := handleDirectoryCreated(watcher, event.Name, watchedDirs)
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

// resolveSymlinks resolves every DIRECTORY path to its real, symlink-free
// form; an explicit FILE path is left exactly as given. Used once, up front
// in runDev, before either definition discovery or watch setup sees any
// path — see the comment at its call site for why directories are resolved
// but files are not.
func resolveSymlinks(paths []string) ([]string, error) {
	resolved := make([]string, len(paths))
	for i, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		if !info.IsDir() {
			resolved[i] = p
			continue
		}
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		resolved[i] = filepath.Clean(r)
	}
	return resolved, nil
}

// setupWatches installs the initial fsnotify watches for paths (already
// symlink-resolved by the caller) and returns the tracking map the event
// loop's dynamic directory-creation handling (handleDirectoryCreated) reads.
// watchedDirs maps every watched directory to whether it is part of a
// RECURSIVELY managed tree: true for a directory --path argument (and
// everything addRecursiveWatch finds beneath it, including subdirectories
// discovered later), false for the single, non-recursive watch installed on
// an explicit file argument's containing directory. Directory arguments are
// processed first so that, if a file argument names a path already covered
// by one, the recursive tag always wins.
func setupWatches(watcher *fsnotify.Watcher, paths []string) (map[string]bool, error) {
	watchedDirs := make(map[string]bool)

	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			return nil, fmt.Errorf("stat %s: %w", p, statErr)
		}
		if !info.IsDir() {
			continue
		}
		if _, err := addRecursiveWatch(watcher, p, watchedDirs); err != nil {
			return nil, fmt.Errorf("watch %s: %w", p, err)
		}
	}

	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			return nil, fmt.Errorf("stat %s: %w", p, statErr)
		}
		if info.IsDir() {
			continue
		}
		parentDir := filepath.Dir(p)
		if _, ok := watchedDirs[parentDir]; ok {
			continue
		}
		if err := watcher.Add(parentDir); err != nil {
			return nil, fmt.Errorf("watch %s: %w", parentDir, err)
		}
		watchedDirs[parentDir] = false
	}

	return watchedDirs, nil
}

// handleDirectoryCreated processes a Create event whose target, newDir, is a
// directory. It only recursively watches (and rescans for YAML written
// before the watch was established — the create-dir-then-write-file race)
// when newDir's parent is itself part of a RECURSIVELY managed tree; a
// directory appearing inside a lone file's non-recursive parent watch (see
// setupWatches) is left untouched on purpose — expanding it would let a
// step's own output write into a newly created subdirectory retrigger the
// run that produced it, forever.
//
// newDir is normalized with filepath.Clean before the lookup: on Linux,
// fsnotify's inotify backend builds an event's Name by concatenating the
// watch's OWN (filepath.Clean'd at Add-time) path with the raw entry name —
// not filepath.Join — so a watch on "." (the default root) reports a newly
// created entry as "./new", while THIS package's own watchedDirs entries are
// always recorded under their clean form ("new"). Without normalizing here,
// the lookup for "./new"'s parent misses the "." entry recorded at startup.
func handleDirectoryCreated(watcher *fsnotify.Watcher, newDir string, watchedDirs map[string]bool) (foundYAML bool, err error) {
	newDir = filepath.Clean(newDir)
	if !watchedDirs[filepath.Dir(newDir)] {
		return false, nil
	}
	return addRecursiveWatch(watcher, newDir, watchedDirs)
}

// addRecursiveWatch adds an fsnotify watch for dir and every subdirectory
// nested beneath it, recording each newly watched directory in watchedDirs
// as part of a recursively managed tree (already-watched directories are
// skipped). It reports whether any YAML file was found during the walk,
// which callers use to detect a file that landed in a brand new directory
// before its watch was established.
//
// dir is normalized with filepath.Clean before the walk: filepath.WalkDir
// reports the walk ROOT under exactly the string it was given (descendants
// go through filepath.Join, which always cleans), and fsnotify.Add cleans
// whatever path it is given before storing it internally — so an uncleaned
// root here (e.g. "./new") would be tracked and watched under a different
// spelling than what fsnotify (and this function, called again for a
// descendant) will use later, breaking the watchedDirs lookups that gate
// dynamic directory discovery.
func addRecursiveWatch(watcher *fsnotify.Watcher, dir string, watchedDirs map[string]bool) (foundYAML bool, err error) {
	dir = filepath.Clean(dir)
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
			watchedDirs[path] = true
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
// same path is wrongly treated as already watched. root is normalized with
// filepath.Clean for the same reason addRecursiveWatch and
// handleDirectoryCreated normalize their path arguments — see their
// comments.
func removeWatchedSubtree(watcher *fsnotify.Watcher, root string, watchedDirs map[string]bool) {
	root = filepath.Clean(root)
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
