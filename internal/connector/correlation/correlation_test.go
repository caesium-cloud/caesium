package correlation

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func correlationJob(t *testing.T, db *gorm.DB, alias string, queue bool) models.Job {
	t.Helper()
	id := uuid.New()
	trigger := models.Trigger{ID: uuid.New(), Alias: alias + "-trigger", Type: models.TriggerTypeCron, Configuration: `{"cron":"0 0 1 1 *","timezone":"UTC"}`}
	require.NoError(t, db.Create(&trigger).Error)
	job := models.Job{ID: id, Alias: alias, TriggerID: trigger.ID}
	if queue {
		job.Concurrency = datatypes.JSON(`{"maxRuns":1,"strategy":"queue"}`)
	}
	require.NoError(t, db.Create(&job).Error)
	return job
}
func correlationSource(t *testing.T, catalog *Store) ActivityEvidence {
	t.Helper()
	ref, err := connector.NewExecutionReference("primary", map[string]string{"process": "p-1", "attempt": "r-1"})
	require.NoError(t, err)
	_, err = connector.NewStore(catalog.db).RecordSnapshot(t.Context(), connector.Observation{Execution: ref, Generation: 1, SourceEventID: "initial", NativeStatus: "running", DisplayStatus: connector.DisplayRunning, Availability: connector.AvailabilityAvailable, Completeness: connector.CompletenessComplete, SourceKind: connector.SourceDirect, ObservedAt: time.Now().UTC()})
	require.NoError(t, err)
	return ActivityEvidence{Execution: ref, WorkflowType: "publication", ActivityID: "a-1", ActivityType: "caesium.start", IdempotencyKey: "r-1/a-1", SourceEventID: 5, SourceEventType: "ACTIVITY_STARTED", ScheduledEventID: 4, Attempt: 1}
}
func correlationBinding(aliases ...string) connector.Binding {
	return connector.Binding{Name: "publication", Version: "1", ActivityAllowlist: []connector.ActivityJob{{ActivityType: "caesium.start", Jobs: aliases}}}
}

func TestCorrelationVerifiesRealAdmissionAndCoalescesConcurrentRetries(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	runs := run.NewStore(db)
	catalog := NewStore(db)
	job := correlationJob(t, db, "job-a", false)
	source := correlationSource(t, catalog)
	started, err := runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartParams(map[string]string{"not-reconstructed": "secret"}), run.WithStartIdempotencyKey(source.IdempotencyKey))
	require.NoError(t, err)
	verifier := NewCorrelator(runs, catalog)
	source.Hint = &connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: source.IdempotencyKey, Outcome: "created", RunID: started.RunID.String()}
	const readers = 8
	results := make(chan models.ExternalExecutionCorrelation, readers)
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			r, e := verifier.Verify(context.Background(), correlationBinding(job.Alias), source)
			results <- r
			errs <- e
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	var first models.ExternalExecutionCorrelation
	for r := range results {
		if first.ID == uuid.Nil {
			first = r
		}
		require.Equal(t, first.ID, r.ID)
		require.Equal(t, started.RunID, r.RunID)
	}
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionCorrelation{}))
	require.Equal(t, int64(1), countModel(t, db, &models.JobRun{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecution{}))
	writes := 0
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("correlation-no-create", func(*gorm.DB) { writes++ }))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("correlation-no-update", func(*gorm.DB) { writes++ }))
	again, err := verifier.Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	require.Equal(t, first.VerifiedAt, again.VerifiedAt)
	require.Zero(t, writes, "unchanged evidence must issue no write")
	reverse, err := catalog.RunCorrelations(t.Context(), job.ID, *started.RunID)
	require.NoError(t, err)
	require.Len(t, reverse, 1)
	require.Equal(t, first.ID, reverse[0].ID)
}

func TestCorrelationPreservesQueueIdentityAcrossPromotionAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "correlation.db")
	db := openSQLiteFile(t, path)
	require.NoError(t, db.AutoMigrate(models.All...))
	runs := run.NewStore(db)
	catalog := NewStore(db)
	job := correlationJob(t, db, "queue-job", true)
	source := correlationSource(t, catalog)
	holder, err := runs.StartWithResult(t.Context(), job.ID, nil)
	require.NoError(t, err)
	queued, err := runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartIdempotencyKey(source.IdempotencyKey))
	require.NoError(t, err)
	require.Equal(t, run.StartOutcomeQueued, queued.Outcome)
	source.Hint = &connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: source.IdempotencyKey, Outcome: "queued", QueueID: queued.QueueID.String()}
	before, err := NewCorrelator(runs, catalog).Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", *holder.RunID).Update("status", "succeeded").Error)
	pending, err := runs.DequeueNextRun(t.Context(), job.ID, "correlation-dequeuer")
	require.NoError(t, err)
	require.NotNil(t, pending)
	promoted, err := runs.StartQueuedRun(t.Context(), pending)
	require.NoError(t, err)
	require.NoError(t, runs.DeleteQueuedRun(t.Context(), pending))
	// An old queued heartbeat is no longer an exact match. Refusal preserves the
	// established row, and metadata alone can verify the promoted admission.
	_, err = NewCorrelator(runs, catalog).Verify(t.Context(), correlationBinding(job.Alias), source)
	require.ErrorIs(t, err, ErrUnverifiedCorrelation)
	source.Hint = nil
	after, err := NewCorrelator(runs, catalog).Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID)
	require.Equal(t, before.AdmissionID, after.AdmissionID)
	require.Equal(t, before.QueueID, after.QueueID)
	require.Equal(t, promoted.ID, *after.RunID)
	require.Equal(t, "created", after.Outcome)
	closeSQLite(t, db)
	reopened := openSQLiteFile(t, path)
	links, err := NewStore(reopened).Correlations(t.Context(), source.Execution)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, after.ID, links[0].ID)
	require.Equal(t, promoted.ID, *links[0].RunID)
	same, err := NewCorrelator(run.NewStore(reopened), NewStore(reopened)).Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	require.Equal(t, after.ID, same.ID)
	require.Equal(t, after.VerifiedAt, same.VerifiedAt)
}

func TestCorrelationRefusesUntrustedHintsAndAmbiguousAdmissions(t *testing.T) {
	cases := []string{"missing", "wrong_job_allowlist", "ambiguous", "other_workflow_key", "unrelated_activity", "wrong_workflow_type", "missing_event", "wrong_job_hint", "wrong_key_hint", "wrong_outcome", "wrong_run", "wrong_queue", "wrong_version", "missing_hint_field", "oversized_allowlist"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			runs := run.NewStore(db)
			catalog := NewStore(db)
			job := correlationJob(t, db, "first", false)
			other := correlationJob(t, db, "other", false)
			source := correlationSource(t, catalog)
			binding := correlationBinding(job.Alias)
			key := source.IdempotencyKey
			if name == "other_workflow_key" {
				key = "different-workflow/a-1"
			}
			var started run.StartResult
			var err error
			if name != "missing" {
				started, err = runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartIdempotencyKey(key))
				require.NoError(t, err)
			}
			source.Hint = &connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: source.IdempotencyKey, Outcome: "created"}
			if started.RunID != nil {
				source.Hint.RunID = started.RunID.String()
			}
			switch name {
			case "wrong_job_allowlist":
				binding = correlationBinding(other.Alias)
			case "ambiguous":
				_, err = runs.StartWithResult(t.Context(), other.ID, nil, run.WithStartIdempotencyKey(key))
				require.NoError(t, err)
				binding = correlationBinding(job.Alias, other.Alias)
			case "unrelated_activity":
				source.ActivityType = "unrelated"
			case "wrong_workflow_type":
				source.WorkflowType = "unrelated"
			case "missing_event":
				source.SourceEventID = 0
			case "wrong_job_hint":
				source.Hint.JobID = other.ID.String()
			case "wrong_key_hint":
				source.Hint.IdempotencyKey = "other/a-1"
			case "wrong_outcome":
				source.Hint.Outcome = "queued"
			case "wrong_run":
				source.Hint.RunID = uuid.NewString()
			case "wrong_queue":
				source.Hint.QueueID = uuid.NewString()
			case "wrong_version":
				source.Hint.Version = "v2"
			case "missing_hint_field":
				source.Hint.JobID = ""
			case "oversized_allowlist":
				for i := range MaxCorrelationJobs {
					binding.ActivityAllowlist[0].Jobs = append(binding.ActivityAllowlist[0].Jobs, fmt.Sprint("job", i))
				}
			}
			_, err = NewCorrelator(runs, catalog).Verify(t.Context(), binding, source)
			require.ErrorIs(t, err, ErrUnverifiedCorrelation)
			require.Zero(t, countModel(t, db, &models.ExternalExecutionCorrelation{}))
		})
	}
}

func TestCorrelationOldEvidenceCannotRegressOrRepointLink(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	runs := run.NewStore(db)
	catalog := NewStore(db)
	job := correlationJob(t, db, "job", false)
	source := correlationSource(t, catalog)
	_, err := runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartIdempotencyKey(source.IdempotencyKey))
	require.NoError(t, err)
	verifier := NewCorrelator(runs, catalog)
	first, err := verifier.Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	source.SourceEventID--
	_, err = verifier.Verify(t.Context(), correlationBinding(job.Alias), source)
	require.ErrorIs(t, err, connector.ErrStaleGeneration)
	source.SourceEventID += 2
	source.Attempt = 2
	next, err := verifier.Verify(t.Context(), correlationBinding(job.Alias), source)
	require.NoError(t, err)
	require.Equal(t, first.ID, next.ID)
	require.True(t, !next.VerifiedAt.Before(first.VerifiedAt))
	source.Attempt = 1
	_, err = verifier.Verify(t.Context(), correlationBinding(job.Alias), source)
	require.ErrorIs(t, err, connector.ErrStaleGeneration)
	links, err := catalog.Correlations(t.Context(), source.Execution)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, next.EvidenceDigest, links[0].EvidenceDigest)
	require.Equal(t, next.VerifiedAt, links[0].VerifiedAt)
	var count int64
	require.NoError(t, db.Model(&models.ExternalExecutionCorrelation{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
}

func TestCorrelationHonorsConfiguredEvidenceBudget(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	runs := run.NewStore(db)
	catalog := NewStore(db)
	job := correlationJob(t, db, "bounded", false)
	source := correlationSource(t, catalog)
	_, err := runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartIdempotencyKey(source.IdempotencyKey))
	require.NoError(t, err)
	_, err = NewCorrelator(runs, catalog).Verify(t.Context(), correlationBinding(job.Alias), source, connector.WriteBounds{MaxMetadataBytes: 100})
	require.ErrorIs(t, err, connector.ErrPayloadTooLarge)
	require.Zero(t, countModel(t, db, &models.ExternalExecutionCorrelation{}))
}

func countModel(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(model).Count(&count).Error)
	return count
}
func openSQLiteFile(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
func closeSQLite(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
}
