package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const isolationRunPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*"]}
  isolated:
    kubernetes: {namespace: caesium-isolated}
  finance: {}
`

// TestIsolationSelectorValueFollowsTheNamespacesSettings: the stamp binds a
// row to its namespace's isolation settings, not the file's bytes — a
// comment-only edit or a change to another namespace keeps it, a change to the
// namespace's rules or mapping moves it — and a worker's claim values are
// exactly its isolated namespaces' stamps.
func TestIsolationSelectorValueFollowsTheNamespacesSettings(t *testing.T) {
	useAccessPolicy(t, isolationRunPolicy)
	base := policy.ProcessHolder().Current()
	_, marketing := RevisionIsolation(base, "marketing")
	_, isolated := RevisionIsolation(base, "isolated")
	require.ElementsMatch(t, []string{marketing, isolated}, IsolationClaimValues(base))
	finance, _ := RevisionIsolation(base, "finance")
	require.False(t, finance)

	useAccessPolicy(t, "# a comment-only edit\n"+isolationRunPolicy+"  other: {secrets: {allow: []}}\n")
	edited := policy.ProcessHolder().Current()
	require.NotEqual(t, base.Hash, edited.Hash)
	_, again := RevisionIsolation(edited, "marketing")
	require.Equal(t, marketing, again, "edits outside the namespace keep its stamp")

	useAccessPolicy(t, `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*", "env/EXTRA_*"]}
`)
	_, changed := RevisionIsolation(policy.ProcessHolder().Current(), "marketing")
	require.NotEqual(t, marketing, changed, "a change to the namespace's rules moves its stamp")
	_, ghost := RevisionIsolation(policy.ProcessHolder().Current(), "ghost")
	require.NotContains(t, IsolationClaimValues(policy.ProcessHolder().Current()), ghost, "an undeclared namespace is never pulled")
}

// TestRetryRestampsRowsUnderTheCurrentPolicy: a row that failed before a
// reload isolated its namespace comes back from a retry already marked, in the
// retry's own transaction — never schedulable unmarked.
func TestRetryRestampsRowsUnderTheCurrentPolicy(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	useAccessPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later: {}\n")
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)
	job := createNamespacedJob(t, db, "iso-retry-"+uuid.NewString()[:6], "later", nil)
	runID := seedNamespacedRun(t, db, job.ID, "later", StatusRunning, nil)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step", Type: "task"}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTasks(runID, []RegisterTaskInput{{Task: task, Atom: atom}}))
	var row models.TaskRun
	require.NoError(t, db.First(&row, "job_run_id = ?", runID).Error)
	require.NotContains(t, row.NodeSelector, IsolationSelectorKey)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Update("status", string(TaskStatusFailed)).Error)
	now := time.Now().UTC()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Updates(map[string]any{"status": string(StatusFailed), "completed_at": now}).Error)

	useAccessPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later:\n    secrets: {allow: []}\n")
	_, err := store.RetryFromFailure(runID)
	require.NoError(t, err)
	require.NoError(t, db.First(&row, "id = ?", row.ID).Error)
	require.Equal(t, string(TaskStatusPending), row.Status)
	_, want := NamespaceIsolation("later")
	require.Equal(t, want, row.NodeSelector[IsolationSelectorKey])

	// The row fails again, a reload removes the namespace's rules, and the
	// retry drops the stale mark: a worker serving the new file pulls it.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Update("status", string(TaskStatusFailed)).Error)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Updates(map[string]any{"status": string(StatusFailed), "completed_at": time.Now().UTC()}).Error)
	useAccessPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later: {}\n")
	_, err = store.RetryFromFailure(runID)
	require.NoError(t, err)
	require.NoError(t, db.First(&row, "id = ?", row.ID).Error)
	require.Equal(t, string(TaskStatusPending), row.Status)
	require.NotContains(t, row.NodeSelector, IsolationSelectorKey)
}

// TestIsolationFenceNeverOverwritesAMarkSetAfterItsRead: another node marks
// the row between the sweep's read and its write; the conditional write
// leaves that mark alone. A placeholder set in between is still bound.
func TestIsolationFenceNeverOverwritesAMarkSetAfterItsRead(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	useAccessPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later: {}\n")
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)
	job := createNamespacedJob(t, db, "fence-race-"+uuid.NewString()[:6], "later", nil)
	runID := seedNamespacedRun(t, db, job.ID, "later", StatusRunning, nil)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step", Type: "task"}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTasks(runID, []RegisterTaskInput{{Task: task, Atom: atom}}))
	var row models.TaskRun
	require.NoError(t, db.First(&row, "job_run_id = ?", runID).Error)

	useAccessPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later:\n    secrets: {allow: []}\n")
	_, want := NamespaceIsolation("later")
	t.Cleanup(func() { fenceBeforeWrite = nil })
	for _, raced := range []string{"v1-newer", isolationSelectorUnloaded} {
		require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Update("node_selector", datatypes.JSONMap{}).Error)
		fenceBeforeWrite = func(id uuid.UUID) {
			require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", id).
				Update("node_selector", datatypes.JSONMap{IsolationSelectorKey: raced}).Error)
		}
		n, err := store.FenceIsolation(t.Context(), policy.ProcessHolder().Current())
		require.NoError(t, err)
		require.NoError(t, db.First(&row, "id = ?", row.ID).Error)
		if raced == isolationSelectorUnloaded {
			require.Equal(t, 1, n)
			require.Equal(t, want, row.NodeSelector[IsolationSelectorKey], "a placeholder is bound")
			continue
		}
		require.Zero(t, n)
		require.Equal(t, raced, row.NodeSelector[IsolationSelectorKey], "a mark set after the read is kept")
	}
}

// TestWithIsolationSelectorDropsAMarkTheFileNoLongerAsksFor: a selector copied
// from an earlier row (a replay's source) keeps the step's own entries, loses
// a stale isolation mark under a file that leaves the namespace unisolated,
// is rebound under one that isolates it, and is kept as-is by a node serving
// no file (it cannot judge the mark).
func TestWithIsolationSelectorDropsAMarkTheFileNoLongerAsksFor(t *testing.T) {
	copied := datatypes.JSONMap{"zone": "a", IsolationSelectorKey: "v1-stale"}
	useAccessPolicy(t, isolationRunPolicy)
	require.Equal(t, datatypes.JSONMap{"zone": "a"}, WithIsolationSelector(copied, "finance"))
	require.Equal(t, datatypes.JSONMap{"zone": "a"}, WithIsolationSelector(copied, "default"))
	_, want := NamespaceIsolation("marketing")
	require.Equal(t, datatypes.JSONMap{"zone": "a", IsolationSelectorKey: want}, WithIsolationSelector(copied, "marketing"))
	require.Equal(t, "v1-stale", copied[IsolationSelectorKey], "the source selector is never mutated")

	policy.SetProcessHolder(nil)
	require.Equal(t, copied, WithIsolationSelector(copied, "finance"))
}

// TestRegisteredRowsOfIsolatedNamespacesAreMarked: a row registered in a
// namespace whose isolation context is non-default carries the reserved
// node-selector entry (so only a worker able to honour the context can pull
// it), keeps the step's own selector, and every other row is unchanged.
func TestRegisteredRowsOfIsolatedNamespacesAreMarked(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	useAccessPolicy(t, isolationRunPolicy)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)
	register := func(namespace string) models.TaskRun {
		job := createNamespacedJob(t, db, "iso-"+namespace+"-"+uuid.NewString()[:6], namespace, nil)
		runID := seedNamespacedRun(t, db, job.ID, namespace, StatusRunning, nil)
		task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step", Type: "task", NodeSelector: datatypes.JSONMap{"zone": "a"}}
		require.NoError(t, db.Create(task).Error)
		require.NoError(t, store.RegisterTasks(runID, []RegisterTaskInput{{Task: task, Atom: atom}}))
		var row models.TaskRun
		require.NoError(t, db.First(&row, "job_run_id = ?", runID).Error)
		namespaceOf, quarantine, err := store.RunIsolationInfo(t.Context(), runID)
		require.NoError(t, err)
		require.Equal(t, namespace, namespaceOf)
		require.False(t, quarantine)
		return row
	}
	for _, ns := range []string{"marketing", "isolated", "undeclared"} {
		row := register(ns)
		isolated, want := NamespaceIsolation(ns)
		require.True(t, isolated, ns)
		require.Equal(t, want, row.NodeSelector[IsolationSelectorKey], ns)
		requires, err := store.RunRequiresIsolation(t.Context(), row.JobRunID)
		require.NoError(t, err)
		require.True(t, requires, ns)
		require.Equal(t, "a", row.NodeSelector["zone"], ns)
	}
	for _, ns := range []string{"finance", "default"} {
		row := register(ns)
		require.Equal(t, datatypes.JSONMap{"zone": "a"}, row.NodeSelector, ns)
	}

	// No policy file: nothing is marked.
	policy.SetProcessHolder(nil)
	row := register("marketing")
	require.Equal(t, datatypes.JSONMap{"zone": "a"}, row.NodeSelector)
	requires, err := store.RunRequiresIsolation(t.Context(), row.JobRunID)
	require.NoError(t, err)
	require.False(t, requires)

	// A reload that isolates the namespace later: the owner marks the run's
	// unfinished rows once per revision, rebinding them to the new one.
	n, err := store.StampIsolationRequired(t.Context(), row.JobRunID, "v1-r1")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = store.StampIsolationRequired(t.Context(), row.JobRunID, "v1-r1")
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = store.StampIsolationRequired(t.Context(), row.JobRunID, "v1-r2")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var stamped models.TaskRun
	require.NoError(t, db.First(&stamped, "id = ?", row.ID).Error)
	require.Equal(t, "v1-r2", stamped.NodeSelector[IsolationSelectorKey])
	require.Equal(t, "a", stamped.NodeSelector["zone"])
}

// TestIsolationFenceMarksRowsWhenAReloadIsolatesTheirNamespace: rows
// registered while their namespace had no isolation settings are marked as
// soon as a reload gives it some, by the fence every policy-serving node runs
// (no run owner involved). The sweep only adds marks: a node whose file is
// stale (other settings, or none for the namespace) never rebinds or removes
// one, while the placeholder stamped before a file loaded is rebound. Finished
// rows and rows of unisolated namespaces are untouched.
func TestIsolationFenceMarksRowsWhenAReloadIsolatesTheirNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(body string) { require.NoError(t, os.WriteFile(path, []byte(body), 0o600)) }
	write("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later: {}\n  other: {}\n")
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)
	register := func(namespace string) models.TaskRun {
		job := createNamespacedJob(t, db, "fence-"+namespace+"-"+uuid.NewString()[:6], namespace, nil)
		runID := seedNamespacedRun(t, db, job.ID, namespace, StatusRunning, nil)
		task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step", Type: "task", NodeSelector: datatypes.JSONMap{"zone": "a"}}
		require.NoError(t, db.Create(task).Error)
		require.NoError(t, store.RegisterTasks(runID, []RegisterTaskInput{{Task: task, Atom: atom}}))
		var row models.TaskRun
		require.NoError(t, db.First(&row, "job_run_id = ?", runID).Error)
		require.NotContains(t, row.NodeSelector, IsolationSelectorKey)
		return row
	}
	pending := register("later")
	running := register("later")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", running.ID).Updates(map[string]any{"status": string(TaskStatusRunning), "claimed_by": "node-a"}).Error)
	finished := register("later")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", finished.ID).Update("status", string(TaskStatusSucceeded)).Error)
	unisolated := register("other")

	fence := NewIsolationFence(store, holder)
	holder.SetObserver(fence.Observe)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { fence.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	selectorOf := func(id uuid.UUID) datatypes.JSONMap {
		var row models.TaskRun
		require.NoError(t, db.First(&row, "id = ?", id).Error)
		return row.NodeSelector
	}
	awaitStamp := func(want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			return selectorOf(pending.ID)[IsolationSelectorKey] == want && selectorOf(running.ID)[IsolationSelectorKey] == want
		}, 10*time.Second, 20*time.Millisecond)
	}

	write("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later:\n    secrets: {allow: []}\n  other: {}\n")
	result, err := holder.Reload(t.Context(), policy.ReloadRequest{Actor: "test"})
	require.NoError(t, err)
	require.True(t, result.Changed)
	_, first := RevisionIsolation(holder.Current(), "later")
	awaitStamp(first)
	require.Equal(t, "a", selectorOf(pending.ID)["zone"], "the step's own selector is kept")

	require.Equal(t, datatypes.JSONMap{"zone": "a"}, selectorOf(finished.ID), "a finished row is left alone")
	require.Equal(t, datatypes.JSONMap{"zone": "a"}, selectorOf(unisolated.ID), "an unisolated namespace's row is left alone")

	// The sweep itself, synchronously: idempotent once the rows are marked.
	n, err := store.FenceIsolation(t.Context(), holder.Current())
	require.NoError(t, err)
	require.Zero(t, n)

	// Nodes serving stale files sweep too (at their startup or reload): they
	// neither rebind nor remove the mark.
	revisionOf := func(body string) *policy.Revision {
		stalePath := filepath.Join(t.TempDir(), "policy.yaml")
		require.NoError(t, os.WriteFile(stalePath, []byte(body), 0o600))
		stale, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: stalePath})
		require.NoError(t, err)
		return stale.Current()
	}
	for _, body := range []string{
		"apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later:\n    kubernetes: {namespace: caesium-later}\n  other: {}\n",
		"apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  later: {}\n  other: {}\n",
	} {
		n, err = store.FenceIsolation(t.Context(), revisionOf(body))
		require.NoError(t, err)
		require.Zero(t, n)
		require.Equal(t, first, selectorOf(pending.ID)[IsolationSelectorKey])
		require.Equal(t, first, selectorOf(running.ID)[IsolationSelectorKey])
	}

	// The placeholder stamped while a configured file was not loaded yet
	// carries no settings: the sweep binds it.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", pending.ID).
		Update("node_selector", datatypes.JSONMap{"zone": "a", IsolationSelectorKey: isolationSelectorUnloaded}).Error)
	n, err = store.FenceIsolation(t.Context(), holder.Current())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, first, selectorOf(pending.ID)[IsolationSelectorKey])

	// A fence on a node without a policy file never sweeps.
	quiet := make(chan struct{})
	go func() { NewIsolationFence(store, nil).Run(t.Context()); close(quiet) }()
	select {
	case <-quiet:
	case <-time.After(5 * time.Second):
		t.Fatal("a fence without a policy file must return at once")
	}
}
