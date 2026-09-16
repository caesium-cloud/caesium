package run

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const postgresDeadlineWait = 3 * time.Second

// TestPostgresRunTimeoutSerializesEveryTerminalTaskWriter exercises the actual
// PostgreSQL row lock used to order a run timeout before a racing terminal task
// write. It is opt-in because the ordinary unit suite has no PostgreSQL server.
//
// Each case pauses CompleteIfActive immediately after its JobRun UPDATE. At
// that point PostgreSQL is holding the JobRun row lock but the timeout
// transaction has not touched TaskRuns. A terminal writer without the matching
// JobRun-first lock can therefore commit a late result while the timeout is
// paused; a correctly ordered writer must remain blocked until timeout commits.
func TestPostgresRunTimeoutSerializesEveryTerminalTaskWriter(t *testing.T) {
	db := openDeadlinePostgres(t)

	type completionCase struct {
		name     string
		claimed  bool
		complete func(*Store, postgresDeadlineFixture) error
	}
	parts := []pkgtask.Partition{{Key: "late-a"}, {Key: "late-b"}}
	cases := []completionCase{
		{
			name:    "claimed success",
			claimed: true,
			complete: func(store *Store, f postgresDeadlineFixture) error {
				return store.CompleteTaskClaimedWithPartitions(
					f.runID, f.producerRun.ID, "success", f.producerRun.ClaimedBy,
					map[string]string{"late": "claimed"}, nil, parts,
				)
			},
		},
		{
			name:    "claimed cache",
			claimed: true,
			complete: func(store *Store, f postgresDeadlineFixture) error {
				return store.CacheHitTaskClaimedWithPartitions(
					f.runID, f.producerRun.ID,
					CacheHitSource{RunID: f.runID, CreatedAt: time.Now().UTC()},
					"success", f.producerRun.ClaimedBy,
					map[string]string{"late": "cache"}, nil, parts,
				)
			},
		},
		{
			name:    "owner completion",
			claimed: true,
			complete: func(store *Store, f postgresDeadlineFixture) error {
				expansion := &FanOutExpansion{
					ProducerTaskID: f.producer.ID,
					Partitions:     parts,
					Groups: []ExpandedGroup{{
						TaskID:   f.consumer.ID,
						TaskName: f.consumer.Name,
						Instances: []ExpandedInstance{
							{TaskRunID: uuid.New(), TaskID: f.consumer.ID, PartitionIndex: 0, Partition: parts[0]},
							{TaskRunID: uuid.New(), TaskID: f.consumer.ID, PartitionIndex: 1, Partition: parts[1]},
						},
					}},
				}
				return store.CompleteTaskOwner(
					f.runID, f.producerRun.ID, TaskStatusSucceeded, "success", "",
					f.producerRun.ClaimedBy, map[string]string{"late": "owner"}, nil,
					1, 1, nil, expansion,
				)
			},
		},
		{
			name: "unclaimed local completion",
			complete: func(store *Store, f postgresDeadlineFixture) error {
				_, err := store.CompleteTaskWithPartitions(
					f.runID, f.producer.ID, "success",
					map[string]string{"late": "local"}, nil, parts,
				)
				return err
			},
		},
		{
			name: "unclaimed local failure",
			complete: func(store *Store, f postgresDeadlineFixture) error {
				return store.FailTask(f.runID, f.producerRun.ID, errors.New("late local failure"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := seedPostgresDeadlineFixture(t, db, tc.name)
			store := NewStore(db)
			timeoutFailure := NewRunDeadlineError(time.Second)
			timeoutMessage := timeoutFailure.Error()

			timeoutHoldingLock := make(chan struct{})
			releaseTimeout := make(chan struct{})
			var (
				holdOnce    sync.Once
				releaseOnce sync.Once
			)
			release := func() { releaseOnce.Do(func() { close(releaseTimeout) }) }
			callbackName := "test:hold_postgres_run_timeout_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			require.NoError(t, db.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "job_runs" ||
					!postgresUpdateCarriesError(tx, timeoutMessage) {
					return
				}
				holdOnce.Do(func() {
					close(timeoutHoldingLock)
					<-releaseTimeout
				})
			}))
			defer func() { require.NoError(t, db.Callback().Update().Remove(callbackName)) }()
			// Registered after callback removal so LIFO cleanup releases a blocked
			// callback before removing it on any failing path.
			defer release()

			type timeoutResult struct {
				finalized bool
				err       error
			}
			timeoutDone := make(chan timeoutResult, 1)
			go func() {
				finalized, err := store.CompleteIfActive(fixture.runID, timeoutFailure)
				timeoutDone <- timeoutResult{finalized: finalized, err: err}
			}()

			select {
			case <-timeoutHoldingLock:
			case <-time.After(postgresDeadlineWait):
				release()
				t.Fatal("timeout transaction did not reach its locked JobRun write")
			}

			completionDone := make(chan error, 1)
			go func() { completionDone <- tc.complete(store, fixture) }()

			var completedBeforeRelease bool
			var earlyCompletionErr error
			select {
			case earlyCompletionErr = <-completionDone:
				completedBeforeRelease = true
			case <-time.After(250 * time.Millisecond):
				// Expected: SELECT ... FOR UPDATE is waiting on CompleteIfActive's
				// uncommitted JobRun status write.
			}

			release()
			var timeoutResultValue timeoutResult
			select {
			case timeoutResultValue = <-timeoutDone:
			case <-time.After(postgresDeadlineWait):
				t.Fatal("timeout transaction did not finish after release")
			}

			completionErr := earlyCompletionErr
			if !completedBeforeRelease {
				select {
				case completionErr = <-completionDone:
				case <-time.After(postgresDeadlineWait):
					t.Fatal("terminal writer did not finish after timeout committed")
				}
			}

			if completedBeforeRelease {
				t.Fatalf("terminal writer committed while the timeout held the JobRun lock: %v", earlyCompletionErr)
			}
			require.NoError(t, timeoutResultValue.err)
			require.True(t, timeoutResultValue.finalized)
			if tc.claimed {
				require.ErrorIs(t, completionErr, ErrTaskClaimMismatch)
			} else {
				require.NoError(t, completionErr)
			}

			assertPostgresDeadlineWon(t, db, fixture)
		})
	}
}

type postgresDeadlineFixture struct {
	runID       uuid.UUID
	producer    *models.Task
	consumer    *models.Task
	producerRun models.TaskRun
}

func seedPostgresDeadlineFixture(t *testing.T, db *gorm.DB, caseName string) postgresDeadlineFixture {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID: triggerID, Alias: "postgres-deadline-trigger", Type: models.TriggerTypeCron,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID: jobID, TriggerID: triggerID,
		Alias:     "postgres-deadline-" + strings.ReplaceAll(caseName, " ", "-"),
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	store := NewStore(db)
	runRecord := &models.JobRun{
		ID: uuid.New(), JobID: jobID, TriggerID: triggerID,
		Status: string(StatusRunning), StartedAt: now, TimeoutStartedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(runRecord).Error)

	atomModel := &models.Atom{
		ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23",
		Command: `["true"]`, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(atomModel).Error)
	producer := &models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "discover",
		Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
		CreatedAt: now, UpdatedAt: now,
	}
	encodedFanOut := datatypes.JSON([]byte(`{"from":"discover","maxPartitions":4}`))
	consumer := &models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "process",
		Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
		FanOutConfig: encodedFanOut, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(producer).Error)
	require.NoError(t, db.Create(consumer).Error)
	require.NoError(t, db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: jobID, FromTaskID: producer.ID, ToTaskID: consumer.ID,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: producer, Atom: atomModel, OutstandingPredecessors: 0},
		{Task: consumer, Atom: atomModel, OutstandingPredecessors: 1},
	}))

	producerRun, err := loadUniqueTaskRun(db, runRecord.ID, producer.ID)
	require.NoError(t, err)
	claimedAt := time.Now().UTC()
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", producerRun.ID).Updates(map[string]any{
		"status":        string(TaskStatusRunning),
		"claimed_by":    "postgres-race-worker",
		"claim_attempt": 1,
		"started_at":    claimedAt,
		"runtime_id":    "late-runtime",
	}).Error)
	require.NoError(t, db.First(producerRun, "id = ?", producerRun.ID).Error)

	return postgresDeadlineFixture{
		runID: runRecord.ID, producer: producer, consumer: consumer, producerRun: *producerRun,
	}
}

func assertPostgresDeadlineWon(t *testing.T, db *gorm.DB, f postgresDeadlineFixture) {
	t.Helper()
	var runRecord models.JobRun
	require.NoError(t, db.First(&runRecord, "id = ?", f.runID).Error)
	require.Equal(t, string(StatusFailed), runRecord.Status)
	require.Contains(t, runRecord.Error, "run timed out after 1s")

	var producer models.TaskRun
	require.NoError(t, db.First(&producer, "id = ?", f.producerRun.ID).Error)
	require.Equal(t, string(TaskStatusFailed), producer.Status)
	require.Contains(t, producer.Error, "run timed out after 1s")
	require.Empty(t, producer.Output, "late completion output must not persist")
	require.False(t, producer.CacheHit, "late cache completion must not persist")
	require.Nil(t, producer.CacheOriginRunID)
	require.Empty(t, producer.Partitions, "late producer fan-out must not persist")

	var consumers []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Order("partition_index ASC").Find(&consumers).Error)
	require.Len(t, consumers, 1, "late completion must not expand the fan-out group")
	require.Equal(t, 0, consumers[0].PartitionCount)
	require.Equal(t, string(TaskStatusFailed), consumers[0].Status)
}

func postgresUpdateCarriesError(tx *gorm.DB, expected string) bool {
	updates, ok := tx.Statement.Dest.(map[string]any)
	if !ok {
		return false
	}
	message, ok := updates["error"].(string)
	return ok && message == expected
}

func openDeadlinePostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CAESIUM_TEST_POSTGRES_DSN is not set; PostgreSQL lock test is opt-in")
	}

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schema := "caesium_deadline_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	schemaCreated := false
	closeScoped := func() error { return nil }
	t.Cleanup(func() {
		require.NoError(t, closeScoped())
		if schemaCreated {
			require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		}
		require.NoError(t, adminSQL.Close())
	})
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	schemaCreated = true

	scopedDSN, err := postgresDSNWithSearchPath(dsn, schema)
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	closeScoped = sqlDB.Close

	var currentSchema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&currentSchema).Error)
	require.Equal(t, schema, currentSchema, "test connection must be isolated to its generated schema")
	require.NoError(t, db.AutoMigrate(
		&models.Atom{},
		&models.Trigger{},
		&models.Job{},
		&models.Backfill{},
		&models.Task{},
		&models.TaskEdge{},
		&models.Callback{},
		&models.JobRun{},
		&models.TaskRun{},
		&models.CallbackRun{},
		&models.ExecutionEvent{},
	))
	return db
}

func postgresDSNWithSearchPath(dsn, schema string) (string, error) {
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse PostgreSQL test DSN: %w", err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	return strings.TrimSpace(dsn) + " search_path=" + schema, nil
}
