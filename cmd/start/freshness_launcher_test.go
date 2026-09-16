package start

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
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
	if attempts != len(derivedRunJobLookupBackoffs)+1 {
		t.Fatalf("job lookup attempts = %d, want %d (initial try plus every backoff)",
			attempts, len(derivedRunJobLookupBackoffs)+1)
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
