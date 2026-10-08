package job

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	backfillstore "github.com/caesium-cloud/caesium/internal/backfill"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func backfillLifetimeFixture(t *testing.T) (*models.Backfill, *models.Job, *backfillstore.Store, *runstore.Store) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	j := &models.Job{ID: uuid.New(), Alias: "backfill-lifetime-" + uuid.NewString()}
	require.NoError(t, db.Create(j).Error)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := &models.Backfill{ID: uuid.New(), JobID: j.ID, Status: models.BackfillStatusRunning, Start: start, End: start.Add(2 * time.Hour), MaxConcurrent: 1, Reprocess: models.ReprocessAll}
	bStore := backfillstore.NewStore(db)
	require.NoError(t, bStore.Create(b))
	return b, j, bStore, runstore.NewStore(db)
}

func TestBackfillChildrenDetachUserCancelButFollowServerAndJoin(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	owner := runlife.New(context.Background())
	workCtx, release, err := owner.Reserve(runlife.WithSupervisor(t.Context(), owner))
	require.NoError(t, err)
	driverCtx, cancelDriver := context.WithCancel(workCtx)
	defer cancelDriver()
	started := make(chan context.Context, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	driverDone := make(chan struct{})
	t.Cleanup(func() {
		cancelDriver()
		owner.CloseAndCancel()
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Wait(ctx))
	})
	go func() {
		defer close(driverDone)
		defer release()
		runBackfill(driverCtx, b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
			func(ctx context.Context, _ *models.Job, _ map[string]string) error {
				started <- ctx
				<-ctx.Done()
				<-finish
				return ctx.Err()
			})
	}()
	var child context.Context
	select {
	case child = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill child did not start")
	}
	cancelDriver()
	require.NoError(t, child.Err(), "user backfill cancellation lets accepted child drain")
	select {
	case <-driverDone:
		t.Fatal("driver returned before child drain")
	default:
	}
	owner.CloseAndCancel()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("server cancellation did not reach child")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded)
	unblock()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	require.NoError(t, owner.Wait(waitCtx))
	<-driverDone
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusCancelled, stored.Status)
	require.Equal(t, 1, stored.FailedRuns, "final progress flush completed before owner join")
	runID, ok := runstore.FromContext(child)
	require.True(t, ok)
	require.Zero(t, CancelRunContexts(runID), "child registration released after join")
}

func TestClosedOwnerBackfillCreatesNoChildRun(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	owner := runlife.New(context.Background())
	owner.CloseAndCancel()
	called := false
	runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
		func(context.Context, *models.Job, map[string]string) error { called = true; return nil })
	require.False(t, called)
	var count int64
	require.NoError(t, rStore.DB().Model(&models.JobRun{}).Where("backfill_id = ?", b.ID).Count(&count).Error)
	require.Zero(t, count)
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusCancelled, stored.Status)
}

func TestBackfillChildAdmissionFailureReleasesReservation(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	injected := errors.New("backfill child insertion failed")
	const callback = "test:backfill_child_insert_failure"
	require.NoError(t, rStore.DB().Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "JobRun" {
			_ = tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { _ = rStore.DB().Callback().Create().Remove(callback) })
	owner := runlife.New(context.Background())
	workCtx, release, err := owner.Reserve(runlife.WithSupervisor(t.Context(), owner))
	require.NoError(t, err)
	called := false
	runBackfill(workCtx, b, j, mustParseCron(t, "0 * * * *"), time.UTC, bStore, rStore,
		func(context.Context, *models.Job, map[string]string) error { called = true; return nil })
	release()
	owner.CloseAndCancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, owner.Wait(ctx))
	require.False(t, called)
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, models.BackfillStatusFailed, stored.Status)
}

func TestDirectBackfillRetainsUserCancelDrain(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	driverCtx, cancelDriver := context.WithCancel(t.Context())
	defer cancelDriver()
	started := make(chan context.Context, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	done := make(chan struct{})
	t.Cleanup(func() { cancelDriver(); unblock(); <-done })
	schedule := mustParseCron(t, "0 * * * *")
	go func() {
		defer close(done)
		runBackfill(driverCtx, b, j, schedule, time.UTC, bStore, rStore,
			func(ctx context.Context, _ *models.Job, _ map[string]string) error {
				started <- ctx
				<-finish
				return nil
			})
	}()
	var child context.Context
	select {
	case child = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child did not start")
	}
	cancelDriver()
	require.NoError(t, child.Err())
	require.Nil(t, runlife.FromContext(child))
	select {
	case <-done:
		t.Fatal("direct driver failed to drain accepted child")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct driver did not finish drain")
	}
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.CompletedRuns)
	require.Zero(t, stored.FailedRuns)
}

func TestBackfillFinalizesExactCommittedAdmissionFailureBeforeReleasingOwnership(t *testing.T) {
	b, j, bStore, rStore := backfillLifetimeFixture(t)
	// Another active row must never be adopted or finalized by resemblance.
	other := models.JobRun{ID: uuid.New(), JobID: j.ID, Status: string(runstore.StatusRunning)}
	require.NoError(t, rStore.DB().Create(&other).Error)
	injected := errors.New("post-insert child readback failed")
	var committedID uuid.UUID
	injectedRead := false
	finalizing := make(chan struct{}, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	const createCallback = "test:observe_backfill_child_commit"
	const queryCallback = "test:fail_backfill_child_readback"
	const updateCallback = "test:block_backfill_child_finalization"
	require.NoError(t, rStore.DB().Callback().Create().After("gorm:create").Register(createCallback, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*models.JobRun); ok && row.BackfillID != nil && *row.BackfillID == b.ID {
			committedID = row.ID
		}
	}))
	require.NoError(t, rStore.DB().Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
		// loadRun uses a joined Table("job_runs").First into an anonymous
		// projection; its parsed schema is not named JobRun. Match that exact
		// qualified-ID read, and assert it is outside the insertion transaction.
		if tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || committedID == uuid.Nil || injectedRead {
			return
		}
		where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
		if !ok {
			return
		}
		for _, condition := range where.Exprs {
			expr, ok := condition.(clause.Expr)
			if !ok || expr.SQL != "job_runs.id = ?" || len(expr.Vars) != 1 {
				continue
			}
			id, ok := expr.Vars[0].(uuid.UUID)
			if !ok || id != committedID {
				continue
			}
			_, insideTransaction := tx.Statement.ConnPool.(gorm.TxCommitter)
			require.False(t, insideTransaction, "fault must follow the committed insertion")
			injectedRead = true
			_ = tx.AddError(injected)
			return
		}
	}))
	require.NoError(t, rStore.DB().Callback().Update().Before("gorm:update").Register(updateCallback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "JobRun" {
			var committed models.JobRun
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).First(&committed, "id = ?", committedID).Error)
			require.Equal(t, string(runstore.StatusRunning), committed.Status, "fault followed a durable active insertion")
			finalizing <- struct{}{}
			<-finish
		}
	}))
	owner := runlife.New(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		owner.CloseAndCancel()
		unblock()
		<-done
		_ = rStore.DB().Callback().Create().Remove(createCallback)
		_ = rStore.DB().Callback().Query().Remove(queryCallback)
		_ = rStore.DB().Callback().Update().Remove(updateCallback)
	})
	var calls atomic.Int32
	schedule := mustParseCron(t, "0 * * * *")
	go func() {
		defer close(done)
		runBackfill(runlife.WithSupervisor(t.Context(), owner), b, j, schedule, time.UTC, bStore, rStore,
			func(context.Context, *models.Job, map[string]string) error { calls.Add(1); return nil })
	}()
	select {
	case <-finalizing:
	case <-time.After(5 * time.Second):
		t.Fatal("committed failure did not reach conditional finalization")
	}
	owner.CloseAndCancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded, "child remains reserved through terminal persistence")
	unblock()
	<-done
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	require.NoError(t, owner.Wait(waitCtx))
	require.True(t, injectedRead)
	require.NotEqual(t, uuid.Nil, committedID)
	require.Zero(t, calls.Load())
	var actual, untouched models.JobRun
	require.NoError(t, rStore.DB().First(&actual, "id = ?", committedID).Error)
	require.NoError(t, rStore.DB().First(&untouched, "id = ?", other.ID).Error)
	require.Equal(t, string(runstore.StatusFailed), actual.Status)
	require.Contains(t, actual.Error, injected.Error())
	require.NotNil(t, actual.CompletedAt)
	require.Equal(t, string(runstore.StatusRunning), untouched.Status)
	require.Nil(t, untouched.CompletedAt)
	var taskCount int64
	require.NoError(t, rStore.DB().Model(&models.TaskRun{}).Where("job_run_id = ?", actual.ID).Count(&taskCount).Error)
	require.Zero(t, taskCount)
	stored, err := bStore.Get(b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.FailedRuns)
}
