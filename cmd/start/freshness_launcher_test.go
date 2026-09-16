package start

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openLauncherTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared&_busy_timeout=5000"
	conn, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := conn.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := conn.AutoMigrate(models.All...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return conn
}

// seedRunningDerivedRun creates the row state the freshness evaluator leaves
// behind: an admitted, committed `running` job run with no task rows yet.
func seedRunningDerivedRun(t *testing.T, conn *gorm.DB) (*run.JobRun, uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	jobID := uuid.New()
	if err := conn.Create(&models.Job{
		ID:        jobID,
		Alias:     "derived-" + jobID.String(),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	runID := uuid.New()
	if err := conn.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    string(run.StatusRunning),
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create job run: %v", err)
	}
	return &run.JobRun{ID: runID, JobID: jobID, Status: run.StatusRunning}, jobID
}

func runStatus(t *testing.T, conn *gorm.DB, runID uuid.UUID) string {
	t.Helper()
	var row models.JobRun
	if err := conn.First(&row, "id = ?", runID).Error; err != nil {
		t.Fatalf("load job run: %v", err)
	}
	return row.Status
}

// TestLaunchDerivedRunFinalizesWhenJobLookupFails is the regression for the
// stranded-run hazard the freshness launcher itself could reintroduce. The
// evaluator commits the run and its `derived` audit BEFORE the launcher is
// called, and job.Run — whose aborted-resume finalizer would normally
// terminalize a run that never reached an engine — is never entered when the
// job lookup fails. Without an explicit finalization the run stays `running`
// with zero tasks: the evaluator then reports skipped_active_run forever and a
// maxRuns policy refuses every later arrival.
func TestLaunchDerivedRunFinalizesWhenJobLookupFails(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	lookupErr := errors.New("catalog unavailable")
	attempts := 0
	executed := false

	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(context.Context, uuid.UUID) (*models.Job, error) {
			attempts++
			return nil, lookupErr
		},
		func(context.Context, *models.Job, *run.JobRun) error {
			executed = true
			return nil
		},
	)

	if executed {
		t.Fatal("the executor must not run when the job cannot be loaded")
	}
	if attempts != len(derivedRunRetryBackoffs)+1 {
		t.Fatalf("job lookup attempts = %d, want %d (initial try plus every backoff)",
			attempts, len(derivedRunRetryBackoffs)+1)
	}
	if status := runStatus(t, conn, derived.ID); status != string(run.StatusFailed) {
		t.Fatalf("run status = %q, want %q: an unlaunchable admitted run must not stay running",
			status, run.StatusFailed)
	}

	var stored models.JobRun
	if err := conn.First(&stored, "id = ?", derived.ID).Error; err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if stored.Error == "" {
		t.Fatal("the finalized run must record why it could not be launched")
	}
	if stored.CompletedAt == nil {
		t.Fatal("the finalized run must record a completion time")
	}
}

// TestLaunchDerivedRunStopsRetryingOnRecordNotFound proves a deleted job is not
// retried on the transient-contention schedule: it is terminal immediately.
func TestLaunchDerivedRunStopsRetryingOnRecordNotFound(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	attempts := 0
	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(context.Context, uuid.UUID) (*models.Job, error) {
			attempts++
			return nil, gorm.ErrRecordNotFound
		},
		func(context.Context, *models.Job, *run.JobRun) error {
			t.Fatal("the executor must not run for a deleted job")
			return nil
		},
	)

	if attempts != 1 {
		t.Fatalf("job lookup attempts = %d, want 1 for a not-found job", attempts)
	}
	if status := runStatus(t, conn, derived.ID); status != string(run.StatusFailed) {
		t.Fatalf("run status = %q, want %q", status, run.StatusFailed)
	}
}

// TestFreshnessRunLauncherFencesCancellationDuringLookup is the regression for
// a run cancelled in the window between admission and job.Run's own cancel
// registration.
//
// The admitted run is already visible to CancelRun and to the concurrency
// `replace` admission, but job.Run's registration sits on the far side of the
// job-lookup retries. Without a kickoff-site registration the cancel reaches no
// registered context; job.Run then resolves the cancelled run without checking
// its status and RegisterTasks has no parent-status guard, so the local executor
// launches containers for a run the operator already cancelled.
func TestFreshnessRunLauncherFencesCancellationDuringLookup(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	release := make(chan struct{})
	// Buffered: the launcher's goroutine records that it entered the lookup
	// whether or not the test goroutine is already waiting.
	lookupEntered := make(chan struct{}, 1)
	executed := make(chan struct{}, 1)

	launcher := newFreshnessRunLauncher(
		store,
		func(_ context.Context, id uuid.UUID) (*models.Job, error) {
			select {
			case lookupEntered <- struct{}{}:
			default:
			}
			<-release
			var j models.Job
			if err := conn.First(&j, "id = ?", id).Error; err != nil {
				return nil, err
			}
			return &j, nil
		},
		func(context.Context, *models.Job, *run.JobRun) error {
			executed <- struct{}{}
			return nil
		},
	)

	launcher(context.Background(), derived)

	select {
	case <-lookupEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the launcher to start its job lookup")
	}

	// The launcher must already have registered the run's cancellation: this
	// returns the number of contexts it cancelled, so > 0 proves registration
	// happened BEFORE the lookup finished.
	if cancelled := job.CancelRunContexts(derived.ID); cancelled == 0 {
		close(release)
		t.Fatal("the launcher did not register the run's cancellation before looking up its job")
	}
	close(release)

	select {
	case <-executed:
		t.Fatal("a run cancelled during the job lookup must not execute its DAG")
	case <-time.After(time.Second):
	}
}

// TestLaunchDerivedRunSkipsTerminalRun covers the other half of the fence: a
// cancellation that landed BEFORE this launcher registered, or whose
// run_cancelled event the non-blocking bus dropped, is only visible in the row.
func TestLaunchDerivedRunSkipsTerminalRun(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	executed := false
	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(_ context.Context, id uuid.UUID) (*models.Job, error) {
			// The run is cancelled while the lookup is in flight.
			if err := conn.Model(&models.JobRun{}).
				Where("id = ?", derived.ID).
				Update("status", string(run.StatusCancelled)).Error; err != nil {
				return nil, err
			}
			var j models.Job
			if err := conn.First(&j, "id = ?", id).Error; err != nil {
				return nil, err
			}
			return &j, nil
		},
		func(context.Context, *models.Job, *run.JobRun) error {
			executed = true
			return nil
		},
	)

	if executed {
		t.Fatal("a run that reached a terminal status must not execute its DAG")
	}
	if status := runStatus(t, conn, derived.ID); status != string(run.StatusCancelled) {
		t.Fatalf("run status = %q, want %q: the fence must not rewrite a terminal run",
			status, run.StatusCancelled)
	}
}

// failJobRunReads makes the first n reads of job_runs fail, simulating
// transient database contention on the status fence.
func failJobRunReads(t *testing.T, conn *gorm.DB, n int) {
	t.Helper()
	const name = "test:fail_job_run_reads"
	remaining := n
	require.NoError(t, conn.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if remaining <= 0 {
			return
		}
		table := tx.Statement.Table
		if table == "" && tx.Statement.Schema != nil {
			table = tx.Statement.Schema.Table
		}
		if table != "job_runs" {
			return
		}
		remaining--
		tx.AddError(errors.New("database is locked"))
	}))
	t.Cleanup(func() { _ = conn.Callback().Query().Remove(name) })
}

// TestLaunchDerivedRunFenceRetriesTransientStatusRead is the regression for a
// fence that waved a run through because it could not read its status.
//
// The dangerous combination is a cancellation that landed BEFORE this launcher
// registered — so ctx.Err() is nil and the row is the only evidence — together
// with a transient read failure. Treating "unreadable" as "still active"
// executed the cancelled run: job.Run re-reads it but does not reject a
// cancelled status, and RegisterTasks has no parent-status guard.
func TestLaunchDerivedRunFenceRetriesTransientStatusRead(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	// Cancelled before this launcher ever registered: only the row knows.
	require.NoError(t, conn.Model(&models.JobRun{}).
		Where("id = ?", derived.ID).
		Update("status", string(run.StatusCancelled)).Error)

	var j models.Job
	require.NoError(t, conn.First(&j, "id = ?", derived.JobID).Error)

	failJobRunReads(t, conn, 1)

	executed := false
	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(context.Context, uuid.UUID) (*models.Job, error) { return &j, nil },
		func(context.Context, *models.Job, *run.JobRun) error {
			executed = true
			return nil
		},
	)

	require.False(t, executed,
		"a cancelled run must not execute just because the first status read failed")
	require.Equal(t, string(run.StatusCancelled), runStatus(t, conn, derived.ID),
		"the fence must not rewrite a terminal run")
}

// TestLaunchDerivedRunFinalizesWhenFenceCannotResolveStatus proves the fence
// fails CLOSED: when the status can never be established it neither executes
// nor strands the run.
func TestLaunchDerivedRunFinalizesWhenFenceCannotResolveStatus(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, _ := seedRunningDerivedRun(t, conn)

	var j models.Job
	require.NoError(t, conn.First(&j, "id = ?", derived.JobID).Error)

	// Exhaust every fence attempt, then let the finalization read through.
	failJobRunReads(t, conn, len(derivedRunRetryBackoffs)+1)

	executed := false
	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(context.Context, uuid.UUID) (*models.Job, error) { return &j, nil },
		func(context.Context, *models.Job, *run.JobRun) error {
			executed = true
			return nil
		},
	)

	require.False(t, executed, "an unconfirmed run status must not dispatch a DAG")
	require.Equal(t, string(run.StatusFailed), runStatus(t, conn, derived.ID),
		"an unlaunchable admitted run must be finalized, not stranded running")
}

// TestLaunchDerivedRunRetriesTransientLookupFailure proves a run is not thrown
// away for a transient read error: the lookup is retried and the run executes.
func TestLaunchDerivedRunRetriesTransientLookupFailure(t *testing.T) {
	conn := openLauncherTestDB(t)
	store := run.NewStore(conn)
	derived, jobID := seedRunningDerivedRun(t, conn)

	attempts := 0
	var executedJob *models.Job
	launchDerivedRun(
		context.Background(),
		store,
		derived,
		func(_ context.Context, id uuid.UUID) (*models.Job, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("database is locked")
			}
			var j models.Job
			if err := conn.First(&j, "id = ?", id).Error; err != nil {
				return nil, err
			}
			return &j, nil
		},
		func(_ context.Context, j *models.Job, _ *run.JobRun) error {
			executedJob = j
			return nil
		},
	)

	if attempts != 2 {
		t.Fatalf("job lookup attempts = %d, want 2", attempts)
	}
	if executedJob == nil || executedJob.ID != jobID {
		t.Fatalf("executor received %v, want job %s", executedJob, jobID)
	}
	if status := runStatus(t, conn, derived.ID); status != string(run.StatusRunning) {
		t.Fatalf("run status = %q, want %q: a launched run is finalized by job.Run, not the launcher",
			status, run.StatusRunning)
	}
}
