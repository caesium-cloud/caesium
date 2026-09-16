package job

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const movedTagDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func stubImageResolver(digest string) *imagecheck.Resolver {
	fn := func(context.Context, string) (string, error) { return digest, nil }
	return imagecheck.NewResolver(
		imagecheck.WithEngineDigestFunc(models.AtomEngineDocker, fn),
		imagecheck.WithEngineDigestFunc(models.AtomEnginePodman, fn),
		imagecheck.WithEngineDigestFunc(models.AtomEngineKubernetes, fn),
	)
}

// TestPinDigestsExecutesResolvedDigestNotWarmTag is the local-lane half of
// executing the digest pinDigests recorded. The row still names the mutable
// tag (the recipe freeze); Create must be given the registry digest so a
// locally cached tag at digest A cannot run while the cache key claims B.
func TestPinDigestsExecutesResolvedDigestNotWarmTag(t *testing.T) {
	const imageTag = "registry.example.com/app:v1"

	for _, engineKind := range []models.AtomEngine{
		models.AtomEngineDocker,
		models.AtomEnginePodman,
		models.AtomEngineKubernetes,
	} {
		t.Run(string(engineKind), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

			store := run.NewStore(db)
			engine := newFakeEngine()

			jobID := uuid.New()
			taskID := uuid.New()
			atomID := uuid.New()

			jobModel := &models.Job{ID: jobID, Alias: "pin-digest-exec", TriggerID: uuid.New()}
			require.NoError(t, db.Create(jobModel).Error)

			taskModel := &models.Task{
				ID:          taskID,
				JobID:       jobID,
				AtomID:      atomID,
				Name:        "subject",
				CacheConfig: datatypes.JSON(`{"ttl":"1h","pinDigests":true,"digestTTL":0}`),
			}
			taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
			catalogAtom := fakeModelAtom(atomID)
			catalogAtom.Image = imageTag
			catalogAtom.Engine = engineKind
			atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
			persistGraph(t, db, taskSvc.tasks, nil)

			opts := withTestDeps(store, env.Environment{
				MaxParallelTasks:  1,
				TaskFailurePolicy: taskFailurePolicyHalt,
				ExecutionMode:     executionModeLocal,
			}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
			opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver(movedTagDigest) })

			require.NoError(t, New(jobModel, opts...).Run(context.Background()))

			created := engine.createRequestsForTask(taskID)
			require.Len(t, created, 1)
			require.Equal(t, imageTag+"@"+movedTagDigest, created[0].Image,
				"Create must execute the registry digest, not the warm mutable tag")

			frozen := taskRunByID(latestRunSnapshot(t, store, jobID), taskID)
			require.NotNil(t, frozen)
			require.Equal(t, movedTagDigest, frozen.ResolvedImageDigest,
				"the digest folded into the cache key must be the one that ran")
		})
	}
}

func TestPinDigestsOffLeavesMutableTag(t *testing.T) {
	const imageTag = "registry.example.com/app:v1"

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	jobModel := &models.Job{ID: jobID, Alias: "no-pin-digest", TriggerID: uuid.New()}
	require.NoError(t, db.Create(jobModel).Error)

	taskModel := &models.Task{
		ID:          taskID,
		JobID:       jobID,
		AtomID:      atomID,
		Name:        "subject",
		CacheConfig: datatypes.JSON(`{"ttl":"1h"}`),
	}
	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Image = imageTag
	catalogAtom.Engine = models.AtomEnginePodman
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
	opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver(movedTagDigest) })

	require.NoError(t, New(jobModel, opts...).Run(context.Background()))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 1)
	require.Equal(t, imageTag, created[0].Image)
}

func TestPinDigestsExecutesLocalConfigImageID(t *testing.T) {
	const imageTag = "locally-built:dev"
	const configID = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	marked := imagecheck.MarkImageIDDigest(configID)

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	jobModel := &models.Job{ID: jobID, Alias: "pin-local-id", TriggerID: uuid.New()}
	require.NoError(t, db.Create(jobModel).Error)

	taskModel := &models.Task{
		ID:          taskID,
		JobID:       jobID,
		AtomID:      atomID,
		Name:        "subject",
		CacheConfig: datatypes.JSON(`{"ttl":"1h","pinDigests":true,"digestTTL":0}`),
	}
	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Image = imageTag
	catalogAtom.Engine = models.AtomEngineDocker
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
	opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver(marked) })

	require.NoError(t, New(jobModel, opts...).Run(context.Background()))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 1)
	require.Equal(t, configID, created[0].Image,
		"Create must execute the local config ID, not locally-built:dev@<configID>")

	frozen := taskRunByID(latestRunSnapshot(t, store, jobID), taskID)
	require.NotNil(t, frozen)
	require.Equal(t, marked, frozen.ResolvedImageDigest)
}

// Unknown identity must defeat legacy tag entries, D2 equivalent-output
// substitution, and every transitive descendant, while values remains explicit.
func TestPinDigestsUnavailableBypassesLegacyCacheAndTransitiveDescendants(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	engine := newFakeEngine()
	jobID := uuid.New()
	model := &models.Job{ID: jobID, Alias: "unknown-image", TriggerID: uuid.New()}
	require.NoError(t, db.Create(model).Error)
	tasks := &fakeTaskService{}
	atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{}}
	edges := &fakeTaskEdgeService{}
	for i, name := range []string{"source", "mid", "leaf", "values"} {
		task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: uuid.New(), Name: name, Position: i, CacheConfig: datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`)}
		a := fakeModelAtom(task.AtomID)
		a.Image = "registry.example.com/verified:v1"
		a.Engine = models.AtomEngineKubernetes
		if name == "source" {
			a.Image = "node-local:mutable"
			task.CacheConfig = datatypes.JSON(`{"pinDigests":false,"ttl":"1h"}`)
		}
		if name == "values" {
			task.CacheConfig = datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h","chain":"values"}`)
		}
		tasks.tasks = append(tasks.tasks, task)
		atoms.atoms[task.AtomID] = a
		engine.logsByName[task.ID.String()] = "##caesium::output {\"token\":\"same\"}\n"
	}
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {0, 3}} {
		edges.edges = append(edges.edges, &models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: tasks.tasks[pair[0]].ID, ToTaskID: tasks.tasks[pair[1]].ID})
	}
	persistGraph(t, db, tasks.tasks, edges.edges)
	opts := withTestDeps(store, env.Environment{MaxParallelTasks: 1, ExecutionMode: executionModeLocal, TaskFailurePolicy: taskFailurePolicyHalt}, tasks, atoms, edges, engine)
	resolver := imagecheck.NewResolver(imagecheck.WithEngineDigestFunc(models.AtomEngineKubernetes, func(_ context.Context, image string) (string, error) {
		if image == "node-local:mutable" {
			return "", imagecheck.ErrDigestUnavailable
		}
		return movedTagDigest, nil
	}))
	opts = append(opts, func(j *job) { j.imageResolver = resolver })
	require.NoError(t, New(model, opts...).Run(context.Background()))
	cfg := datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`)
	tasks.tasks[0].CacheConfig = cfg
	require.NoError(t, db.Model(&models.Task{}).Where("id = ?", tasks.tasks[0].ID).Update("cache_config", cfg).Error)
	var priorHash string
	for iteration := 0; iteration < 2; iteration++ {
		if iteration == 1 {
			tasks.tasks[1].CacheConfig = datatypes.JSON(`false`)
			require.NoError(t, db.Model(&models.Task{}).Where("id = ?", tasks.tasks[1].ID).Update("cache_config", datatypes.JSON(`false`)).Error)
		}
		var midMisses dto.Metric
		midCounter := metrics.TaskCacheMissesTotal.WithLabelValues(model.Alias, "mid")
		require.NoError(t, midCounter.Write(&midMisses))
		require.NoError(t, New(model, opts...).Run(context.Background()))
		if iteration == 1 {
			var after dto.Metric
			require.NoError(t, midCounter.Write(&after))
			require.Equal(t, midMisses.GetCounter().GetValue(), after.GetCounter().GetValue(), "disabled intermediary is not a cache miss")
		}
		snapshot := latestRunSnapshot(t, store, jobID)
		for i, task := range tasks.tasks {
			var row models.TaskRun
			require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", snapshot.ID, task.ID).First(&row).Error)
			if i == 3 {
				require.Equal(t, string(run.TaskStatusCached), row.Status)
				continue
			}
			require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
			require.Empty(t, row.EffectiveHash, "uncertainty cannot be erased by equal-output short-circuit")
			require.Contains(t, string(row.HashInputBlob), "unresolvedImageIdentity")
			if i == 1 {
				require.NotEqual(t, priorHash, row.Hash)
				priorHash = row.Hash
			}
		}
	}
	for i, task := range tasks.tasks {
		expected := 3
		if i == 3 {
			expected = 1
		}
		require.Len(t, engine.createRequestsForTask(task.ID), expected)
	}
	var count int64
	require.NoError(t, db.Model(&models.TaskCache{}).Where("job_id = ?", jobID).Count(&count).Error)
	require.Equal(t, int64(4), count, "uncertain executions and descendants must not publish new entries")
}

func TestPinDigestsUnavailableThroughUnhashedTerminalPredecessor(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		failed, verified, terminalWriteFailed bool
	}{{name: "skipped"}, {name: "identity-write-failed", failed: true}, {name: "verified-skipped", verified: true}, {name: "failure-write-rejected", failed: true, terminalWriteFailed: true}} {
		t.Run(tc.name, func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			engine := newFakeEngine()
			jobID := uuid.New()
			model := &models.Job{ID: jobID, Alias: "unknown-skipped-image", TriggerID: uuid.New()}
			require.NoError(t, db.Create(model).Error)
			tasks := &fakeTaskService{}
			atoms := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{}}
			edges := &fakeTaskEdgeService{}
			for i, name := range []string{"source", "middle", "leaf", "values-middle", "values-leaf"} {
				task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: uuid.New(), Name: name, Position: i, CacheConfig: datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`)}
				a := fakeModelAtom(task.AtomID)
				a.Image = "registry.example.com/verified:v1"
				a.Engine = models.AtomEngineKubernetes
				if i == 0 {
					a.Image = "node-local:mutable"
				}
				if i == 1 || i == 3 {
					if i == 3 || !tc.failed {
						task.TriggerRule = jobdef.TriggerRuleAllFailed
					}
					task.CacheConfig = datatypes.JSON(`false`)
				}
				if i == 2 || i == 4 {
					task.TriggerRule = jobdef.TriggerRuleAllDone
				}
				if i == 3 {
					task.CacheConfig = datatypes.JSON(`{"chain":"values"}`)
				}
				tasks.tasks = append(tasks.tasks, task)
				atoms.atoms[task.AtomID] = a
				engine.logsByName[task.ID.String()] = "##caesium::output {\"token\":\"same\"}\n"
			}
			for _, pair := range [][2]int{{0, 1}, {1, 2}, {0, 3}, {3, 4}} {
				edges.edges = append(edges.edges, &models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: tasks.tasks[pair[0]].ID, ToTaskID: tasks.tasks[pair[1]].ID})
			}
			persistGraph(t, db, tasks.tasks, edges.edges)
			if tc.failed {
				require.NoError(t, db.Exec("CREATE TRIGGER fail_middle_identity BEFORE UPDATE OF hash ON task_runs WHEN NEW.task_id = '"+tasks.tasks[1].ID.String()+"' BEGIN SELECT RAISE(ABORT, 'identity write unavailable'); END").Error)
			}
			if tc.terminalWriteFailed {
				require.NoError(t, db.Exec("CREATE TRIGGER fail_middle_terminal BEFORE UPDATE OF status ON task_runs WHEN NEW.task_id = '"+tasks.tasks[1].ID.String()+"' AND NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'terminal write unavailable'); END").Error)
			}
			opts := withTestDeps(store, env.Environment{MaxParallelTasks: 1, ExecutionMode: executionModeLocal, TaskFailurePolicy: taskFailurePolicyContinue}, tasks, atoms, edges, engine)
			resolver := imagecheck.NewResolver(imagecheck.WithEngineDigestFunc(models.AtomEngineKubernetes, func(_ context.Context, image string) (string, error) {
				if image == "node-local:mutable" && !tc.verified {
					return "", imagecheck.ErrDigestUnavailable
				}
				return movedTagDigest, nil
			}))
			opts = append(opts, func(j *job) { j.imageResolver = resolver })
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				started := time.Now()
				err := New(model, opts...).Run(ctx)
				cancel()
				require.Less(t, time.Since(started), 3*time.Second, "identity failure must return without waiting for unresolved descendants")
				if tc.terminalWriteFailed {
					require.ErrorIs(t, err, errUnresolvedIdentityTerminalWrite)
					require.Contains(t, err.Error(), "identity write unavailable")
					require.Contains(t, err.Error(), "terminal write unavailable")
				}
				if tc.failed {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			snapshot := latestRunSnapshot(t, store, jobID)
			if tc.terminalWriteFailed {
				require.Empty(t, engine.createRequestsForTask(tasks.tasks[1].ID))
				require.Empty(t, engine.createRequestsForTask(tasks.tasks[2].ID), "cannot admit downstream work without durable predecessor identity")
				require.Empty(t, engine.createRequestsForTask(tasks.tasks[4].ID))
				return
			}
			expected := run.TaskStatusSkipped
			if tc.failed {
				expected = run.TaskStatusFailed
			}
			require.Equal(t, expected, taskRunByID(snapshot, tasks.tasks[1].ID).Status)
			require.Empty(t, engine.createRequestsForTask(tasks.tasks[1].ID))
			expectedLeafExecutions := 2
			if tc.verified {
				expectedLeafExecutions = 1
			}
			require.Len(t, engine.createRequestsForTask(tasks.tasks[2].ID), expectedLeafExecutions, "unhashed intermediary must preserve upstream identity qualification")
			require.Len(t, engine.createRequestsForTask(tasks.tasks[4].ID), 1, "explicit values boundary preserves cache reuse")
			require.Equal(t, run.TaskStatusCached, taskRunByID(snapshot, tasks.tasks[4].ID).Status)
		})
	}
}

func TestPinDigestsUnavailableFanoutPropagatesInstanceUncertainty(t *testing.T) {
	f := newFanOutFixture(t, `["one","two"]`, &jobdef.FanOut{From: "list", MaxParallel: 2, MaxPartitions: 16}, 0)
	f.enableProducerCache(t)
	f.addDownstream(t)
	cfg := datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`)
	f.taskSvc.tasks[0].CacheConfig = cfg
	require.NoError(t, f.db.Model(&models.Task{}).Where("id = ?", f.producer).Update("cache_config", cfg).Error)
	f.enableStepCache(t)
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver("") })
	hashes := map[string]string{}
	for range 2 {
		before := f.runIDs(t)
		require.NoError(t, New(&models.Job{ID: f.jobID, Alias: "fanout-unknown"}, opts...).Run(context.Background()))
		rows := f.instanceRowsFor(t, f.newRunIDSince(t, before))
		require.Len(t, rows, 2)
		for _, row := range rows {
			require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
			require.Contains(t, string(row.HashInputBlob), "unresolvedImageIdentity")
			require.NotEqual(t, hashes[row.PartitionValue], row.Hash)
			hashes[row.PartitionValue] = row.Hash
		}
	}
}

func TestPinDigestsUnavailableFanoutIdentityWriteFailureIsTerminalUnderContinue(t *testing.T) {
	f := newFanOutFixture(t, `["one","two"]`, &jobdef.FanOut{From: "list", MaxParallel: 1, MaxPartitions: 16, FailurePolicy: jobdef.FanOutFailureContinue}, 0)
	f.setFannedCacheConfig(t, datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`))
	require.NoError(t, f.db.Exec(`CREATE TRIGGER fail_unknown_identity BEFORE UPDATE OF hash ON task_runs
WHEN NEW.partition_value = 'one' AND NEW.partition_count > 0
BEGIN SELECT RAISE(ABORT, 'simulated identity write failure'); END;`).Error)
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver("") })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(&models.Job{ID: f.jobID}, opts...).Run(ctx) }()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "persist unresolved image identity")
		require.NoError(t, ctx.Err(), "failure must return before the timeout, without redispatching")
	case <-ctx.Done():
		t.Fatal("identity-write failure redrove a pending partition instead of terminating")
	}
	require.Zero(t, f.engine.createCount("one"), "unrecorded identity must not launch an atom")
	require.Equal(t, 1, f.engine.createCount("two"), "continue must still execute the independent sibling")
	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		if row.PartitionValue == "one" {
			require.Equal(t, string(run.TaskStatusFailed), row.Status)
			require.Contains(t, row.Error, "simulated identity write failure")
		} else {
			require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
		}
	}
}

func TestPinDigestsUnavailableFanoutDoubleWriteFailureAbortsDownstream(t *testing.T) {
	f := newFanOutFixture(t, `["one"]`, &jobdef.FanOut{From: "list", MaxParallel: 1, MaxPartitions: 16, FailurePolicy: jobdef.FanOutFailureContinue}, 0)
	f.addDownstream(t)
	f.setFannedCacheConfig(t, datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`))
	f.taskSvc.tasks[2].TriggerRule = jobdef.TriggerRuleAllDone
	f.taskSvc.tasks[2].CacheConfig = datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`)
	require.NoError(t, f.db.Model(&models.Task{}).Where("id = ?", f.downstream).Updates(map[string]any{"trigger_rule": jobdef.TriggerRuleAllDone, "cache_config": f.taskSvc.tasks[2].CacheConfig}).Error)
	f.atomSvc.atoms[f.taskSvc.tasks[1].AtomID].Image = "node-local:mutable"
	f.atomSvc.atoms[f.taskSvc.tasks[1].AtomID].Engine = models.AtomEngineKubernetes
	f.atomSvc.atoms[f.taskSvc.tasks[2].AtomID].Engine = models.AtomEngineKubernetes
	require.NoError(t, f.db.Exec("CREATE TRIGGER fail_unknown_hash BEFORE UPDATE OF hash ON task_runs WHEN NEW.task_id = '"+f.fanned.String()+"' BEGIN SELECT RAISE(ABORT, 'identity write unavailable'); END").Error)
	require.NoError(t, f.db.Exec("CREATE TRIGGER fail_unknown_terminal BEFORE UPDATE OF status ON task_runs WHEN NEW.task_id = '"+f.fanned.String()+"' AND NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'terminal write unavailable'); END").Error)
	resolver := imagecheck.NewResolver(imagecheck.WithEngineDigestFunc(models.AtomEngineKubernetes, func(_ context.Context, image string) (string, error) {
		if image == "node-local:mutable" {
			return "", imagecheck.ErrDigestUnavailable
		}
		return movedTagDigest, nil
	}))
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, func(j *job) { j.imageResolver = resolver })
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := New(&models.Job{ID: f.jobID}, opts...).Run(ctx)
		require.NoError(t, ctx.Err(), "double-write failure must return without timing out")
		cancel()
		require.ErrorIs(t, err, errUnresolvedIdentityTerminalWrite)
		require.ErrorContains(t, err, "identity write unavailable")
		require.ErrorContains(t, err, "terminal write unavailable")
	}
	snapshot := latestRunSnapshot(t, f.store, f.jobID)
	require.Zero(t, f.engine.createCount("one"))
	require.Empty(t, f.engine.createRequestsForTask(f.downstream), "unknown partition identity must never release an all_done cache consumer")
	require.False(t, taskRunByID(snapshot, f.downstream).CacheHit)
}

func TestPinDigestsUnavailableFanoutRetryRenewsIdentityInSameRun(t *testing.T) {
	f := newFanOutFixture(t, `["one","two"]`, &jobdef.FanOut{From: "list", MaxParallel: 1, MaxPartitions: 16, FailurePolicy: jobdef.FanOutFailureContinue}, 0)
	f.setFannedCacheConfig(t, datatypes.JSON(`{"pinDigests":true,"digestTTL":0,"ttl":"1h"}`))
	f.addDownstream(t)
	f.engine.createErrByPartition["one"] = errors.New("first image execution failed")
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	resolver := stubImageResolver(movedTagDigest)
	opts = append(opts, func(j *job) { j.imageResolver = resolver })
	require.Error(t, New(&models.Job{ID: f.jobID}, opts...).Run(context.Background()))
	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	var prior models.TaskRun
	for _, row := range rows {
		if row.PartitionValue == "one" {
			prior = row
		}
	}
	require.Equal(t, string(run.TaskStatusFailed), prior.Status)
	require.Equal(t, movedTagDigest, prior.ResolvedImageDigest)
	delete(f.engine.createErrByPartition, "one")
	resolver = stubImageResolver("")
	// Keep the stale known descriptor in place while allowing the authoritative
	// row hash/digest write and terminal status writes. The tag must not execute
	// while replay could still read the previous digest as its immutable proof.
	require.NoError(t, f.db.Exec(`CREATE TRIGGER fail_unknown_descriptor BEFORE UPDATE OF execution_descriptor ON task_runs
WHEN NEW.partition_value = 'one' AND instr(NEW.hash_input_blob, 'unresolvedImageIdentity') > 0
BEGIN SELECT RAISE(ABORT, 'simulated descriptor write failure'); END;`).Error)
	_, err := f.store.RetryFromFailure(prior.JobRunID)
	require.NoError(t, err)
	require.ErrorContains(t, New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), prior.JobRunID)), "persist unresolved image execution descriptor")
	require.Equal(t, 1, f.engine.createCount("one"), "stale descriptor must fail before another image execution")
	require.NoError(t, f.db.First(&prior, "id = ?", prior.ID).Error)
	require.Equal(t, string(run.TaskStatusFailed), prior.Status)
	require.Empty(t, prior.ResolvedImageDigest)
	require.Contains(t, string(prior.HashInputBlob), "unresolvedImageIdentity")
	var descriptor models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(prior.ExecutionDescriptor, &descriptor))
	require.Equal(t, movedTagDigest, descriptor.Runtime.ResolvedImageDigest)
	require.NoError(t, f.db.Exec("DROP TRIGGER fail_unknown_descriptor").Error)
	_, err = f.store.RetryFromFailure(prior.JobRunID)
	require.NoError(t, err)
	require.NoError(t, New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), prior.JobRunID)))
	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", prior.ID).Error)
	require.Equal(t, prior.JobRunID, retried.JobRunID)
	require.Equal(t, string(run.TaskStatusSucceeded), retried.Status)
	require.NotEqual(t, prior.Hash, retried.Hash, "a same-run retry must renew unknown image identity")
	require.NotEqual(t, string(prior.HashInputBlob), string(retried.HashInputBlob))
	require.Equal(t, 2, f.engine.createCount("one"))
	require.Equal(t, 1, f.engine.createCount("two"), "successful sibling stays frozen on retry")
	var downstream models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", prior.JobRunID, f.downstream).First(&downstream).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), downstream.Status)
	require.Contains(t, string(downstream.HashInputBlob), "unresolvedImageIdentity")
	var published int64
	require.NoError(t, f.db.Model(&models.TaskCache{}).Where("job_id = ?", f.jobID).Count(&published).Error)
	require.Equal(t, int64(1), published, "only the verified successful sibling may publish")
}
