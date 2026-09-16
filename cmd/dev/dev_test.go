package dev

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddRecursiveWatch is a table test for the directory-tracking logic
// dev.go's watch mode relies on to fix #515 (dev watch mode misses new DAG
// subdirectories and their edits): addRecursiveWatch must watch every
// directory in a tree (not just ones that happen to contain YAML), skip
// directories it has already watched, and report whether it found a YAML
// file anywhere in the walk — the signal callers use to detect a file that
// landed in a brand new directory before its watch was established (the
// create-dir-then-write-file race).
func TestAddRecursiveWatch(t *testing.T) {
	newWatcher := func(t *testing.T) *fsnotify.Watcher {
		t.Helper()
		w, err := fsnotify.NewWatcher()
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		return w
	}

	t.Run("watches every directory in a nested tree and reports YAML found", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "a", "b")
		require.NoError(t, os.MkdirAll(nested, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "a", "job.job.yaml"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(nested, "notes.txt"), []byte("x"), 0o644))

		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

		foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML, "expected the walk to find job.job.yaml under root/a")

		for _, dir := range []string{root, filepath.Join(root, "a"), nested, filepath.Join(root, "empty")} {
			_, ok := watchedDirs[dir]
			assert.True(t, ok, "expected %s to be watched", dir)
		}
	})

	t.Run("reports no YAML found for a tree with none", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "readme.txt"), []byte("x"), 0o644))

		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

		foundYAML, err := addRecursiveWatch(watcher, root, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML)
		_, ok := watchedDirs[filepath.Join(root, "sub")]
		assert.True(t, ok, "a directory with no YAML must still be watched")
	})

	t.Run("is idempotent for already-watched directories", func(t *testing.T) {
		root := t.TempDir()
		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

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

		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

		foundYAML, err := addRecursiveWatch(watcher, newDir, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML)
		_, ok := watchedDirs[newDir]
		assert.True(t, ok)
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
	newWatcher := func(t *testing.T) *fsnotify.Watcher {
		t.Helper()
		w, err := fsnotify.NewWatcher()
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		return w
	}

	t.Run("prunes the moved directory and every descendant, leaves siblings", func(t *testing.T) {
		watcher := newWatcher(t)
		root := t.TempDir()
		watchedDirs := map[string]struct{}{
			root:                               {},
			filepath.Join(root, "new"):         {},
			filepath.Join(root, "new", "deep"): {},
			filepath.Join(root, "new-sibling"): {},
			filepath.Join(root, "other"):       {},
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
		watcher := newWatcher(t)
		root := t.TempDir()
		newDir := filepath.Join(root, "new")
		deepDir := filepath.Join(newDir, "deep")
		require.NoError(t, os.MkdirAll(deepDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(deepDir, "job.job.yaml"), []byte("x"), 0o644))

		watchedDirs := make(map[string]struct{})
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
// watches. dev.go now resolves the root with filepath.EvalSymlinks before
// calling addRecursiveWatch; this proves both halves of that fix.
func TestAddRecursiveWatchSymlinkRoot(t *testing.T) {
	newWatcher := func(t *testing.T) *fsnotify.Watcher {
		t.Helper()
		w, err := fsnotify.NewWatcher()
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		return w
	}

	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "job.job.yaml"), []byte("x"), 0o644))
	link := filepath.Join(base, "jobs")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	t.Run("documents the gotcha: walking the symlink path directly finds nothing", func(t *testing.T) {
		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

		foundYAML, err := addRecursiveWatch(watcher, link, watchedDirs)
		require.NoError(t, err)
		assert.False(t, foundYAML, "filepath.WalkDir must not follow the root symlink")
		assert.Empty(t, watchedDirs, "no watch should be installed for an unresolved symlink root")
	})

	t.Run("resolving the symlink first (dev.go's fix) installs the watch and finds YAML", func(t *testing.T) {
		watcher := newWatcher(t)
		watchedDirs := make(map[string]struct{})

		resolved, err := filepath.EvalSymlinks(link)
		require.NoError(t, err)

		foundYAML, err := addRecursiveWatch(watcher, resolved, watchedDirs)
		require.NoError(t, err)
		assert.True(t, foundYAML)
		assert.Contains(t, watchedDirs, resolved)
		assert.Contains(t, watcher.WatchList(), resolved)
	})
}
