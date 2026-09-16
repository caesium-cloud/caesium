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
	// roots.paths is the set of directory arguments themselves, each also
	// watched from its OWN parent (roots.sentinelParents) — see
	// setupWatches — so moving one away and recreating it is still
	// observable, without those parent watches reacting to unrelated
	// content that also happens to live there.
	watchedDirs, roots, err := setupWatches(watcher, paths)
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
			eventName, pathErr := absoluteWatchPath(event.Name)
			if pathErr != nil {
				_, _ = fmt.Fprintf(w, "Watch error: %v\n", pathErr)
				continue
			}

			if event.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(eventName); statErr == nil && info.IsDir() {
					cleanName := eventName
					if _, isRoot := roots.paths[cleanName]; isRoot {
						// A selected directory root reappeared under its
						// OWN path (moved away — removeWatchedSubtree
						// dropped it and everything beneath it on the
						// Remove/Rename below — then recreated). Nothing
						// else is watching for this: a root's tree only
						// ever watches itself and its descendants, never
						// its parent's other children. Restore full
						// recursive watching under it.
						foundYAML, err := addRecursiveWatch(watcher, cleanName, watchedDirs, roots.sentinelParents)
						if err != nil {
							_, _ = fmt.Fprintf(w, "Watch error: %v\n", err)
						}
						if foundYAML {
							debounce.Reset(200 * time.Millisecond)
						}
						continue
					}
					// A new (or recreated) NESTED subdirectory.
					// handleDirectoryCreated only expands it (and rescans
					// for YAML written before the watch was established —
					// the create-dir-then-write-file race) when it lands
					// inside a RECURSIVELY managed tree; a directory
					// appearing under a lone file's (or a root's own)
					// non-recursive parent watch is intentionally left
					// unwatched.
					foundYAML, err := handleDirectoryCreated(watcher, eventName, watchedDirs, roots.sentinelParents)
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
				// watch for its new inode. roots.paths is untouched here (on
				// purpose): if event.Name was a selected directory root
				// itself, we still want to recognize its return — see the
				// roots.paths check in the Create branch above, and the
				// parent watch setupWatches installs for exactly this case.
				removeWatchedSubtree(watcher, eventName, watchedDirs)
			}
			if !jobdef.IsYAML(eventName) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			if _, sentinelOnly := roots.sentinelParents[filepath.Dir(eventName)]; sentinelOnly {
				// This directory is watched ONLY to observe a selected
				// directory root reappearing under it (see setupWatches) —
				// nothing else in it was ever explicitly selected. A
				// sibling YAML file changing here (e.g. a step's own
				// output write via a bind mount) must not retrigger a run
				// that has nothing to do with it; the root's own Create is
				// already handled above via roots.paths, before this point
				// is ever reached.
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
		resolved[i], err = canonicalWatchDir(r)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", p, err)
		}
	}
	return resolved, nil
}

// dirRoots is setupWatches' bookkeeping specific to DIRECTORY --path
// arguments. paths is the set of the roots' own (clean) paths, used to
// recognize one reappearing after a move (see the event loop's Create
// handling). sentinelParents is the set of directories watched SOLELY to
// observe a root's own parent for that reappearance — never for their other,
// unrelated contents: the event loop's generic YAML Write/Create handling
// skips a sentinel-only parent's file events other than the root's own
// Create (handled separately via paths), so a sibling file changing in that
// parent — e.g. a step's own output write via a bind mount — cannot
// retrigger a run that has nothing to do with it. A directory stops being
// "sentinel-only" the moment it also has a genuine reason to be watched (an
// explicit file argument's parent, or becoming a recursive root itself) —
// see setupWatches and addRecursiveWatch, which demote/promote it.
type dirRoots struct {
	paths           map[string]struct{}
	sentinelParents map[string]struct{}
}

// setupWatches installs the initial fsnotify watches for paths (already
// symlink-resolved by the caller) and returns the tracking map the event
// loop's dynamic directory-creation handling (handleDirectoryCreated) reads,
// plus dirRoots bookkeeping for DIRECTORY --path arguments (see its doc).
// watchedDirs maps every watched directory to whether it is part of a
// RECURSIVELY managed tree: true for a directory --path argument (and
// everything addRecursiveWatch finds beneath it, including subdirectories
// discovered later), false for a single, non-recursive watch — either an
// explicit file argument's containing directory, or a directory root's OWN
// parent (see dirRoots.sentinelParents). Directory arguments are processed
// first so that, if a file argument names a path already covered by one,
// the recursive tag always wins.
//
// setupWatches also installs a non-recursive watch on each directory root's
// PARENT (unless the root has no meaningful parent, e.g. "/"), so
// that moving the root away and recreating it is observable:
// removeWatchedSubtree drops the root and everything beneath it on the
// Remove/Rename event, and nothing would otherwise be left watching for its
// return — a directory root's own tree only ever watches itself and its
// descendants, never upward. The event loop reacts to a Create whose name is
// EXACTLY one of roots.paths (checked before the generic non-recursive-parent
// gating below) by re-running addRecursiveWatch for that root, restoring
// full recursive watching under it without expanding into unrelated content
// the parent might also contain — the same "filtered to one name" idea the
// single-file case already relies on. If a LATER argument in paths turns out
// to be an explicit file whose parent is one of these sentinel-only
// directories, it is demoted (removed from sentinelParents): that directory
// now has a genuine reason to react to its other contents too.
func setupWatches(watcher *fsnotify.Watcher, paths []string) (watchedDirs map[string]bool, roots dirRoots, err error) {
	watchedDirs = make(map[string]bool)
	roots = dirRoots{
		paths:           make(map[string]struct{}),
		sentinelParents: make(map[string]struct{}),
	}

	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			return nil, dirRoots{}, fmt.Errorf("stat %s: %w", p, statErr)
		}
		if !info.IsDir() {
			continue
		}
		root, err := canonicalWatchDir(p)
		if err != nil {
			return nil, dirRoots{}, fmt.Errorf("canonicalize %s: %w", p, err)
		}
		if _, err := addRecursiveWatch(watcher, root, watchedDirs, roots.sentinelParents); err != nil {
			return nil, dirRoots{}, fmt.Errorf("watch %s: %w", p, err)
		}
		roots.paths[root] = struct{}{}
		if parentDir := filepath.Dir(root); parentDir != root {
			if _, ok := watchedDirs[parentDir]; !ok {
				if err := watcher.Add(parentDir); err != nil {
					return nil, dirRoots{}, fmt.Errorf("watch %s: %w", parentDir, err)
				}
				watchedDirs[parentDir] = false
				roots.sentinelParents[parentDir] = struct{}{}
			}
		}
	}

	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			return nil, dirRoots{}, fmt.Errorf("stat %s: %w", p, statErr)
		}
		if info.IsDir() {
			continue
		}
		parentDir, err := canonicalWatchDir(filepath.Dir(p))
		if err != nil {
			return nil, dirRoots{}, fmt.Errorf("canonicalize parent of %s: %w", p, err)
		}
		// A directory serving as an explicit file's parent has a genuine
		// reason to react to arbitrary YAML writes in it (the pre-#515
		// behaviour) — if it was only tracked as a root-recreation
		// sentinel until now, that purpose no longer applies alone.
		delete(roots.sentinelParents, parentDir)
		if _, ok := watchedDirs[parentDir]; ok {
			continue
		}
		if err := watcher.Add(parentDir); err != nil {
			return nil, dirRoots{}, fmt.Errorf("watch %s: %w", parentDir, err)
		}
		watchedDirs[parentDir] = false
	}

	return watchedDirs, roots, nil
}

// handleDirectoryCreated processes a Create event whose target, newDir, is a
// directory. It only recursively watches (and rescans for YAML written
// before the watch was established — the create-dir-then-write-file race)
// when newDir's parent is itself part of a RECURSIVELY managed tree; a
// directory appearing inside a lone file's non-recursive parent watch (see
// setupWatches) is left untouched on purpose — expanding it would let a
// step's own output write into a newly created subdirectory retrigger the
// run that produced it, forever. sentinelParents is forwarded to
// addRecursiveWatch — see its doc for why.
//
// newDir is normalized to an absolute path before the lookup. fsnotify event
// names reflect the registered watch spelling, while setupWatches registers
// only absolute canonical directories; this keeps new-directory events and
// watchedDirs keys in the same namespace.
func handleDirectoryCreated(watcher *fsnotify.Watcher, newDir string, watchedDirs map[string]bool, sentinelParents map[string]struct{}) (foundYAML bool, err error) {
	newDir, err = absoluteWatchPath(newDir)
	if err != nil {
		return false, err
	}
	if !watchedDirs[filepath.Dir(newDir)] {
		return false, nil
	}
	return addRecursiveWatch(watcher, newDir, watchedDirs, sentinelParents)
}

// addRecursiveWatch adds an fsnotify watch for dir and every subdirectory
// nested beneath it, recording each newly watched directory in watchedDirs
// as part of a recursively managed tree (already-watched directories are
// skipped, EXCEPT that an existing non-recursive entry is upgraded — see
// below). It reports whether any YAML file was found during the walk, which
// callers use to detect a file that landed in a brand new directory before
// its watch was established.
//
// If the walk revisits a path ALREADY tracked as non-recursive (false) —
// e.g. two overlapping directory arguments like `--path jobs/existing
// --path jobs`, where processing "jobs/existing" first installs "jobs" as a
// setupWatches parent sentinel before "jobs" is ever walked in its own
// right — it is promoted to recursive (true) and dropped from
// sentinelParents (it now has a genuine reason, as a real recursive root, to
// react to its own contents; see setupWatches and the event loop). Without
// this, the root's own entry stayed permanently non-recursive despite the
// walk still correctly recursing into (and watching) everything beneath it,
// so handleDirectoryCreated later refused to expand into a brand new
// subdirectory created directly inside it — #515 reproduced for this
// multi-path invocation.
//
// dir is made absolute before the walk. filepath.WalkDir reports its root
// using the spelling it received, so a relative root would otherwise produce
// watchedDirs keys that disagree with fsnotify events from canonical watches.
func addRecursiveWatch(watcher *fsnotify.Watcher, dir string, watchedDirs map[string]bool, sentinelParents map[string]struct{}) (foundYAML bool, err error) {
	dir, err = absoluteWatchPath(dir)
	if err != nil {
		return false, err
	}
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// The path may have been removed mid-walk (e.g. a transient
			// temp directory or a racing delete) — skip it rather than
			// failing the whole scan.
			return nil
		}
		if d.IsDir() {
			if recursive, ok := watchedDirs[path]; ok {
				if !recursive {
					watchedDirs[path] = true
					delete(sentinelParents, path)
				}
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
// same path is wrongly treated as already watched. A removed path cannot be
// symlink-resolved, so root is normalized to its absolute spelling only.
func removeWatchedSubtree(watcher *fsnotify.Watcher, root string, watchedDirs map[string]bool) {
	root, err := absoluteWatchPath(root)
	if err != nil {
		return
	}
	prefix := root + string(filepath.Separator)
	for dir := range watchedDirs {
		if dir == root || strings.HasPrefix(dir, prefix) {
			_ = watcher.Remove(dir)
			delete(watchedDirs, dir)
		}
	}
}

// canonicalWatchDir gives every watched directory one identity, regardless of
// whether the user supplied it relative to the current working directory,
// absolutely, or through a directory symlink. Explicit FILE arguments remain
// untouched for discovery/execution (see resolveSymlinks), but their parent
// watch uses this identity so fsnotify events and watchedDirs keys agree.
func canonicalWatchDir(path string) (string, error) {
	abs, err := absoluteWatchPath(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// absoluteWatchPath is used for remove/rename event names, whose target may
// already be gone and therefore cannot be resolved through EvalSymlinks.
func absoluteWatchPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
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
