package run

import (
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
