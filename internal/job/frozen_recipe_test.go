package job

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestRetryExecutesRecipeFrozenOnTheRow is the local-lane half of the
// cross-lane contract in issue #354: a re-entered run executes the
// engine/image/command the run was REGISTERED with, not whatever the catalog
// says at re-entry time.
//
// The distributed worker has always done this — it reads taskRun.Engine,
// taskRun.Image and parseTaskCommand(taskRun.Command) off the row
// (internal/worker/runtime_executor.go). The local executor used to rebuild its
// runner from a live svc.Get(t.AtomID), so a `run retry` after a `job apply`
// ran the NEW command locally and the OLD one distributed — the same run,
// two answers, depending on the lane.
//
// The apply is simulated the way the importer performs it: reconcileTasksTx
// MUTATES the existing atom row in place (same atom ID, new engine/image/
// command/spec), so the frozen AtomID still resolves — only its contents moved.
func TestRetryExecutesRecipeFrozenOnTheRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Image = "alpine:3.23"
	catalogAtom.Command = `["sh","-c","echo v1"]`
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	// First run fails, leaving the run in a terminal state a retry can re-open.
	engine.resultByName[taskID.String()] = atom.Failure
	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskStatusByID(snapshot)[taskID])

	frozen := taskRunByID(snapshot, taskID)
	require.NotNil(t, frozen)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, frozen.Command)

	// `job apply` lands a new definition for the same step.
	catalogAtom.Image = "example/subject:v2"
	catalogAtom.Command = `["sh","-c","echo v2"]`

	// Retry the run. The reset must leave the recipe columns alone …
	retried, err := store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, taskRunByID(retried, taskID).Command,
		"retryFromFailure must reset scheduling state without rewriting the frozen recipe")

	// … and the re-entered run must execute them.
	delete(engine.resultByName, taskID.String())
	require.NoError(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 2, "expected one container per attempt")
	require.Equal(t, []string{"sh", "-c", "echo v1"}, created[1].Command,
		"the retry executed the live catalog command, not the one frozen on the task_runs row")
	require.Equal(t, "alpine:3.23", created[1].Image,
		"the retry executed the live catalog image, not the one frozen on the task_runs row")

	final := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusSucceeded, taskStatusByID(final)[taskID])
}

// TestFirstRunFreezesTheLiveCatalogRecipe is the other half of the contract:
// freezing on re-entry must not mean a run is blind to a definition change. A
// NEW run registers new rows from the current catalog, so applying a change and
// triggering a run picks it up — that is the documented way to adopt a fix.
func TestFirstRunFreezesTheLiveCatalogRecipe(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Command = `["sh","-c","echo v1"]`
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	require.NoError(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	catalogAtom.Command = `["sh","-c","echo v2"]`
	require.NoError(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 2)
	require.Equal(t, []string{"sh", "-c", "echo v1"}, created[0].Command)
	require.Equal(t, []string{"sh", "-c", "echo v2"}, created[1].Command,
		"a NEW run registers fresh rows, so it must pick up the applied definition")
}

// TestRetryUsesTheAttemptBudgetFrozenOnTheRow covers the other execution field
// the row freezes and the distributed worker reads from it: MaxAttempts
// (RegisterTasks writes task.Retries+1; the worker loops to
// taskRun.MaxAttempts). The local lane used to recompute the budget from the
// LIVE task, so a retry after a `job apply` that changed `retries:` gave the
// same run a different number of attempts depending on which lane executed it.
func TestRetryUsesTheAttemptBudgetFrozenOnTheRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskModel := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Retries: 1}
	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	persistGraph(t, db, taskSvc.tasks, nil)

	// Every attempt fails, on this run and on the retry.
	for _, key := range []string{
		taskID.String(),
		taskID.String() + "-attempt2",
		taskID.String() + "-attempt3",
		taskID.String() + "-attempt4",
	} {
		engine.createErrByName[key] = errors.New("always fails")
	}

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	// retries: 1 -> two attempts, and the row freezes max_attempts = 2.
	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))
	require.Len(t, engine.createRequestsForTask(taskID), 2)

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, 2, taskRunByID(snapshot, taskID).MaxAttempts)

	// `job apply` raises the step's retry budget.
	taskModel.Retries = 3

	_, err := store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)
	require.Error(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)))

	require.Len(t, engine.createRequestsForTask(taskID), 4,
		"the retry ran the applied retry budget instead of the 2 attempts the row froze")
}

// buildFrozenFieldFixture wires a one-task job whose row therefore freezes the
// job-level schemaValidation and the resolved cache config, and returns the
// pieces a test mutates to simulate `caesium job apply`.
func buildFrozenFieldFixture(t *testing.T, jobAlias string, opts ...func(*models.Job, *models.Task)) (
	*run.Store, *fakeEngine, *models.Job, *models.Task, []JobOption,
) {
	t.Helper()

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	jobModel := &models.Job{ID: jobID, Alias: jobAlias, TriggerID: uuid.New()}
	taskModel := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Name: "subject"}
	for _, apply := range opts {
		apply(jobModel, taskModel)
	}
	require.NoError(t, db.Create(jobModel).Error)

	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	persistGraph(t, db, taskSvc.tasks, nil)

	jobOpts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	return store, engine, jobModel, taskModel, jobOpts
}

// TestRetryValidatesAgainstTheFrozenOutputSchema covers `output_schema` and
// `schema_validation`, the two remaining columns RegisterTasks freezes and the
// distributed worker validates from (runtimeExecutor.runSchemaValidation reads
// taskRun.OutputSchema / taskRun.SchemaValidation).
//
// The local lane used to read them live - taskModel.OutputSchema and the live
// job's schemaValidation - so after a `job apply` edited the schema or flipped
// warn/fail, a retried run's PASS/FAIL outcome depended on which lane executed
// it. Here the run is registered under `fail` with a schema the output violates;
// the apply then relaxes both. The retry must still fail.
func TestRetryValidatesAgainstTheFrozenOutputSchema(t *testing.T) {
	strictSchema, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"rows": map[string]any{"type": "integer"}},
		"required":   []string{"rows"},
	})
	require.NoError(t, err)

	store, engine, _, taskModel, opts := buildFrozenFieldFixture(t, "frozen-schema",
		func(j *models.Job, task *models.Task) {
			j.SchemaValidation = jobdefschema.SchemaValidationFail
			task.OutputSchema = datatypes.JSON(strictSchema)
		})

	jobID := taskModel.JobID
	taskID := taskModel.ID

	// The container emits a string where the schema demands an integer.
	engine.logsByName[taskID.String()] = "##caesium::output {\"rows\":\"not-a-number\"}\n"

	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()),
		"fail-mode validation must fail the run")

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskStatusByID(snapshot)[taskID])
	frozen := taskRunByID(snapshot, taskID)
	require.Equal(t, jobdefschema.SchemaValidationFail, frozen.SchemaValidation,
		"the row must carry the enforcement mode the run was registered under")
	require.NotEmpty(t, frozen.OutputSchema, "the row must carry the registered schema")

	// `job apply` relaxes the contract: schema dropped AND enforcement disabled.
	// Neither may reach the already-registered run.
	taskModel.OutputSchema = nil
	require.NoError(t, store.DB().Model(&models.Job{}).
		Where("id = ?", jobID).Update("schema_validation", "").Error)
	require.NoError(t, store.DB().Model(&models.Task{}).
		Where("id = ?", taskID).Update("output_schema", nil).Error)

	_, err = store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)

	require.Error(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)),
		"the retry validated against the relaxed live definition instead of the schema frozen on the row")

	final := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskStatusByID(final)[taskID])
}

// TestRetryUsesTheCacheConfigFrozenOnTheRow covers the seven cache columns.
// cacheCfg.Version and cacheCfg.Chain are folded into the identity hash and
// cacheCfg.Enabled gates publication, so recomputing them from the live
// definition gave a retried task a different cache key - and a different publish
// decision - than the worker, which builds cacheCfg straight off the row.
//
// The mutation is a `cache.version` BUMP rather than a disable, deliberately:
// the hash is only computed and persisted while caching is on
// (internal/job/job.go gates it on cacheCfg.Enabled), so disabling the cache
// would make a pre-fix retry write no hash at all and leave the first run's
// value in place - the assertion would then pass without the fix. Bumping the
// version keeps caching on down both paths, so the hash is genuinely rewritten
// and the only question is WHICH version went into it.
//
// The task also has to FAIL in the first run, or the retry would preserve it as
// succeeded and never re-execute (nor re-hash) it.
func TestRetryUsesTheCacheConfigFrozenOnTheRow(t *testing.T) {
	cacheV1, err := json.Marshal(map[string]any{"ttl": "1h", "version": 1})
	require.NoError(t, err)

	store, engine, _, taskModel, opts := buildFrozenFieldFixture(t, "frozen-cache",
		func(j *models.Job, task *models.Task) {
			task.CacheConfig = datatypes.JSON(cacheV1)
		})

	jobID := taskModel.JobID
	taskID := taskModel.ID

	// Fails on every attempt so the retry genuinely re-executes and re-hashes.
	engine.resultByName[taskID.String()] = atom.Failure

	require.Error(t, New(&models.Job{ID: jobID}, opts...).Run(context.Background()))

	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskStatusByID(snapshot)[taskID])

	frozen := taskRunByID(snapshot, taskID)
	require.True(t, frozen.CacheEnabled, "the row must freeze the resolved cache config")
	require.Equal(t, 1, frozen.CacheVersion, "the row must freeze the registered cache version")

	registeredHash := taskHashFromDB(t, store, snapshot.ID, taskID)
	require.NotEmpty(t, registeredHash, "an enabled-cache task must record its identity hash")

	// `job apply` bumps the step's cache version - a deliberate key rotation.
	// A NEW run must honour it; this already-registered run must not.
	cacheV2, err := json.Marshal(map[string]any{"ttl": "1h", "version": 2})
	require.NoError(t, err)
	taskModel.CacheConfig = datatypes.JSON(cacheV2)
	require.NoError(t, store.DB().Model(&models.Task{}).
		Where("id = ?", taskID).Update("cache_config", datatypes.JSON(cacheV2)).Error)

	_, err = store.RetryFromFailure(snapshot.ID)
	require.NoError(t, err)
	require.Error(t, New(&models.Job{ID: jobID}, opts...).
		Run(run.WithContext(context.Background(), snapshot.ID)))

	require.Equal(t, registeredHash, taskHashFromDB(t, store, snapshot.ID, taskID),
		"the retry rebuilt cache identity from the live cache.version instead of the one frozen on the row")
}

// taskHashFromDB reads the identity hash the executor persisted for a task run.
func taskHashFromDB(t *testing.T, store *run.Store, runID, taskID uuid.UUID) string {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, store.DB().
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		First(&row).Error)
	return row.Hash
}
