package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func isExecutionInputRows(tx *gorm.DB) bool {
	_, rows := tx.Statement.Dest.(*[]models.TaskRun)
	return rows && slices.Contains(tx.Statement.Selects, "output")
}

func countWorkerInputReads(t *testing.T, db *gorm.DB) *int {
	t.Helper()
	reads := new(int)
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:count_worker_inputs", func(tx *gorm.DB) {
		if isExecutionInputRows(tx) {
			*reads = *reads + 1
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove("test:count_worker_inputs") })
	return reads
}

type changingPredecessorEngine struct {
	*attemptResultEngine
	envs   []map[string]string
	change func()
}

func (e *changingPredecessorEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.envs = append(e.envs, maps.Clone(req.Spec.Env))
	if len(e.envs) == 1 {
		e.change()
	}
	return e.attemptResultEngine.Create(req)
}

func TestRuntimePredecessorSnapshotReusedAcrossRetries(t *testing.T) {
	for _, mode := range []string{"cache", "ordinary cache disabled", "fanout cache disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := seedProducerTaskRun(t, "snapshot-retry")
			f.taskRun.CacheEnabled = mode == "cache"
			f.taskRun.CacheChain = cache.ChainValues // Separate image capability reads retain their own policy.
			f.taskRun.MaxAttempts = 2
			if mode == "fanout cache disabled" {
				f.taskRun.PartitionCount = 1
				f.taskRun.PartitionValue = "shard"
			}
			require.NoError(t, f.db.Save(f.taskRun).Error)
			predecessor := addWorkerPredecessor(t, f, `{"value":"first"}`)
			reads := countWorkerInputReads(t, f.db)
			engine := &changingPredecessorEngine{attemptResultEngine: &attemptResultEngine{results: []atom.Result{atom.Failure, atom.Success}}, change: func() {
				require.NoError(t, f.db.Model(predecessor).Updates(map[string]any{"output": datatypes.JSON(`{"value":"second"}`), "hash": "changed-hash"}).Error)
			}}
			(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(context.Background(), f.taskRun)
			require.Equal(t, 1, *reads)
			require.Len(t, engine.envs, 2)
			for _, env := range engine.envs {
				require.Equal(t, "first", env["CAESIUM_OUTPUT_UPSTREAM_VALUE"])
			}
			row := reloadInstanceRow(t, f.db, f.taskRun.ID)
			require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
			require.Equal(t, 2, row.Attempt)
			if mode == "ordinary cache disabled" {
				require.Empty(t, row.Hash)
				require.Empty(t, row.HashInputBlob)
				return
			}
			var blob cache.HashInputBlob
			require.NoError(t, json.Unmarshal(row.HashInputBlob, &blob))
			require.Equal(t, []string{"upstream-hash"}, blob.PredecessorHashes)
			require.Equal(t, map[string]string{"value": "first"}, blob.PredecessorOutputs["upstream"])
			digest := sha256.Sum256([]byte("first"))
			require.Equal(t, "sha256:"+hex.EncodeToString(digest[:]), blob.Env["CAESIUM_OUTPUT_UPSTREAM_VALUE"].Redacted.Digest)
			require.NotContains(t, blob.Env, "CAESIUM_RUN_ID")
			require.NotContains(t, blob.Env, "CAESIUM_PARTITION")
			var descriptor models.TaskExecutionDescriptor
			require.NoError(t, json.Unmarshal(row.ExecutionDescriptor, &descriptor))
			require.Equal(t, map[string]string{"value": "first"}, descriptor.DAG.PredecessorOutputs[predecessor.TaskID])
			require.Equal(t, "upstream-hash", descriptor.DAG.PredecessorEffectiveHashes[predecessor.TaskID])
		})
	}
}

func TestRuntimePredecessorFailurePreventsLaunchAndIdentity(t *testing.T) {
	for _, scenario := range []string{"row query", "refs", "descriptor refs", "names", "missing name", "scalar decode", "fanout decode", "aggregation", "env collision"} {
		t.Run(scenario, func(t *testing.T) {
			f := seedProducerTaskRun(t, "input-failure")
			predecessor := addWorkerPredecessor(t, f, `{"value":"valid"}`)
			injected := errors.New("input query failed")
			switch scenario {
			case "scalar decode":
				require.NoError(t, f.db.Model(predecessor).Update("output", datatypes.JSON(`{"value":9}`)).Error)
			case "fanout decode":
				require.NoError(t, f.db.Model(predecessor).Updates(map[string]any{"output": datatypes.JSON(`{"value":9}`), "partition_count": 1, "partition_value": "a"}).Error)
			case "aggregation":
				output, err := json.Marshal(map[string]string{"value": strings.Repeat("x", pkgtask.MaxOutputBytes)})
				require.NoError(t, err)
				require.NoError(t, f.db.Model(predecessor).Updates(map[string]any{"output": datatypes.JSON(output), "partition_count": 1, "partition_value": "a"}).Error)
			case "env collision":
				require.NoError(t, f.db.Model(predecessor).Update("output", datatypes.JSON(`{"v-a":"one","v_a":"two"}`)).Error)
			}
			require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register("test:worker_input_failure", func(tx *gorm.DB) {
				fail := false
				switch scenario {
				case "row query":
					fail = isExecutionInputRows(tx)
				case "refs":
					_, fail = tx.Statement.Dest.(*[]models.TaskEdge)
				case "descriptor refs":
					fail = strings.Contains(strings.Join(tx.Statement.Selects, " "), "task_runs.quarantine AS task_quarantine")
				case "names":
					_, fail = tx.Statement.Dest.(*[]models.Task)
				}
				if fail {
					tx.AddError(injected)
				}
			}))
			t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:worker_input_failure") })
			if scenario == "missing name" {
				require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("test:missing_worker_name", func(tx *gorm.DB) {
					if rows, ok := tx.Statement.Dest.(*[]models.Task); ok {
						*rows = nil
					}
				}))
				t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:missing_worker_name") })
			}
			engine := &captureCreateEngine{}
			sink := &fakeSink{}
			(&runtimeExecutor{store: f.store, localSink: sink, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(context.Background(), f.taskRun)
			require.Nil(t, engine.createReq)
			require.Equal(t, 1, sink.failed)
			require.Zero(t, sink.succeeded)
			require.Zero(t, sink.cached)
			if scenario == "row query" || scenario == "refs" || scenario == "descriptor refs" || scenario == "names" {
				require.ErrorIs(t, sink.lastErr, injected)
			}
			row := reloadInstanceRow(t, f.db, f.taskRun.ID)
			require.Empty(t, row.Hash)
			require.Empty(t, row.HashInputBlob)
			entries, err := cache.NewStore(f.db).ListByJob(f.task.JobID)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestRuntimePredecessorAcquisitionAuthority(t *testing.T) {
	for _, authority := range []string{"claim revoked", "canceled", "run deadline"} {
		t.Run(authority, func(t *testing.T) {
			f := seedProducerTaskRun(t, "input-authority")
			addWorkerPredecessor(t, f, `{"value":9}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if authority == "claim revoked" {
				require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.taskRun.ID).Update("claimed_by", "new-owner").Error)
			}
			if authority == "run deadline" {
				setWorkerRunDeadline(t, f.db, f.jobRun, f.taskRun, 50*time.Millisecond, time.Now())
			}
			require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("test:input_authority", func(tx *gorm.DB) {
				if !isExecutionInputRows(tx) {
					return
				}
				switch authority {
				case "canceled":
					cancel()
				case "run deadline":
					<-tx.Statement.Context.Done()
				}
			}))
			t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:input_authority") })
			engine := &captureCreateEngine{}
			sink := &fakeSink{}
			(&runtimeExecutor{store: f.store, localSink: sink, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(ctx, f.taskRun)
			require.Nil(t, engine.createReq)
			require.Zero(t, sink.failed, "acquisition failure cannot supersede ownership")
			require.Zero(t, sink.cached)
			require.Zero(t, sink.succeeded)
		})
	}
}

func TestRuntimePredecessorAcquisitionDoesNotStartAttemptTimer(t *testing.T) {
	f := seedProducerTaskRun(t, "input-attempt-budget")
	f.taskRun.CacheEnabled = false
	addWorkerPredecessor(t, f, `{"value":"valid"}`)
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register("test:input_acquisition_delay", func(tx *gorm.DB) {
		if isExecutionInputRows(tx) {
			time.Sleep(150 * time.Millisecond)
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:input_acquisition_delay") })
	engine := &captureCreateEngine{}
	sink := &fakeSink{}
	(&runtimeExecutor{store: f.store, taskTimeout: 100 * time.Millisecond, localSink: sink, engineFactory: func(ctx context.Context, _ models.AtomEngine) (atom.Engine, error) {
		require.NoError(t, ctx.Err())
		return engine, nil
	}}).Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq)
	require.Equal(t, 1, sink.succeeded)
	require.Zero(t, sink.failed)
}

func TestRuntimePredecessorPartitionEnvOverridesOutput(t *testing.T) {
	f := seedFanOutTaskRun(t, "input-partition-precedence", `{"from":"upstream","env":"CAESIUM_OUTPUT_UPSTREAM_VALUE"}`, pkgtask.Partition{Key: "partition"}, 1, false)
	addWorkerPredecessor(t, f, `{"value":"output"}`)
	spec, err := json.Marshal(container.Spec{Env: map[string]string{"CAESIUM_OUTPUT_UPSTREAM_VALUE": "base"}})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.Atom{}).Where("id = ?", f.taskRun.AtomID).Update("spec", datatypes.JSON(spec)).Error)
	env := executeCapturingEnv(t, f)
	require.Equal(t, "partition", env["CAESIUM_OUTPUT_UPSTREAM_VALUE"])
}

func TestRuntimePredecessorSnapshotOnCacheMissAndHit(t *testing.T) {
	f := seedProducerTaskRun(t, "inputs-cache")
	predecessor := addWorkerPredecessor(t, f, `{"value":"stable"}`)
	reads := countWorkerInputReads(t, f.db)
	engine := &captureCreateEngine{}
	(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq)
	require.Equal(t, 1, *reads)
	old := reloadInstanceRow(t, f.db, f.taskRun.ID)
	f.taskRun = f.newProducerRunAttempt(t)
	predCopy := *predecessor
	predCopy.ID = uuid.New()
	predCopy.JobRunID = f.jobRun.ID
	require.NoError(t, f.db.Create(&predCopy).Error)
	sink := &fakeSink{}
	hitEngine := &captureCreateEngine{}
	(&runtimeExecutor{store: f.store, localSink: sink, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return hitEngine, nil }}).Execute(context.Background(), f.taskRun)
	require.Nil(t, hitEngine.createReq)
	require.Equal(t, 1, sink.cached)
	require.Equal(t, 2, *reads)
	current := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, old.Hash, current.Hash)
	var blob cache.HashInputBlob
	require.NoError(t, json.Unmarshal(current.HashInputBlob, &blob))
	require.Equal(t, map[string]string{"value": "stable"}, blob.PredecessorOutputs["upstream"])
}

func TestRuntimePredecessorImageQueryFailureStillExecutesWithCacheBypass(t *testing.T) {
	f := seedProducerTaskRun(t, "input-image-capability")
	addWorkerPredecessor(t, f, `{"value":"valid"}`)
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register("test:image_capability_failure", func(tx *gorm.DB) {
		if slices.Contains(tx.Statement.Selects, "hash_input_blob") && !slices.Contains(tx.Statement.Selects, "output") {
			tx.AddError(errors.New("image capability unavailable"))
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:image_capability_failure") })
	engine := &captureCreateEngine{}
	(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq)
	require.Equal(t, "valid", engine.createReq.Spec.Env["CAESIUM_OUTPUT_UPSTREAM_VALUE"])
	row := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
	var blob cache.HashInputBlob
	require.NoError(t, json.Unmarshal(row.HashInputBlob, &blob))
	require.NotEmpty(t, blob.UnresolvedImageIdentity)
	entries, err := cache.NewStore(f.db).ListByJob(f.task.JobID)
	require.NoError(t, err)
	require.Empty(t, entries)
}
