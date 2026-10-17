package cache

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestListByJobContextFiltersByFillingRunNamespace drives the joined query
// (task_caches JOIN job_runs) on SQLite: under a history scope a cache entry
// is listed only when the run that filled it sits in an allowed namespace; an
// entry whose run row is gone is hidden; unrestricted it is ListByJob.
func TestListByJobContextFiltersByFillingRunNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	now := time.Now().UTC()
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cache-ns", Namespace: "finance", TriggerID: uuid.New()}).Error)
	run := func(namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	entry := func(hash string, runID uuid.UUID) {
		require.NoError(t, db.Create(&models.TaskCache{Hash: hash, JobID: jobID, TaskName: hash, Result: "out-" + hash, RunID: runID, TaskRunID: uuid.New(), CreatedAt: now}).Error)
	}
	entry("marketing-entry", run("marketing"))
	entry("finance-entry", run("finance"))
	entry("orphan-entry", uuid.New())
	// Another job's entry never appears (the job predicate is qualified, so
	// the join cannot make it ambiguous).
	require.NoError(t, db.Create(&models.TaskCache{Hash: "other-job", JobID: uuid.New(), TaskName: "x", Result: "x", RunID: run("finance"), TaskRunID: uuid.New(), CreatedAt: now}).Error)

	hashes := func(entries []Entry) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.Hash)
		}
		return out
	}
	plain, err := store.ListByJob(jobID)
	require.NoError(t, err)
	unrestricted, err := store.ListByJobContext(context.Background(), jobID)
	require.NoError(t, err)
	require.ElementsMatch(t, hashes(plain), hashes(unrestricted))
	require.ElementsMatch(t, []string{"marketing-entry", "finance-entry", "orphan-entry"}, hashes(unrestricted))

	finance := authz.WithAllowedNamespaces(context.Background(), []string{"finance"})
	scoped, err := store.ListByJobContext(finance, jobID)
	require.NoError(t, err)
	require.Equal(t, []string{"finance-entry"}, hashes(scoped))
	require.Equal(t, "out-finance-entry", scoped[0].Result)
	require.True(t, authz.Filtered(finance))

	none, err := store.ListByJobContext(authz.WithAllowedNamespaces(context.Background(), nil), jobID)
	require.NoError(t, err)
	require.Empty(t, none)
}
