package dev

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testManifest returns a minimal valid job manifest with the given alias,
// shared by every test in this file that needs a real, parseable definition.
func testManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["echo", "hi"]
`, alias)
}

// newTestWatcher returns a real fsnotify.Watcher closed on test cleanup,
// shared by every test in this file that needs one.
func newTestWatcher(t *testing.T) *fsnotify.Watcher {
	t.Helper()
	w, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestAddRecursiveWatch is a table test for the directory-tracking logic
// dev.go's watch mode relies on to fix #515 (dev watch mode misses new DAG
// subdirectories and their edits): addRecursiveWatch must watch every
// directory in a tree (not just ones that happen to contain YAML), skip
// directories it has already watched, and report whether it found a YAML
// file anywhere in the walk — the signal callers use to detect a file that
// landed in a brand new directory before its watch was established (the
// create-dir-then-write-file race).
func TestAddRecursiveWatch(t *testing.T) {
	t.Run("watches every directory in a nested tree and reports YAML found", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "a", "b")
		require.NoError(t, os.MkdirAll(nested, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "a", "job.job.yaml"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(nested, "notes.txt"), []byte("x"), 0o644))

		watcher := newTestWatcher(t)
		watchedDirs := make(map[string]bool)

		foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML, "expected the walk to find job.job.yaml under root/a")

		for _, dir := range []string{root, filepath.Join(root, "a"), nested, filepath.Join(root, "empty")} {
			assert.True(t, watchedDirs[dir], "expected %s to be watched recursively", dir)
		}
	})

	t.Run("reports no YAML found for a tree with none", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "readme.txt"), []byte("x"), 0o644))

		watcher := newTestWatcher(t)
		watchedDirs := make(map[string]bool)

		foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML)
		assert.True(t, watchedDirs[filepath.Join(root, "sub")], "a directory with no YAML must still be watched")
	})

	t.Run("is idempotent for already-watched directories", func(t *testing.T) {
		root := t.TempDir()
		watcher := newTestWatcher(t)
		watchedDirs := make(map[string]bool)

		_, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		require.Len(t, watchedDirs, 1)

		// A second pass over the SAME tree must not error (fsnotify.Add is not
		// re-invoked for directories already tracked) and must not change the
		// tracked set.
		foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML)
		assert.Len(t, watchedDirs, 1)
	})

	t.Run("catches a YAML file written into a new directory before the watch existed", func(t *testing.T) {
		// Simulates the create-dir-then-write-file race #515 calls out: by the
		// time the caller gets around to calling addRecursiveWatch for a
		// freshly created directory, the file may already be sitting inside
		// it. The rescan (not a separate fsnotify event) must catch it.
		root := t.TempDir()
		newDir := filepath.Join(root, "new")
		require.NoError(t, os.MkdirAll(newDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(newDir, "new.job.yaml"), []byte("x"), 0o644))

		watcher := newTestWatcher(t)
		watchedDirs := make(map[string]bool)

		foundYAML, err := addRecursiveWatch(watcher, newDir, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML)
		assert.True(t, watchedDirs[newDir])
	})
}

// TestRemoveWatchedSubtree covers a gap an adversarial review caught on the
// #515 fix: a directory move/remove only generates ONE fsnotify event, for
// that directory itself — not for any descendants that got their own entry
// in watchedDirs during the initial recursive walk (verified: on Linux,
// moving an ancestor directory does not emit move/remove events for watches
// held on its descendants, since those are separate inotify watches by
// inode). Deleting only the named entry left stale descendant paths behind,
// so recreating the same tree later looked "already watched" and skipped
// installing a real watch on the new inode — silently dropping all further
// edits to it. removeWatchedSubtree must prune the whole former subtree.
func TestRemoveWatchedSubtree(t *testing.T) {
	t.Run("prunes the moved directory and every descendant, leaves siblings", func(t *testing.T) {
		watcher := newTestWatcher(t)
		root := t.TempDir()
		watchedDirs := map[string]bool{
			root:                               true,
			filepath.Join(root, "new"):         true,
			filepath.Join(root, "new", "deep"): true,
			filepath.Join(root, "new-sibling"): true,
			filepath.Join(root, "other"):       true,
		}

		removeWatchedSubtree(watcher, filepath.Join(root, "new"), watchedDirs)

		_, stillNew := watchedDirs[filepath.Join(root, "new")]
		_, stillDeep := watchedDirs[filepath.Join(root, "new", "deep")]
		assert.False(t, stillNew, "the moved/removed directory itself must be pruned")
		assert.False(t, stillDeep, "a descendant tracked under its own path must be pruned too")

		// A sibling whose name merely shares the removed directory's name as
		// a PREFIX (not a path component) must survive — this guards against
		// a naive strings.HasPrefix(dir, root) without the separator.
		_, stillSibling := watchedDirs[filepath.Join(root, "new-sibling")]
		assert.True(t, stillSibling, "a sibling directory must not be pruned")
		_, stillOther := watchedDirs[filepath.Join(root, "other")]
		assert.True(t, stillOther, "an unrelated directory must not be pruned")
		_, stillRoot := watchedDirs[root]
		assert.True(t, stillRoot, "an ancestor of the removed directory must not be pruned")
	})

	t.Run("recreating a pruned subtree installs a real watch on the new inode", func(t *testing.T) {
		// End-to-end proof at the addRecursiveWatch/removeWatchedSubtree
		// composition level (the exact sequence dev.go's event loop runs):
		// walk once, prune as if the directory moved away, recreate it, walk
		// again — the descendant must come back watched.
		watcher := newTestWatcher(t)
		root := t.TempDir()
		newDir := filepath.Join(root, "new")
		deepDir := filepath.Join(newDir, "deep")
		require.NoError(t, os.MkdirAll(deepDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(deepDir, "job.job.yaml"), []byte("x"), 0o644))

		watchedDirs := make(map[string]bool)
		_, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		require.Contains(t, watchedDirs, deepDir)
		require.Contains(t, watcher.WatchList(), deepDir)

		// Simulate the directory moving OUTSIDE the watched tree: remove it
		// from disk and from tracking (removeWatchedSubtree also issues
		// watcher.Remove for every pruned path).
		require.NoError(t, os.RemoveAll(newDir))
		removeWatchedSubtree(watcher, newDir, watchedDirs)
		assert.NotContains(t, watchedDirs, deepDir)
		assert.NotContains(t, watcher.WatchList(), deepDir,
			"the descendant's fsnotify watch must actually be removed, not just untracked")

		// Recreate the same tree (a new inode) and re-walk it, exactly as
		// the Create branch in dev.go's event loop would.
		require.NoError(t, os.MkdirAll(deepDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(deepDir, "job.job.yaml"), []byte("x"), 0o644))
		foundYAML, err := addRecursiveWatch(watcher, newDir, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML)
		assert.Contains(t, watchedDirs, deepDir)
		assert.Contains(t, watcher.WatchList(), deepDir,
			"a fresh watch must be installed for the recreated directory's new inode")
	})
}

// TestAddRecursiveWatchSymlinkRoot covers the second adversarial-review gap:
// filepath.WalkDir Lstats its root and does not follow a symlink there (only
// os.Stat, which the caller uses to classify a --path argument as a file or
// directory, follows it) — so watching a symlinked directory, or the parent
// of an explicit file reached through a symlink, silently installed zero
// watches. dev.go now resolves every --path argument with resolveSymlinks
// ONCE, up front — before definition discovery (jobdef.CollectDefinitions,
// jobdef.ResolveYAMLFiles) or watch setup ever sees it — rather than only
// inside watch setup, since a second-round review caught that discovery has
// the IDENTICAL os.Lstat-via-filepath.WalkDir blindness and runs first: the
// first executeRun would fail closed with "no job definitions selected"
// before the original, watch-setup-only fix ever ran. This proves both the
// watching half (via addRecursiveWatch, as before) and, separately, the
// discovery half (via jobdef.CollectDefinitions, the actual CLI failure the
// review flagged — the original version of this test resolved manually and
// only ever exercised addRecursiveWatch, missing that failure entirely).
func TestAddRecursiveWatchSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	manifest := `apiVersion: v1
kind: Job
metadata:
  alias: symlink-discovery-test
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["echo", "hi"]
`
	require.NoError(t, os.WriteFile(filepath.Join(real, "job.job.yaml"), []byte(manifest), 0o644))
	link := filepath.Join(base, "jobs")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	t.Run("documents the gotcha: walking the symlink path directly finds nothing", func(t *testing.T) {
		watcher := newTestWatcher(t)
		watchedDirs := make(map[string]bool)

		foundYAML, err := addRecursiveWatch(watcher, link, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML, "filepath.WalkDir must not follow the root symlink")
		assert.Empty(t, watchedDirs, "no watch should be installed for an unresolved symlink root")
	})

	t.Run("documents the SAME gotcha for definition discovery, unresolved", func(t *testing.T) {
		// This is the failure a second-round review caught: discovery runs
		// BEFORE watch setup (the very first executeRun, and every rerun),
		// via the identical os.Stat+filepath.WalkDir pattern, and is
		// equally blind to a symlinked root.
		defs, err := jobdef.CollectDefinitions([]string{link}, true)
		require.NoError(t, err)
		assert.Empty(t, defs, "CollectDefinitions must not see through an unresolved symlink root")
	})

	t.Run("resolveSymlinks (dev.go's actual fix) enables both discovery and watching", func(t *testing.T) {
		resolved, err := resolveSymlinks([]string{link})
		require.NoError(t, err)
		require.Len(t, resolved, 1)

		defs, err := jobdef.CollectDefinitions(resolved, true)
		require.NoError(t, err)
		require.Len(t, defs, 1, "discovery must find the manifest once the symlink is resolved")
		assert.Equal(t, "symlink-discovery-test", defs[0].Metadata.Alias)

		watcher := newTestWatcher(t)
		watchedDirs, rootPaths, err := setupWatches(watcher, resolved)
		require.NoError(t, err)
		assert.True(t, watchedDirs[resolved[0]], "the resolved directory root must be watched recursively")
		assert.Contains(t, watcher.WatchList(), resolved[0])
		assert.Contains(t, rootPaths, resolved[0])
	})
}

// TestSetupWatchesSingleFileInput fixes a second-round-review regression in
// the #515 fix: recursively watching a single explicit file's ENTIRE parent
// directory (rather than just that directory itself, non-recursively, as
// before #515) means a step that writes its own output into a subdirectory
// of that same directory can retrigger the run that produced it, forever.
// setupWatches must keep an explicit file argument's parent non-recursive,
// exactly like the pre-#515 behaviour, while still fully recursing into a
// directory argument.
func TestSetupWatchesSingleFileInput(t *testing.T) {
	t.Run("a file argument's parent is watched but NOT recursively", func(t *testing.T) {
		root := t.TempDir()
		manifestPath := filepath.Join(root, "job.yaml")
		require.NoError(t, os.WriteFile(manifestPath, []byte("x"), 0o644))
		// A pre-existing nested directory — must stay unwatched for a
		// single-file input; only #515's directory-input case recurses.
		nested := filepath.Join(root, "output")
		require.NoError(t, os.MkdirAll(nested, 0o755))

		watcher := newTestWatcher(t)
		watchedDirs, _, err := setupWatches(watcher, []string{manifestPath})
		require.NoError(t, err)

		recursive, ok := watchedDirs[root]
		require.True(t, ok, "the file's parent directory must be watched")
		assert.False(t, recursive, "a single-file input's parent watch must be tracked as non-recursive")
		assert.NotContains(t, watchedDirs, nested)
		assert.Contains(t, watcher.WatchList(), root)
		assert.NotContains(t, watcher.WatchList(), nested)
	})

	t.Run("a directory argument is still fully recursive", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "a", "b")
		require.NoError(t, os.MkdirAll(nested, 0o755))

		watcher := newTestWatcher(t)
		watchedDirs, rootPaths, err := setupWatches(watcher, []string{root})
		require.NoError(t, err)

		assert.True(t, watchedDirs[root])
		assert.True(t, watchedDirs[filepath.Join(root, "a")])
		assert.True(t, watchedDirs[nested])
		assert.Contains(t, rootPaths, root)
	})

	t.Run("a directory argument's recursive tag wins over a file argument sharing its parent", func(t *testing.T) {
		root := t.TempDir()
		manifestPath := filepath.Join(root, "job.yaml")
		require.NoError(t, os.WriteFile(manifestPath, []byte("x"), 0o644))

		watcher := newTestWatcher(t)
		// root supplied as BOTH a directory input and (via job.yaml) a file
		// input whose parent is that same directory — the directory's
		// recursive tag must win regardless of slice order.
		watchedDirs, _, err := setupWatches(watcher, []string{manifestPath, root})
		require.NoError(t, err)
		assert.True(t, watchedDirs[root], "the shared directory must end up tagged recursive")
	})
}

// TestRootRecreationAfterMove fixes a round-4 P2 finding: moving a selected
// DIRECTORY root away (removeWatchedSubtree correctly drops it and every
// descendant from watchedDirs on the Remove/Rename event) and recreating it
// left NOTHING watching for its return — a directory root's own tree only
// ever watches itself and its descendants, never its parent, so the parent
// directory's watch (which WOULD see the recreation) simply didn't exist.
// setupWatches now also installs a non-recursive watch on each directory
// root's OWN parent and records the root in rootPaths; this proves the
// full sequence a real watch-mode session goes through: initial setup,
// the root moving away, its recreation being noticed via rootPaths (the
// event loop's job, exercised here directly since it is the exact check
// dev.go's Create branch performs), and — critically — that a SUBSEQUENT
// edit nested inside the recreated root is still picked up, proving full
// recursive watching was genuinely restored under it, not just the root
// entry itself.
func TestRootRecreationAfterMove(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "jobs")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "job.job.yaml"), []byte(testManifest("original")), 0o644))

	watcher := newTestWatcher(t)
	watchedDirs, rootPaths, err := setupWatches(watcher, []string{root})
	require.NoError(t, err)
	require.Contains(t, rootPaths, root)
	require.True(t, watchedDirs[root])
	parent := filepath.Dir(root)
	require.Contains(t, watcher.WatchList(), parent, "the root's own parent must be watched too")

	// Move the root away — the real-world equivalent of `mv jobs /elsewhere`
	// — and simulate the Rename event's handling exactly as dev.go's event
	// loop would: removeWatchedSubtree drops the root and its descendant.
	elsewhere := filepath.Join(base, "elsewhere")
	require.NoError(t, os.Rename(root, elsewhere))
	removeWatchedSubtree(watcher, root, watchedDirs)
	assert.NotContains(t, watchedDirs, root)
	assert.NotContains(t, watcher.WatchList(), root)
	// rootPaths must survive: we still want to recognize the root's return.
	assert.Contains(t, rootPaths, root)

	// Recreate the root with a (different) valid DAG — the real-world
	// equivalent of `mkdir jobs && write jobs/job.yaml`.
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "job.job.yaml"), []byte(testManifest("recreated")), 0o644))

	// This is the exact check dev.go's event loop's Create branch performs
	// before falling back to handleDirectoryCreated: is the created name one
	// of the tracked roots? If so, restore full recursive watching under it.
	_, isRoot := rootPaths[filepath.Clean(root)]
	require.True(t, isRoot, "the recreated path must still be recognized as a tracked root")
	foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
	require.NoError(t, err)
	assert.True(t, foundYAML, "the recreated root's own manifest must be found on restoration")
	assert.True(t, watchedDirs[root], "the root must be recursively tracked again")
	assert.Contains(t, watcher.WatchList(), root)

	// A SUBSEQUENT edit nested inside the recreated root must still be
	// picked up — proving recursive watching was genuinely restored, not
	// just a watch on the root entry itself.
	nested := filepath.Join(root, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	foundYAML, err = handleDirectoryCreated(watcher, nested, watchedDirs)
	require.NoError(t, err)
	assert.False(t, foundYAML, "nothing written into it yet")
	assert.True(t, watchedDirs[nested], "the newly created nested directory must be watched recursively")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "nested.job.yaml"), []byte(testManifest("nested")), 0o644))
	defs, err := jobdef.CollectDefinitions([]string{root}, true)
	require.NoError(t, err)
	require.Len(t, defs, 2, "both the recreated root's own manifest and the nested one must be discovered")
}

// TestHandleDirectoryCreated fixes the same second-round regression at the
// event-loop decision point: a directory appearing inside a lone file's
// non-recursive parent watch must be left alone (never installed as a new
// watch, never expanded into), while one appearing inside an actual
// recursive tree (a directory --path argument, or something discovered
// beneath one) must still be picked up, exactly as #515 requires.
func TestHandleDirectoryCreated(t *testing.T) {
	t.Run("ignores a directory created under a non-recursive (file-parent) watch", func(t *testing.T) {
		root := t.TempDir()
		watcher := newTestWatcher(t)
		watchedDirs := map[string]bool{root: false} // simulates a single-file input's parent

		newDir := filepath.Join(root, "output")
		require.NoError(t, os.MkdirAll(newDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(newDir, "result.yaml"), []byte("x"), 0o644))

		foundYAML, err := handleDirectoryCreated(watcher, newDir, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML, "a directory under a non-recursive parent must not be expanded")
		assert.NotContains(t, watchedDirs, newDir)
		assert.NotContains(t, watcher.WatchList(), newDir,
			"no watch should be installed for a directory under a non-recursive parent")
	})

	t.Run("expands a directory created under a recursive tree", func(t *testing.T) {
		root := t.TempDir()
		watcher := newTestWatcher(t)
		watchedDirs := map[string]bool{root: true} // simulates a directory --path argument

		newDir := filepath.Join(root, "sub")
		require.NoError(t, os.MkdirAll(newDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(newDir, "job.job.yaml"), []byte("x"), 0o644))

		foundYAML, err := handleDirectoryCreated(watcher, newDir, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML)
		assert.True(t, watchedDirs[newDir])
		assert.Contains(t, watcher.WatchList(), newDir)
	})
}

// TestPathNormalizationDefaultRoot fixes a round-3 P1 finding: on Linux,
// fsnotify's inotify backend builds an event's Name by string-concatenating
// the watch's OWN path (cleaned via filepath.Clean when it was Add()ed) with
// the raw entry name — NOT filepath.Join. A watch on "." (the default root
// with no --path given) therefore reports a newly created entry as "./new",
// while addRecursiveWatch's own watchedDirs entries are always recorded
// under filepath.WalkDir's root spelling. Before this fix, the root itself
// was tracked as "." (clean) but a later Create event named "./new" (dirty)
// was looked up as-is, and once "./new" was itself recursively watched, ITS
// underlying fsnotify watch got cleaned to "new" — so watchedDirs held
// "./new" while later events for its OWN children arrived as "new/deeper",
// a lookup miss. Every path this package treats as a watchedDirs key is now
// normalized with filepath.Clean, so the two spellings can no longer diverge.
func TestPathNormalizationDefaultRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	watcher := newTestWatcher(t)
	watchedDirs, _, err := setupWatches(watcher, []string{"."})
	require.NoError(t, err)
	assert.True(t, watchedDirs["."], "the default root must be tracked under its clean form")

	// Simulate inotify's ACTUAL (unclean) event spelling for a directory
	// created directly under "." — "./new", not "new".
	require.NoError(t, os.MkdirAll("new", 0o755))
	foundYAML, err := handleDirectoryCreated(watcher, "./new", watchedDirs)
	require.NoError(t, err)
	assert.False(t, foundYAML, "nothing has been written into it yet")
	assert.True(t, watchedDirs["new"], "must be tracked under its CLEANED form")
	assert.NotContains(t, watchedDirs, "./new", "must not also carry an uncleaned duplicate key")
	assert.Contains(t, watcher.WatchList(), "new")

	// A directory created inside "new" is reported by fsnotify using ITS
	// OWN watch path — "new" was Add()ed directly (via addRecursiveWatch
	// above), so fsnotify already stores it clean, and the event for a
	// grandchild arrives as "new/deeper" (no "./" prefix this time). The
	// lookup on filepath.Dir("new/deeper") == "new" must hit the entry
	// handleDirectoryCreated just recorded above.
	require.NoError(t, os.MkdirAll(filepath.Join("new", "deeper"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("new", "deeper", "job.job.yaml"), []byte("x"), 0o644))
	foundYAML, err = handleDirectoryCreated(watcher, "new/deeper", watchedDirs)
	require.NoError(t, err)
	assert.True(t, foundYAML, "the nested DAG under the \"./new\"-spelled directory must still be discovered")
	assert.True(t, watchedDirs["new/deeper"])
}

// TestResolveSymlinksPreservesExplicitFileIdentity fixes a round-3 P2
// finding: resolveSymlinks previously resolved EVERY path, including an
// explicit FILE argument reached through a symlink — permanently pinning
// whatever it pointed to at startup into the paths slice executeRun reads
// on every run. Repointing the symlink afterward (an atomic manifest swap,
// e.g. `ln -sfn v2.yaml current.yaml`) then produced no observable change,
// ever: discovery kept reading the OLD target. resolveSymlinks now only
// resolves DIRECTORY paths; a file argument is returned exactly as given,
// so each rerun's os.ReadFile (via jobdef.CollectDefinitions) naturally
// picks up whatever the symlink currently points to.
func TestResolveSymlinksPreservesExplicitFileIdentity(t *testing.T) {
	base := t.TempDir()
	templatesDir := filepath.Join(base, "templates")
	require.NoError(t, os.MkdirAll(templatesDir, 0o755))

	v1 := filepath.Join(templatesDir, "v1.yaml")
	require.NoError(t, os.WriteFile(v1, []byte(testManifest("template-v1")), 0o644))
	v2 := filepath.Join(templatesDir, "v2.yaml")
	require.NoError(t, os.WriteFile(v2, []byte(testManifest("template-v2")), 0o644))

	jobsDir := filepath.Join(base, "jobs")
	require.NoError(t, os.MkdirAll(jobsDir, 0o755))
	current := filepath.Join(jobsDir, "current.yaml")
	if err := os.Symlink(v1, current); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	resolved, err := resolveSymlinks([]string{current})
	require.NoError(t, err)
	require.Equal(t, []string{current}, resolved,
		"an explicit file argument must be left exactly as given, not pinned to its symlink target")

	defs, err := jobdef.CollectDefinitions(resolved, true)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "template-v1", defs[0].Metadata.Alias)

	// Repoint the symlink (an atomic manifest swap). Reading through the
	// SAME unresolved path must pick up the new target immediately — proving
	// nothing pinned the old one.
	require.NoError(t, os.Remove(current))
	require.NoError(t, os.Symlink(v2, current))

	resolvedAgain, err := resolveSymlinks([]string{current})
	require.NoError(t, err)
	require.Equal(t, []string{current}, resolvedAgain)

	defs, err = jobdef.CollectDefinitions(resolvedAgain, true)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "template-v2", defs[0].Metadata.Alias,
		"repointing the symlink must be visible on the very next read")
}

// TestResolveSymlinksAcceptsFileWhoseTargetLacksYAMLExtension covers the
// other half of the round-3 finding: fully resolving an explicit file
// argument previously rejected a perfectly valid selection whenever its
// target's name didn't itself end in .yaml/.yml (jobdef.IsYAML checks the
// RESOLVED path's extension). A file argument left unresolved is judged by
// its OWN extension, which is what the user actually selected.
func TestResolveSymlinksAcceptsFileWhoseTargetLacksYAMLExtension(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "template-no-ext")
	require.NoError(t, os.WriteFile(target, []byte(testManifest("no-ext-target")), 0o644))

	link := filepath.Join(base, "current.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	resolved, err := resolveSymlinks([]string{link})
	require.NoError(t, err)
	require.Equal(t, []string{link}, resolved)

	defs, err := jobdef.CollectDefinitions(resolved, true)
	require.NoError(t, err,
		"a selected .yaml symlink must not be rejected just because its target lacks a YAML extension")
	require.Len(t, defs, 1)
	assert.Equal(t, "no-ext-target", defs[0].Metadata.Alias)
}
