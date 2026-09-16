package dev

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		watchedDirs, err := setupWatches(watcher, resolved)
		require.NoError(t, err)
		assert.True(t, watchedDirs[resolved[0]], "the resolved directory root must be watched recursively")
		assert.Contains(t, watcher.WatchList(), resolved[0])
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
		watchedDirs, err := setupWatches(watcher, []string{manifestPath})
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
		watchedDirs, err := setupWatches(watcher, []string{root})
		require.NoError(t, err)

		assert.True(t, watchedDirs[root])
		assert.True(t, watchedDirs[filepath.Join(root, "a")])
		assert.True(t, watchedDirs[nested])
	})

	t.Run("a directory argument's recursive tag wins over a file argument sharing its parent", func(t *testing.T) {
		root := t.TempDir()
		manifestPath := filepath.Join(root, "job.yaml")
		require.NoError(t, os.WriteFile(manifestPath, []byte("x"), 0o644))

		watcher := newTestWatcher(t)
		// root supplied as BOTH a directory input and (via job.yaml) a file
		// input whose parent is that same directory — the directory's
		// recursive tag must win regardless of slice order.
		watchedDirs, err := setupWatches(watcher, []string{manifestPath, root})
		require.NoError(t, err)
		assert.True(t, watchedDirs[root], "the shared directory must end up tagged recursive")
	})
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
