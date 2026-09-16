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
