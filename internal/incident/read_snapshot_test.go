package incident

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	dbfaults "github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var errCheckpoint = errors.New("checkpoint in progress")

func openSnapshotDB(t *testing.T) *gorm.DB {
	t.Helper()
	store := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(store) })
	return store
}

func TestReadSnapshotRetriesAFailedCommit(t *testing.T) {
	conn, faults := dbfaults.FailCommits(t, openSnapshotDB(t), errCheckpoint, 1)
	retries := metrictestutil.CounterTotal(t, metrics.DBBusyRetriesTotal)

	calls := 0
	require.NoError(t, readSnapshot(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	}))

	require.Equal(t, 2, calls, "fn runs again after the failed COMMIT")
	require.Equal(t, []sql.TxOptions{{ReadOnly: true}, {ReadOnly: true}}, faults.Options)
	require.InDelta(t, retries+1, metrictestutil.CounterTotal(t, metrics.DBBusyRetriesTotal), 0)
}

func TestReadSnapshotSurfacesContentionWhenRetriesRunOut(t *testing.T) {
	attempts := len(db.BusyRetryBackoffs) + 1
	conn, _ := dbfaults.FailCommits(t, openSnapshotDB(t), errCheckpoint, attempts+1)

	calls := 0
	err := readSnapshot(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	})

	require.ErrorIs(t, err, errCheckpoint)
	require.Equal(t, attempts, calls)
}

func TestReadSnapshotDoesNotRetryOtherErrors(t *testing.T) {
	failure := errors.New("disk I/O error")
	conn, _ := dbfaults.FailCommits(t, openSnapshotDB(t), failure, 1)

	calls := 0
	err := readSnapshot(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	})

	require.ErrorIs(t, err, failure)
	require.Equal(t, 1, calls)
}

func TestReadSnapshotHonoursCancellation(t *testing.T) {
	conn, _ := dbfaults.FailCommits(t, openSnapshotDB(t), errCheckpoint, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	err := readSnapshot(ctx, conn, func(*gorm.DB) error {
		calls++
		cancel()
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls, "a cancelled retry does not start another attempt")
}

// The first attempt reads the whole topology and its COMMIT fails; a step is
// then added. The bundle carries the retry's topology alone: every task and
// edge once.
func TestBuildBundleRetriedTopologyHoldsNoDuplicates(t *testing.T) {
	store := openSnapshotDB(t)
	ctx := context.Background()
	trigger := mkTrigger(t, store)
	jobID := mkJob(t, store, trigger, "bundle-retry")
	mkTask := func(name string, position int) uuid.UUID {
		id := uuid.New()
		require.NoError(t, store.Create(&models.Task{ID: id, JobID: jobID, AtomID: uuid.New(), Name: name, Position: position}).Error)
		return id
	}
	extract := mkTask("extract", 0)
	load := mkTask("load", 1)
	require.NoError(t, store.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: extract, ToTaskID: load}).Error)
	inc, _, err := NewStore(store).OpenOrAppend(ctx, OpenParams{JobID: jobID, Class: ClassAuthFailure, LastError: "auth failed"})
	require.NoError(t, err)

	conn, faults := dbfaults.FailCommits(t, store, errCheckpoint, 1)
	faults.AfterRollback = func() { mkTask("publish", 2) }
	bundle, err := BuildBundle(ctx, conn, inc.ID, &Playbook{})

	require.NoError(t, err)
	require.Equal(t, 2, faults.Begins, "only the snapshot is a transaction here, and it ran twice")
	names := make([]string, 0, len(bundle.Job.Tasks))
	for _, task := range bundle.Job.Tasks {
		names = append(names, task.Name)
	}
	require.Equal(t, []string{"extract", "load", "publish"}, names)
	require.Equal(t, []BundleDAGEdge{{From: "extract", To: "load"}}, bundle.Job.Edges)
}
