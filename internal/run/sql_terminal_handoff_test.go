package run

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type sqlTerminalRoute struct {
	name   string
	status TaskStatus
	apply  func(*Store, uuid.UUID, uuid.UUID) error
}

func sqlTerminalRoutes() []sqlTerminalRoute {
	return []sqlTerminalRoute{
		{"success", TaskStatusSucceeded, func(s *Store, runID, taskID uuid.UUID) error {
			return s.CompleteTaskClaimed(runID, taskID, "success", "sql-worker", nil, nil)
		}},
		{"cache", TaskStatusCached, func(s *Store, runID, taskID uuid.UUID) error {
			return s.CacheHitTaskClaimed(runID, taskID, CacheHitSource{}, "success", "sql-worker", nil, nil)
		}},
		{"failure", TaskStatusFailed, func(s *Store, runID, taskID uuid.UUID) error {
			return s.FailTaskClaimed(runID, taskID, errors.New("sql failure"), "sql-worker")
		}},
		{"result failure", TaskStatusFailed, func(s *Store, runID, taskID uuid.UUID) error {
			return s.CompleteTaskClaimed(runID, taskID, "failure", "sql-worker", nil, nil)
		}},
	}
}

func TestSQLTerminalSequencesAreVisibleAndUnique(t *testing.T) {
	for _, route := range sqlTerminalRoutes() {
		t.Run(route.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			runID, taskA, taskB := seedTwoTaskRun(t, db, store, "sql-worker")
			// Independent roots let both failure routes finish without skipping
			// the second row before its own terminal write.
			require.NoError(t, db.Where("from_task_id = ?", taskA).Delete(&models.TaskEdge{}).Error)
			require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).
				Updates(map[string]any{"claimed_by": "sql-worker", "outstanding_predecessors": 0}).Error)
			for _, taskID := range []uuid.UUID{taskA, taskB} {
				require.NoError(t, route.apply(store, runID, taskID))
			}
			tail, err := store.TerminalTaskRunsSince(runID, 0)
			require.NoError(t, err)
			require.Len(t, tail, 2, "every primary SQL terminal route must be replayable by a new owner")
			for i, row := range tail {
				require.Equal(t, route.status, TaskStatus(row.Status))
				require.Equal(t, int64(i+1), row.TerminalSequence, "distinct rows must have distinct dense sequences")
			}
		})
	}
}

func TestSQLTerminalFanOutInstancesEachHaveAReplaySequence(t *testing.T) {
	for _, route := range sqlTerminalRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newFanOutFixture(t, &jobdefschema.FanOut{
				From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
			})
			_, err := f.store.CompleteTaskWithPartitions(f.runID, f.producer.ID, "success", nil, nil, strParts("a", "b", "c"))
			require.NoError(t, err)
			instances := f.instances(t)
			require.Len(t, instances, 3)
			require.NoError(t, f.db.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
				Update("claimed_by", "sql-worker").Error)
			for _, instance := range instances {
				require.NoError(t, route.apply(f.store, f.runID, instance.ID))
			}
			tail, err := f.store.TerminalTaskRunsSince(f.runID, 0)
			require.NoError(t, err)
			require.Len(t, tail, 4, "producer and all three concrete instances must be replayable")
			seen := make(map[uuid.UUID]bool)
			for i, row := range tail {
				require.Equal(t, int64(i+1), row.TerminalSequence)
				if row.TaskID == f.consumer.ID {
					require.Equal(t, string(route.status), row.Status)
					seen[row.ID] = true
				}
			}
			for _, instance := range instances {
				require.True(t, seen[instance.ID], "partition %s's primary key must appear in the replay tail", instance.PartitionValue)
			}
		})
	}
}

func TestSQLTerminalSequenceRollsBackWithFailedWrite(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskA, _ := seedTwoTaskRun(t, db, store, "sql-worker")
	writeFailure := errors.New("terminal write rolled back")
	callback := "test:rollback_sql_terminal"
	require.NoError(t, db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "task_runs" {
			tx.AddError(writeFailure)
		}
	}))
	err := store.CompleteTaskClaimed(runID, taskA, "success", "sql-worker", nil, nil)
	require.NoError(t, db.Callback().Update().Remove(callback))
	require.ErrorIs(t, err, writeFailure)
	tail, err := store.TerminalTaskRunsSince(runID, 0)
	require.NoError(t, err)
	require.Empty(t, tail)
	require.NoError(t, store.CompleteTaskClaimed(runID, taskA, "success", "sql-worker", nil, nil))
	tail, err = store.TerminalTaskRunsSince(runID, 0)
	require.NoError(t, err)
	require.Len(t, tail, 1)
	require.Equal(t, int64(1), tail[0].TerminalSequence, "rollback must not consume the first durable sequence")
}

func TestSQLTerminalMemoryLeaseFencePreservesSQLMode(t *testing.T) {
	for _, route := range sqlTerminalRoutes() {
		for _, memory := range []bool{false, true} {
			for _, leaseState := range []string{"absent", "live", "expired"} {
				name := route.name + "/" + leaseState
				if memory {
					name += "/memory"
				} else {
					name += "/sql"
				}
				t.Run(name, func(t *testing.T) {
					db := testutil.OpenTestDB(t)
					t.Cleanup(func() { testutil.CloseDB(db) })
					store := NewStore(db)
					store.ownerInMemory = memory
					runID, taskA, _ := seedTwoTaskRun(t, db, store, "sql-worker")
					if leaseState != "absent" {
						expires := time.Now().Add(time.Hour)
						if leaseState == "expired" {
							expires = time.Now().Add(-time.Hour)
						}
						require.NoError(t, db.Create(&models.RunLease{
							RunID: runID.String(), OwnerNode: "owner", Generation: 1,
							AcquiredAt: time.Now(), LeaseExpiresAt: expires,
						}).Error)
					}
					err := route.apply(store, runID, taskA)
					var row models.TaskRun
					require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskA).First(&row).Error)
					if memory && leaseState != "absent" {
						require.ErrorIs(t, err, ErrTaskClaimMismatch)
						require.Equal(t, string(TaskStatusPending), row.Status)
						require.Zero(t, row.TerminalSequence, "a rejected handoff must not consume a durable sequence")
					} else {
						require.NoError(t, err)
						require.Equal(t, string(route.status), row.Status)
						require.Equal(t, int64(1), row.TerminalSequence)
					}
				})
			}
		}
	}
}

func TestOwnerMemoryAdvancementModeKeepsLocalSQLCompletion(t *testing.T) {
	require.False(t, ownerMemoryAdvancementMode(env.Environment{ExecutionMode: "local", RunOwnerEnabled: true, RunOwnerInMemory: true}))
	require.False(t, ownerMemoryAdvancementMode(env.Environment{ExecutionMode: "distributed", RunOwnerEnabled: true}))
	require.False(t, ownerMemoryAdvancementMode(env.Environment{ExecutionMode: "distributed", RunOwnerInMemory: true}), "a memory flag without owner coordination must not fence SQL")
	require.True(t, ownerMemoryAdvancementMode(env.Environment{ExecutionMode: "distributed", RunOwnerEnabled: true, RunOwnerInMemory: true}))
	require.True(t, ownerMemoryAdvancementMode(env.Environment{ExecutionMode: " Distributed ", RunOwnerEnabled: true, RunOwnerInMemory: true}), "match the normalized runtime mode")
}

// This race must use PostgreSQL: SQLite's single writer would hide a missing
// JobRun lock on the lease INSERT. The SQL completion is paused after its task
// UPDATE while holding JobRun. Without AcquireLease's matching lock, the owner
// lease commits early and recovery can publish before this terminal sequence.
func TestPostgresSQLCompletionHandoffPrecedesMemoryOwnerRecovery(t *testing.T) {
	db := openDeadlinePostgres(t)
	require.NoError(t, db.AutoMigrate(&models.RunLease{}))
	// RunCheckpoint's SQLite blob tag cannot be AutoMigrated on PostgreSQL.
	// Keep this opt-in race fixture isolated and use PostgreSQL's bytea type.
	require.NoError(t, db.Exec(`CREATE TABLE run_checkpoints (
		run_id text NOT NULL, sequence_high bigint NOT NULL,
		owner_generation bigint NOT NULL, state_blob bytea NOT NULL,
		is_incremental boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL,
		PRIMARY KEY (run_id, sequence_high)
	)`).Error)
	store := NewStore(db)
	store.ownerInMemory = true
	runID, taskA, taskB := seedTwoTaskRun(t, db, store, "sql-worker")
	ls := NewLeaseStore(db)
	ls.ownerInMemory = true

	written := make(chan struct{})
	releaseWrite := make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrite) }) }
	callback := "test:sql_owner_handoff_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "task_runs" ||
			updates["status"] != string(TaskStatusSucceeded) || updates["result"] != "success" {
			return
		}
		holdOnce.Do(func() { close(written); <-releaseWrite })
	}))
	defer func() { require.NoError(t, db.Callback().Update().Remove(callback)) }()
	defer release()

	completionDone := make(chan error, 1)
	go func() { completionDone <- store.CompleteTaskClaimed(runID, taskA, "success", "sql-worker", nil, nil) }()
	select {
	case <-written:
	case <-time.After(postgresDeadlineWait):
		t.Fatal("SQL completion did not reach its locked terminal write")
	}
	leaseDone := make(chan error, 1)
	go func() { _, err := ls.AcquireLease(t.Context(), runID, "owner", time.Hour); leaseDone <- err }()
	var earlyLeaseErr error
	leaseCommittedEarly := false
	select {
	case earlyLeaseErr = <-leaseDone:
		leaseCommittedEarly = true
	case <-time.After(250 * time.Millisecond):
	}
	release()
	select {
	case err := <-completionDone:
		require.NoError(t, err)
	case <-time.After(postgresDeadlineWait):
		t.Fatal("SQL completion did not finish after release")
	}
	if leaseCommittedEarly {
		t.Fatalf("memory lease committed while SQL held JobRun: %v", earlyLeaseErr)
	}
	select {
	case err := <-leaseDone:
		require.NoError(t, err)
	case <-time.After(postgresDeadlineWait):
		t.Fatal("memory lease did not finish after SQL commit")
	}

	mgr := NewOwnerManager(store, CheckpointConfig{})
	res, err := mgr.Recover(runID, 1)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{taskB}, res.Ready, "recovery must replay the pre-lease SQL completion, never redispatch it")
	require.Equal(t, int64(1), res.MaxSequence)
	require.NoError(t, store.ClaimTaskForDispatch(runID, taskB, "owner", 1, time.Hour, true))
	mgr.MarkDispatched(runID, taskB, "owner", 1, time.Now().Add(time.Hour).UnixMilli())
	completed, err := mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "owner", nil, nil)
	require.NoError(t, err)
	require.True(t, completed.Complete)
	tail, err := store.TerminalTaskRunsSince(runID, 0)
	require.NoError(t, err)
	require.Len(t, tail, 2)
	require.Equal(t, int64(1), tail[0].TerminalSequence)
	require.Equal(t, int64(2), tail[1].TerminalSequence, "new owner completion must not reuse the SQL sequence on a distinct row")
}
