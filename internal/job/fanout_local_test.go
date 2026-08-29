package job

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/cache"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// fanOutFixture builds the canonical two-step fan-out graph used by the local
// executor tests: a `list` producer that prints a ##caesium::partitions marker
// and a `process` step that declares fanOut over it.
type fanOutFixture struct {
	db       *gorm.DB
	store    *run.Store
	engine   *fakeEngine
	jobID    uuid.UUID
	producer uuid.UUID
	fanned   uuid.UUID
	// downstream is the optional third step consuming the fanned group.
	downstream uuid.UUID
	taskSvc    *fakeTaskService
	atomSvc    *fakeAtomService
	edgeSvc    *fakeTaskEdgeService
}

// addDownstream appends a `publish` step consuming the fanned group, with
// step-level caching on so its hash_input_blob (and therefore its
// PredecessorHashes) is persisted and observable.
func (f *fanOutFixture) addDownstream(t *testing.T) {
	t.Helper()
	f.downstream = uuid.New()
	downstreamAtom := uuid.New()

	task := &models.Task{
		ID:          f.downstream,
		JobID:       f.jobID,
		AtomID:      downstreamAtom,
		Name:        "publish",
		Position:    2,
		TriggerRule: string(schema.TriggerRuleAllSuccess),
		CacheConfig: datatypes.JSON("true"),
	}
	edge := &models.TaskEdge{ID: uuid.New(), JobID: f.jobID, FromTaskID: f.fanned, ToTaskID: f.downstream}

	f.taskSvc.tasks = append(f.taskSvc.tasks, task)
	f.atomSvc.atoms[downstreamAtom] = fakeModelAtom(downstreamAtom)
	f.edgeSvc.edges = append(f.edgeSvc.edges, edge)

	require.NoError(t, f.db.Create(task).Error)
	require.NoError(t, f.db.Create(edge).Error)
}

func newFanOutFixture(t *testing.T, partitions string, fo *schema.FanOut, retries int) *fanOutFixture {
	t.Helper()

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	f := &fanOutFixture{
		db:       db,
		store:    run.NewStore(db),
		engine:   newFakeEngine(),
		jobID:    uuid.New(),
		producer: uuid.New(),
		fanned:   uuid.New(),
	}

	producerAtom := uuid.New()
	fannedAtom := uuid.New()

	foJSON, err := json.Marshal(fo)
	require.NoError(t, err)

	f.taskSvc = &fakeTaskService{tasks: models.Tasks{
		{ID: f.producer, JobID: f.jobID, AtomID: producerAtom, Name: "list", Position: 0},
		{
			ID:           f.fanned,
			JobID:        f.jobID,
			AtomID:       fannedAtom,
			Name:         "process",
			Position:     1,
			Retries:      retries,
			FanOutConfig: datatypes.JSON(foJSON),
			TriggerRule:  string(schema.TriggerRuleAllSuccess),
		},
	}}
	f.atomSvc = &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		producerAtom: fakeModelAtom(producerAtom),
		fannedAtom:   fakeModelAtom(fannedAtom),
	}}
	f.edgeSvc = &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: f.jobID, FromTaskID: f.producer, ToTaskID: f.fanned},
	}}
	persistGraph(t, f.db, f.taskSvc.tasks, f.edgeSvc.edges)

	f.engine.logsByName[f.producer.String()] = "##caesium::partitions " + partitions + "\n"

	return f
}

// enableStepCache turns on step-level caching for the fanned step. The local
// executor reads cache defaults from the process env (cache.ConfigFromEnv), so
// a step-level override is the hermetic way to exercise the cache path.
func (f *fanOutFixture) enableStepCache(t *testing.T) {
	t.Helper()
	f.setFannedCacheConfig(t, datatypes.JSON("true"))
}

// enableStepCacheChain turns on step-level caching for the fanned step with an
// explicit cache.chain. The map form also sets Enabled (applyCache), matching
// a manifest that writes `cache: {chain: values}` rather than `cache: true`.
func (f *fanOutFixture) enableStepCacheChain(t *testing.T, chain string) {
	t.Helper()
	f.setFannedCacheConfig(t, datatypes.JSON(fmt.Sprintf(`{"chain":%q}`, chain)))
}

func (f *fanOutFixture) setFannedCacheConfig(t *testing.T, cfg datatypes.JSON) {
	t.Helper()
	f.taskSvc.tasks[1].CacheConfig = cfg
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.fanned).
		Update("cache_config", cfg).Error)
}

// setProducerCommand rewrites the listing step's atom command so the next run
// computes a different producer identity hash. Used to churn predecessor
// identity without touching the emitted partition fingerprints.
func (f *fanOutFixture) setProducerCommand(t *testing.T, commandJSON string) {
	t.Helper()
	atomID := f.taskSvc.tasks[0].AtomID
	atom := f.atomSvc.atoms[atomID]
	require.NotNil(t, atom, "producer atom must be in the fake catalog")
	atom.Command = commandJSON
}

// enableProducerCache turns on step-level caching for the `list` producer so a
// second run of the same job resolves it from cache instead of re-executing it.
// That is the only hermetic way to reach the cached-producer expansion path:
// the group must expand from the partition list recorded on the cache entry,
// because no container runs to emit a marker.
func (f *fanOutFixture) enableProducerCache(t *testing.T) {
	t.Helper()
	cfg := datatypes.JSON("true")
	f.taskSvc.tasks[0].CacheConfig = cfg
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.producer).
		Update("cache_config", cfg).Error)
}

// runIDs returns every JobRun of the fixture's job. Tests that execute the job
// more than once select by set difference rather than by created_at ordering,
// which two runs milliseconds apart do not reliably separate.
func (f *fanOutFixture) runIDs(t *testing.T) map[uuid.UUID]bool {
	t.Helper()
	var runs []models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Find(&runs).Error)
	out := make(map[uuid.UUID]bool, len(runs))
	for _, r := range runs {
		out[r.ID] = true
	}
	return out
}

// newRunIDSince returns the single JobRun created since the `before` snapshot.
func (f *fanOutFixture) newRunIDSince(t *testing.T, before map[uuid.UUID]bool) uuid.UUID {
	t.Helper()
	var found []uuid.UUID
	for id := range f.runIDs(t) {
		if !before[id] {
			found = append(found, id)
		}
	}
	require.Len(t, found, 1, "expected exactly one new job run")
	return found[0]
}

// instanceRowsFor is instanceRows for an explicitly identified run.
func (f *fanOutFixture) instanceRowsFor(t *testing.T, runID uuid.UUID) []models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ? AND partition_count > 0", runID, f.fanned).
		Order("partition_index asc").
		Find(&rows).Error)
	return rows
}

// failCompletionWriteFor makes the completion WRITE for one partition's instance
// fail while its container succeeds, via a SQLite trigger that aborts the UPDATE
// marking that row succeeded.
//
// This is the only hermetic way to reach the sweep's `running` branch: the local
// loop drains every in-flight instance before sweeping, so a row that is still
// running at that point means the container finished and the store write did
// not. A fake store would not do — the point is to exercise the real
// CompleteTaskInstance error path through the real transaction.
func (f *fanOutFixture) failCompletionWriteFor(t *testing.T, partition string) {
	t.Helper()
	require.NoError(t, f.db.Exec(fmt.Sprintf(`
CREATE TRIGGER fail_completion_%s BEFORE UPDATE ON task_runs
WHEN NEW.partition_value = '%s' AND NEW.status = '%s'
BEGIN SELECT RAISE(ABORT, 'simulated completion write failure'); END;`,
		partition, partition, string(run.TaskStatusSucceeded))).Error)
}

func (f *fanOutFixture) run(t *testing.T, vars env.Environment) error {
	t.Helper()
	opts := withTestDeps(f.store, vars, f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	return New(&models.Job{ID: f.jobID}, opts...).Run(context.Background())
}

func defaultFanOutVars() env.Environment {
	return env.Environment{
		MaxParallelTasks:    8,
		TaskFailurePolicy:   taskFailurePolicyContinue,
		ExecutionMode:       executionModeLocal,
		FanOutMaxPartitions: 64,
	}
}

// instanceRows returns the fan-out instance rows for the fanned step, ordered by
// partition index.
func (f *fanOutFixture) instanceRows(t *testing.T) []models.TaskRun {
	t.Helper()
	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)

	var rows []models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ? AND partition_count > 0", jobRun.ID, f.fanned).
		Order("partition_index asc").
		Find(&rows).Error)
	return rows
}

func statusByPartition(rows []models.TaskRun) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.PartitionValue] = r.Status
	}
	return out
}

// --- (a) maxParallel ---------------------------------------------------------

// TestFanOutLocalRespectsMaxParallel pins fanOut.maxParallel as an in-flight cap
// on the group. Before this, runFannedGroup executed instances strictly
// serially and never read maxParallel at all.
func TestFanOutLocalRespectsMaxParallel(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d","e","f"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		MaxParallel:   2,
	}, 0)
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} {
		f.engine.runDurationByPartition[p] = 40 * time.Millisecond
	}

	require.NoError(t, f.run(t, defaultFanOutVars()))

	require.LessOrEqual(t, f.engine.maxPartitionConcurrent(), 2,
		"fanOut.maxParallel must cap in-flight instances")
	require.Greater(t, f.engine.maxPartitionConcurrent(), 1,
		"instances must actually run concurrently, not serially")

	for part, status := range statusByPartition(f.instanceRows(t)) {
		require.Equal(t, string(run.TaskStatusSucceeded), status, "partition %s", part)
	}
}

// TestFanOutLocalUnsetMaxParallelRunsConcurrently pins that an unset
// maxParallel is bounded only by the job-level maxParallelTasks.
func TestFanOutLocalUnsetMaxParallelRunsConcurrently(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	for _, p := range []string{"a", "b", "c", "d"} {
		f.engine.runDurationByPartition[p] = 40 * time.Millisecond
	}

	require.NoError(t, f.run(t, defaultFanOutVars()))
	require.GreaterOrEqual(t, f.engine.maxPartitionConcurrent(), 3,
		"an unset maxParallel must not serialize the group")
}

// TestFanOutLocalHonoursJobMaxParallelTasks pins that the group never exceeds
// the job-level pool size even when maxParallel is larger.
func TestFanOutLocalHonoursJobMaxParallelTasks(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d","e","f"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		MaxParallel:   16,
	}, 0)
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} {
		f.engine.runDurationByPartition[p] = 40 * time.Millisecond
	}

	vars := defaultFanOutVars()
	vars.MaxParallelTasks = 2
	require.NoError(t, f.run(t, vars))

	require.LessOrEqual(t, f.engine.maxPartitionConcurrent(), 2,
		"group in-flight must respect job-level maxParallelTasks")
}

// --- (b)+(c) cache identity and cache writes ---------------------------------

// TestFanOutLocalPersistsPerInstanceHashAndBlob pins that every instance
// records its own identity hash and hash-input blob. Without the blob
// `caesium why` cannot explain a partition, and without the hash `run retry`
// cannot match one.
func TestFanOutLocalPersistsPerInstanceHashAndBlob(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)

	f.enableStepCache(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))

	rows := f.instanceRows(t)
	require.Len(t, rows, 3)

	seen := map[string]bool{}
	for _, r := range rows {
		require.NotEmpty(t, r.Hash, "partition %s must persist an identity hash", r.PartitionValue)
		require.NotEmpty(t, r.HashInputBlob, "partition %s must persist a hash-input blob", r.PartitionValue)
		require.False(t, seen[r.Hash], "each partition must hash to a distinct identity")
		seen[r.Hash] = true
	}
}

// TestFanOutLocalWritesCacheEntryPerPartition pins that a successful instance
// publishes a cache entry, so a re-run of the same partition set is a hit.
// Before this the local path only ever called cacheStore.Get.
func TestFanOutLocalWritesCacheEntryPerPartition(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)

	f.enableStepCache(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))

	rows := f.instanceRows(t)
	require.Len(t, rows, 3)

	cacheStore := cache.NewStore(f.db)
	for _, r := range rows {
		_, found, err := cacheStore.Get(r.Hash)
		require.NoError(t, err)
		require.True(t, found,
			"partition %s must publish a cache entry for its identity hash", r.PartitionValue)
	}
}

// TestFanOutHashInputMatchesUnfannedPath pins (c): the fanned and unfanned local
// paths must build the SAME cache.HashInput for a task, differing only in the
// three partition fields. A drifting field list silently changes cache identity
// for fanned steps only.
func TestFanOutHashInputMatchesUnfannedPath(t *testing.T) {
	args := taskHashInputArgs{
		JobAlias:            "alias",
		TaskName:            "process",
		Image:               "alpine:3.23",
		ResolvedImageDigest: "sha256:abc",
		Command:             []string{"sh", "-c", "echo hi"},
		Env:                 map[string]string{"A": "1"},
		WorkDir:             "/w",
		PredecessorHashes:   []string{"h1"},
		PredecessorOutputs:  map[string]map[string]string{"list": {"k": "v"}},
		RunParams:           map[string]string{"p": "q"},
		CacheVersion:        3,
	}

	unfanned := buildTaskHashInput(args)
	fanned := buildTaskHashInput(args)

	require.Equal(t, unfanned, fanned,
		"an unpartitioned instance must hash identically to the unfanned path")
	require.Equal(t, unfanned.Compute(), fanned.Compute())

	// Adding the partition fields must be the ONLY difference.
	args.Partition = "a"
	args.PartitionFingerprint = "sha256:" + fmt.Sprintf("%064d", 1)
	withPartition := buildTaskHashInput(args)
	require.NotEqual(t, unfanned.Compute(), withPartition.Compute())

	withPartition.Partition = ""
	withPartition.PartitionFingerprint = ""
	require.Equal(t, unfanned, withPartition,
		"partition fields must be the only delta between the two paths")
}

func localPartitionFingerprint(hexByte string) string {
	return "sha256:" + strings.Repeat(hexByte, 32)
}

func structuredPartitionList(fpA, fpB, fpC string) string {
	return fmt.Sprintf(
		`[{"key":"a","fingerprint":%q},{"key":"b","fingerprint":%q},{"key":"c","fingerprint":%q}]`,
		fpA, fpB, fpC)
}

func producerStatus(t *testing.T, f *fanOutFixture, runID uuid.UUID) string {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", runID, f.producer).First(&row).Error)
	return row.Status
}

func instanceStatusByPartition(t *testing.T, f *fanOutFixture, runID uuid.UUID) map[string]string {
	t.Helper()
	return statusByPartition(f.instanceRowsFor(t, runID))
}

// TestFanOutLocalValuesChainSkipsUnchangedPartitions is the local-lane half of
// issue #360: with cache.chain: values on the fanned consumer, a producer
// re-run that changes only some fingerprints re-executes exactly those
// instances. The producer is cache-enabled so it records an identity hash;
// without that, there is no predecessor-hash churn for values mode to exclude
// and the skip would be vacuously true (the TestFanOutPerPartitionCacheIdentity
// workaround).
func TestFanOutLocalValuesChainSkipsUnchangedPartitions(t *testing.T) {
	fpA := localPartitionFingerprint("a1")
	fpB := localPartitionFingerprint("b2")
	fpC := localPartitionFingerprint("c3")
	fpBPrime := localPartitionFingerprint("d4")

	f := newFanOutFixture(t, structuredPartitionList(fpA, fpB, fpC), &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)
	f.enableStepCacheChain(t, schema.CacheChainValues)

	before1 := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	run1 := f.newRunIDSince(t, before1)
	require.Equal(t, string(run.TaskStatusSucceeded), producerStatus(t, f, run1))
	got1 := instanceStatusByPartition(t, f, run1)
	require.Equal(t, map[string]string{
		"a": string(run.TaskStatusSucceeded),
		"b": string(run.TaskStatusSucceeded),
		"c": string(run.TaskStatusSucceeded),
	}, got1)
	for _, p := range []string{"a", "b", "c"} {
		require.Equal(t, 1, f.engine.createCount(p), "run 1 partition %s must execute cold", p)
	}

	// Producer identity churns (command moved); emitted fingerprints do not.
	// Under chain: values every instance must cache-hit — this is the skip the
	// feature exists for. The producer itself must re-execute, or the later
	// assertions cannot distinguish "values mode worked" from "the producer
	// never actually re-ran".
	f.setProducerCommand(t, `["echo","ok","r2"]`)
	before2 := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	run2 := f.newRunIDSince(t, before2)
	require.Equal(t, string(run.TaskStatusSucceeded), producerStatus(t, f, run2),
		"the producer command changed, so it must re-execute rather than cache-hit")
	got2 := instanceStatusByPartition(t, f, run2)
	require.Equal(t, map[string]string{
		"a": string(run.TaskStatusCached),
		"b": string(run.TaskStatusCached),
		"c": string(run.TaskStatusCached),
	}, got2, "unchanged key+fingerprint+outputs must hit under chain: values")
	for _, p := range []string{"a", "b", "c"} {
		require.Equal(t, 1, f.engine.createCount(p),
			"run 2 partition %s must not re-execute: %v", p, got2)
	}

	// Fingerprint of b moves. Fingerprints are authoritative: b misses even
	// though the key and the (empty) predecessor outputs look the same.
	f.setProducerCommand(t, `["echo","ok","r3"]`)
	f.engine.logsByName[f.producer.String()] = "##caesium::partitions " + structuredPartitionList(fpA, fpBPrime, fpC) + "\n"
	before3 := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	run3 := f.newRunIDSince(t, before3)
	require.Equal(t, string(run.TaskStatusSucceeded), producerStatus(t, f, run3))
	got3 := instanceStatusByPartition(t, f, run3)
	require.Equal(t, map[string]string{
		"a": string(run.TaskStatusCached),
		"b": string(run.TaskStatusSucceeded),
		"c": string(run.TaskStatusCached),
	}, got3, "exactly the re-fingerprinted partition must re-execute")
	require.Equal(t, 1, f.engine.createCount("a"))
	require.Equal(t, 2, f.engine.createCount("b"), "partition b must re-execute on the fingerprint change")
	require.Equal(t, 1, f.engine.createCount("c"))

	var bRow models.TaskRun
	for _, r := range f.instanceRowsFor(t, run3) {
		if r.PartitionValue == "b" {
			bRow = r
			break
		}
	}
	require.Equal(t, fpBPrime, bRow.PartitionFingerprint,
		"the instance row must record the fingerprint it was keyed by")
}

// TestFanOutLocalTransitiveChainRerunsAllOnProducerChurn is the no-regression
// half: the default chain still re-runs every instance when the producer
// identity moves, even if every fingerprint is unchanged. If this started
// skipping, TestFanOutLocalValuesChainSkipsUnchangedPartitions would no longer
// be proving the chain break.
func TestFanOutLocalTransitiveChainRerunsAllOnProducerChurn(t *testing.T) {
	fpA := localPartitionFingerprint("a1")
	fpB := localPartitionFingerprint("b2")
	fpC := localPartitionFingerprint("c3")

	f := newFanOutFixture(t, structuredPartitionList(fpA, fpB, fpC), &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)
	f.enableStepCache(t) // boolean true → chain defaults to transitive

	before1 := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	run1 := f.newRunIDSince(t, before1)
	require.Equal(t, string(run.TaskStatusSucceeded), producerStatus(t, f, run1))

	f.setProducerCommand(t, `["echo","ok","r2"]`)
	before2 := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	run2 := f.newRunIDSince(t, before2)
	require.Equal(t, string(run.TaskStatusSucceeded), producerStatus(t, f, run2),
		"the producer command changed, so it must re-execute")
	got2 := instanceStatusByPartition(t, f, run2)
	require.Equal(t, map[string]string{
		"a": string(run.TaskStatusSucceeded),
		"b": string(run.TaskStatusSucceeded),
		"c": string(run.TaskStatusSucceeded),
	}, got2, "default chain must re-run every instance when the producer identity moves")
	for _, p := range []string{"a", "b", "c"} {
		require.Equal(t, 2, f.engine.createCount(p),
			"transitive partition %s must re-execute after producer churn", p)
	}
}

// --- (d) per-instance retries ------------------------------------------------

// TestFanOutLocalRetriesFailedInstance pins that an instance honours the step's
// Retries. runFannedGroup previously hardcoded attempt=1.
func TestFanOutLocalRetriesFailedInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 1) // one retry = two attempts
	f.engine.failCreateTimes["b"] = 1

	require.NoError(t, f.run(t, defaultFanOutVars()))

	require.Equal(t, 2, f.engine.createCount("b"), "partition b must be retried once")
	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusSucceeded), status["b"])
	require.Equal(t, string(run.TaskStatusSucceeded), status["a"])
	require.Equal(t, string(run.TaskStatusSucceeded), status["c"])

	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "b" {
			require.Equal(t, 2, r.Attempt, "retried instance must record attempt 2")
		}
	}
}

// TestFanOutLocalRetryExhaustionFailsInstance pins that a sibling exhausting its
// retries fails only itself under failurePolicy: continue.
func TestFanOutLocalRetryExhaustionFailsInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 1)
	f.engine.failCreateTimes["b"] = 5 // never succeeds

	err := f.run(t, defaultFanOutVars())
	require.Error(t, err)

	require.Equal(t, 2, f.engine.createCount("b"), "must stop after MaxAttempts")
	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), status["b"])
	require.Equal(t, string(run.TaskStatusSucceeded), status["a"])
	require.Equal(t, string(run.TaskStatusSucceeded), status["c"])
}

// --- (e) failurePolicy -------------------------------------------------------

// TestFanOutLocalFailFastCancelsPendingSiblings pins failurePolicy: fail_fast.
func TestFanOutLocalFailFastCancelsPendingSiblings(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		MaxParallel:   1,
		FailurePolicy: schema.FanOutFailureFailFast,
	}, 0)
	f.engine.createErrByPartition["a"] = fmt.Errorf("boom")

	err := f.run(t, defaultFanOutVars())
	require.Error(t, err)

	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), status["a"])
	for _, p := range []string{"b", "c", "d"} {
		require.NotEqual(t, string(run.TaskStatusSucceeded), status[p],
			"fail_fast must not run sibling %s after the first failure", p)
	}
	require.Equal(t, 0, f.engine.createCount("d"),
		"fail_fast must not dispatch pending siblings")
}

// TestFanOutLocalContinueRunsIndependentSiblings pins an EXPLICIT
// failurePolicy: continue — an independent sibling still runs after a failure.
// The policy is stated rather than omitted because the default is fail_fast
// (see TestFanOutLocalDefaultFailurePolicyIsFailFast).
func TestFanOutLocalContinueRunsIndependentSiblings(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		MaxParallel:   1,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["a"] = fmt.Errorf("boom")

	err := f.run(t, defaultFanOutVars())
	require.Error(t, err)

	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), status["a"])
	for _, p := range []string{"b", "c", "d"} {
		require.Equal(t, string(run.TaskStatusSucceeded), status[p],
			"continue must keep running independent sibling %s", p)
	}
}

// TestFanOutLocalDefaultFailurePolicyIsFailFast pins the default. An omitted
// fanOut.failurePolicy is fail_fast, matching the schema validator, which
// normalizes "" to fail_fast when it stamps the stored config
// (pkg/jobdef/definition.go validateSteps), and the run owner, whose
// normalizeFanOutFailurePolicy treats "" and any unrecognized value the same
// way. The local lane defaulted to `continue`, so a job that omitted the key
// ran every sibling under `caesium dev` and cancelled them in production —
// exactly the mode-dependent divergence that passes CI in one configuration.
func TestFanOutLocalDefaultFailurePolicyIsFailFast(t *testing.T) {
	for name, policy := range map[string]string{
		"unset":        "",
		"unrecognized": "sometimes",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
				From:          "list",
				MaxPartitions: 16,
				MaxParallel:   1,
				FailurePolicy: policy,
			}, 0)
			f.engine.createErrByPartition["a"] = fmt.Errorf("boom")

			require.Error(t, f.run(t, defaultFanOutVars()))

			rows := f.instanceRows(t)
			status := statusByPartition(rows)
			require.Equal(t, string(run.TaskStatusFailed), status["a"])
			for _, p := range []string{"b", "c", "d"} {
				require.NotEqual(t, string(run.TaskStatusSucceeded), status[p],
					"the default policy must not run sibling %s after the first failure", p)
			}
			require.Equal(t, 0, f.engine.createCount("d"),
				"the default policy must not dispatch pending siblings")

			// The reason string is the cross-lane contract, not just prose: the
			// owner writes exactly this on its own fail-fast skips
			// (internal/run/owner_state.go), so a UI or `caesium why` reading a
			// skipped instance must see one string, not two.
			for _, r := range rows {
				if r.Status == string(run.TaskStatusSkipped) {
					require.Equal(t, "fan-out group failed fast", r.Error,
						"partition %s", r.PartitionValue)
				}
			}
		})
	}
}

// --- cached producer ---------------------------------------------------------

// TestFanOutCachedProducerExpandsGroup pins that a producer resolved from CACHE
// still expands its consumer's fan-out group. A producer's emitted partition
// list is part of what its execution produced, so it rides the cache entry and
// must be replayed into the cache-hit transaction. Without that, the second run
// resolves the producer instantly, the group never expands, and the fanned step
// silently collapses to its single unexpanded template row — a correct-looking
// green run that did none of the work.
//
// The producer is rewired to emit a DIFFERENT partition list before the second
// run. That is the discriminating assertion: the group materializing the
// ORIGINAL partitions proves they came from the cache entry, not from a
// container that quietly re-executed.
func TestFanOutCachedProducerExpandsGroup(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)

	beforeFirst := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	firstRunID := f.newRunIDSince(t, beforeFirst)

	firstRows := f.instanceRowsFor(t, firstRunID)
	require.Len(t, firstRows, 3, "the cold run must expand three instances")

	// A cache entry carrying the emitted partitions is the precondition the
	// cache-hit path replays; assert it directly so a regression in the Put
	// names itself rather than surfacing as a mysterious empty group below.
	var producerRun models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.producer).
		First(&producerRun).Error)
	require.NotEmpty(t, producerRun.Hash)
	entry, found, err := cache.NewStore(f.db).Get(producerRun.Hash)
	require.NoError(t, err)
	require.True(t, found, "the producer must publish a cache entry")
	require.Len(t, entry.Partitions, 3,
		"the producer's cache entry must carry the partitions it emitted")

	// Rewire the marker: if the producer re-executes, the group expands to the
	// NEW list and the assertions below fail loudly.
	f.engine.logsByName[f.producer.String()] = "##caesium::partitions [\"x\",\"y\"]\n"

	beforeSecond := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	secondRunID := f.newRunIDSince(t, beforeSecond)
	require.NotEqual(t, firstRunID, secondRunID)

	var cachedProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.producer).
		First(&cachedProducer).Error)
	require.Equal(t, string(run.TaskStatusCached), cachedProducer.Status,
		"the second run's producer must resolve from cache")

	secondRows := f.instanceRowsFor(t, secondRunID)
	require.Len(t, secondRows, 3,
		"a cached producer must still materialize N instances")

	got := make([]string, 0, len(secondRows))
	for _, r := range secondRows {
		got = append(got, r.PartitionValue)
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status,
			"instance %s must actually execute, not just exist", r.PartitionValue)
	}
	require.Equal(t, []string{"a", "b", "c"}, got,
		"the group must expand from the CACHED partition list")

	// The instances ran for real: three fresh container creates on top of the
	// three from the cold run.
	for _, p := range []string{"a", "b", "c"} {
		require.Equal(t, 2, f.engine.createCount(p),
			"partition %s must have executed on both runs", p)
	}

	// And the collapsed group node resolved, so the run is not left waiting on
	// an unexpanded template row.
	snapshot, err := f.store.Get(secondRunID)
	require.NoError(t, err)
	group := taskRunByID(snapshot, f.fanned)
	require.NotNil(t, group)
	require.Equal(t, run.TaskStatusSucceeded, group.Status)

	var jobRun models.JobRun
	require.NoError(t, f.db.First(&jobRun, "id = ?", secondRunID).Error)
	require.Equal(t, string(run.StatusSucceeded), jobRun.Status)
}

// TestFanOutLocalStaleCacheEntryForcesProducerRerun pins F7 (dynamic-fanout
// closeout): a cache entry with no RECORDED partition list — Partitions ==
// nil, not merely empty, see cache.Entry.Partitions — must never be trusted as
// an empty group for a producer whose consumer fans out from it. Every entry
// written before that column existed looks exactly like this. Trusting it
// would silently collapse a real group to onEmpty on every future cache hit;
// the correct behaviour is to treat the ambiguous entry as a MISS, re-run the
// producer once, and let that real execution backfill a usable entry.
func TestFanOutLocalStaleCacheEntryForcesProducerRerun(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)

	beforeFirst := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	firstRunID := f.newRunIDSince(t, beforeFirst)
	require.Len(t, f.instanceRowsFor(t, firstRunID), 3, "the cold run must expand three instances")

	var producerRun models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.producer).
		First(&producerRun).Error)
	require.NotEmpty(t, producerRun.Hash)

	// Simulate a pre-fan-out cache entry: the column exists but was never
	// populated for this hash — exactly what every entry written before
	// cache.Entry.Partitions existed looks like on disk.
	require.NoError(t, f.db.Model(&models.TaskCache{}).
		Where("hash = ?", producerRun.Hash).
		Update("partitions", nil).Error)
	entry, found, err := cache.NewStore(f.db).Get(producerRun.Hash)
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, entry.Partitions, "the corrupted entry must read back nil, not an empty slice")

	// Rewire the marker to a DIFFERENT list. If the producer does not
	// re-execute, the group either stays skipped (the pre-fix onEmpty bug) or
	// expands to the stale list — never to this one.
	f.engine.logsByName[f.producer.String()] = "##caesium::partitions [\"x\",\"y\"]\n"

	beforeSecond := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	secondRunID := f.newRunIDSince(t, beforeSecond)

	var rerunProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.producer).
		First(&rerunProducer).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), rerunProducer.Status,
		"a producer whose cache entry has no recorded partition list must re-run, not resolve as cached")

	secondRows := f.instanceRowsFor(t, secondRunID)
	got := make([]string, 0, len(secondRows))
	for _, r := range secondRows {
		got = append(got, r.PartitionValue)
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status,
			"instance %s must actually execute, not just exist", r.PartitionValue)
	}
	require.Equal(t, []string{"x", "y"}, got,
		"the group must expand from the producer's FRESH execution, not from the stale/empty cache entry")

	// The fresh execution backfills a real entry (same hash — only the fake
	// log output changed, which is not part of the cache key): a THIRD run
	// with no further tampering must resolve from cache like any ordinary
	// warm producer, proving the entry self-healed rather than staying
	// permanently un-cacheable.
	f.engine.logsByName[f.producer.String()] = "##caesium::partitions [\"should-not-run\"]\n"
	beforeThird := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	thirdRunID := f.newRunIDSince(t, beforeThird)

	var thirdProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", thirdRunID, f.producer).
		First(&thirdProducer).Error)
	require.Equal(t, string(run.TaskStatusCached), thirdProducer.Status,
		"the backfilled entry must hit normally on the next run")

	thirdRows := f.instanceRowsFor(t, thirdRunID)
	gotThird := make([]string, 0, len(thirdRows))
	for _, r := range thirdRows {
		gotThird = append(gotThird, r.PartitionValue)
	}
	require.Equal(t, []string{"x", "y"}, gotThird,
		"the third run must expand from the BACKFILLED entry, not re-execute the rewired marker")
}

// TestFanOutLocalHasFanOutSuccessorErrorForcesProducerMiss is the F7 review's
// P2-A assertion: once a run is known to use fan-out
// (HasAnyFanOutConsumerForRun == true), a TRANSIENT ERROR from
// HasFanOutSuccessor itself must fail CLOSED — treated as a MISS, same as a
// confirmed fan-out consumer — not fail open and admit a cache entry with no
// recorded partition list as a hit. Failing open there would readmit exactly
// the F7 collapse this whole gate exists to prevent: the producer resolves
// "cached", the group expands from nothing, and the consumer is silently
// skipped via onEmpty.
func TestFanOutLocalHasFanOutSuccessorErrorForcesProducerMiss(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)

	beforeFirst := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	firstRunID := f.newRunIDSince(t, beforeFirst)

	var producerRun models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.producer).
		First(&producerRun).Error)
	require.NotEmpty(t, producerRun.Hash)

	// Simulate a pre-fan-out cache entry, exactly like
	// TestFanOutLocalStaleCacheEntryForcesProducerRerun: the column exists but
	// was never populated for this hash.
	require.NoError(t, f.db.Model(&models.TaskCache{}).
		Where("hash = ?", producerRun.Hash).
		Update("partitions", nil).Error)

	// Force HasFanOutSuccessor itself to error — not HasAnyFanOutConsumerForRun,
	// which must stay healthy so the inner check is actually reached.
	run.SetHasFanOutSuccessorErrForTest(fmt.Errorf("simulated transient dqlite read error"))
	t.Cleanup(func() { run.SetHasFanOutSuccessorErrForTest(nil) })

	beforeSecond := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	secondRunID := f.newRunIDSince(t, beforeSecond)

	var rerunProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.producer).
		First(&rerunProducer).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), rerunProducer.Status,
		"a transient HasFanOutSuccessor error must fail CLOSED (re-run the producer), not resolve it cached")
}

// TestFanOutLocalHasAnyFanOutConsumerErrorForcesProducerMiss is the F7 review's
// P2-A assertion: once a run is known to use fan-out
// (HasAnyFanOutConsumerForRun == true), a TRANSIENT ERROR from
// HasFanOutSuccessor itself must fail CLOSED — treated as a MISS, same as a
// confirmed fan-out consumer — not fail open and admit a cache entry with no
// recorded partition list as a hit. Failing open there would readmit exactly
// the F7 collapse this whole gate exists to prevent: the producer resolves
// "cached", the group expands from nothing, and the consumer is silently
// skipped via onEmpty.
func TestFanOutLocalHasAnyFanOutConsumerErrorForcesProducerMiss(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)

	beforeFirst := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	firstRunID := f.newRunIDSince(t, beforeFirst)

	var producerRun models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.producer).
		First(&producerRun).Error)
	require.NotEmpty(t, producerRun.Hash)

	// Simulate a pre-fan-out cache entry, exactly like
	// TestFanOutLocalStaleCacheEntryForcesProducerRerun: the column exists but
	// was never populated for this hash.
	require.NoError(t, f.db.Model(&models.TaskCache{}).
		Where("hash = ?", producerRun.Hash).
		Update("partitions", nil).Error)

	// Force the OUTER per-run pre-filter (HasAnyFanOutConsumerForRun) to
	// error. This run does use fan-out, so admitting the partition-less entry
	// here would collapse the group via onEmpty — the gate must fail closed
	// on this lookup too, not only on HasFanOutSuccessor.
	run.SetHasAnyFanOutConsumerErrForTest(fmt.Errorf("simulated transient dqlite read error"))
	t.Cleanup(func() { run.SetHasAnyFanOutConsumerErrForTest(nil) })

	beforeSecond := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	secondRunID := f.newRunIDSince(t, beforeSecond)

	var rerunProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.producer).
		First(&rerunProducer).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), rerunProducer.Status,
		"a transient HasAnyFanOutConsumerForRun error must fail CLOSED (re-run the producer), not resolve it cached")
}

// TestFanOutLocalEmptyProducerCacheHitTakesOnEmptyWithoutRerunning is the F7
// review's P1-1 assertion the implementation was missing: a producer that
// LEGITIMATELY emits zero partitions (`##caesium::partitions []`, the
// documented way to declare an empty work list — not a stale/legacy entry)
// must be cacheable exactly like any other task. Run 1 executes it for real
// and the consumer takes onEmpty (skip); run 2 must be a genuine cache HIT —
// the producer resolves "cached" without its container running again, and the
// consumer takes onEmpty again, this time purely from the replayed (non-nil,
// empty) partition list. Before the P1-1 fix, pkg/task's parser returned nil
// (not a non-nil empty slice) for an explicitly-empty array, so this producer
// was indistinguishable from a legacy entry and re-executed on EVERY run.
//
// The marker is rewired to a value that would fail parsing if the container
// ran again (see below), so a silent re-execution fails this test loudly
// rather than passing by coincidence.
func TestFanOutLocalEmptyProducerCacheHitTakesOnEmptyWithoutRerunning(t *testing.T) {
	f := newFanOutFixture(t, `[]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableProducerCache(t)

	beforeFirst := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	firstRunID := f.newRunIDSince(t, beforeFirst)

	var producerRun models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.producer).
		First(&producerRun).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), producerRun.Status,
		"the cold run must actually execute the producer")

	// A skipped-by-onEmpty consumer is never expanded — it stays the single
	// template row (partition_count == 0), so instanceRowsFor's
	// `partition_count > 0` filter would find nothing; query the template
	// directly instead.
	var firstConsumer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", firstRunID, f.fanned).
		First(&firstConsumer).Error)
	require.Equal(t, string(run.TaskStatusSkipped), firstConsumer.Status,
		"an empty partition list must take onEmpty (skip) on the cold run")

	// The cache entry the cold run published must carry a NON-NIL empty list —
	// "recorded, and it was empty" — not nil ("never recorded"), or the second
	// run below cannot possibly be a real cache hit.
	entry, found, err := cache.NewStore(f.db).Get(producerRun.Hash)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, entry.Partitions, "a legitimately-empty producer must record a non-nil empty list, not nil")
	require.Empty(t, entry.Partitions)

	// If the producer re-executes on the second run, this malformed marker
	// fails parsing outright — a silent re-run cannot masquerade as a pass.
	f.engine.logsByName[f.producer.String()] = "##caesium::partitions not-valid-json\n"

	beforeSecond := f.runIDs(t)
	require.NoError(t, f.run(t, defaultFanOutVars()))
	secondRunID := f.newRunIDSince(t, beforeSecond)

	var rerunProducer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.producer).
		First(&rerunProducer).Error)
	require.Equal(t, string(run.TaskStatusCached), rerunProducer.Status,
		"a legitimately-empty producer must resolve from cache on the second run, not re-execute — "+
			"had it re-run, the rewired malformed marker above would have failed parsing and this producer "+
			"would be \"failed\", never \"cached\"")

	var secondConsumer models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", secondRunID, f.fanned).
		First(&secondConsumer).Error)
	require.Equal(t, string(run.TaskStatusSkipped), secondConsumer.Status,
		"the cache-hit producer's replayed (empty) list must still take onEmpty")

	var jobRun models.JobRun
	require.NoError(t, f.db.First(&jobRun, "id = ?", secondRunID).Error)
	require.Equal(t, string(run.StatusSucceeded), jobRun.Status)
}

// TestFanOutLocalFailFastLetsRunningSiblingsFinish pins that fail_fast is
// PENDING-only: a sibling already in flight when the first failure lands is left
// to finish, and only siblings that were never dispatched are skipped.
//
// This is a cross-lane contract, not a local nicety. Caesium cannot kill a
// running container, so marking a live instance `skipped` would claim a terminal
// state for work that is still executing and invite its own completion to
// contradict the row afterwards — the replace-cancel resurrection shape. The SQL
// lane (failFastSkipSiblingsTx) and the owner lane both resolve pending siblings
// only; if the Kahn loop resolved running ones it would be the odd lane out, and
// the divergence would be invisible in any test that runs the group serially.
// TestFanOutLocalFailFastCancelsPendingSiblings uses maxParallel: 1, so it
// cannot see this: nothing is ever concurrently in flight there.
func TestFanOutLocalFailFastLetsRunningSiblingsFinish(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d","e","f"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		MaxParallel:   3,
		FailurePolicy: schema.FanOutFailureFailFast,
	}, 0)
	// a, b and c dispatch together (instances go out in partition-index order).
	// a must fail only AFTER b and c are genuinely in flight, so it fails on its
	// RESULT after a delay rather than on create: a create error returns before
	// the sibling goroutines have reached StartTask, and the store would then
	// skip them as PENDING — correct behavior, but a different code path, and
	// the test would be silently exercising the wrong one.
	f.engine.resultByPartition["a"] = atom.Failure
	f.engine.runDurationByPartition["a"] = 60 * time.Millisecond
	f.engine.runDurationByPartition["b"] = 200 * time.Millisecond
	f.engine.runDurationByPartition["c"] = 200 * time.Millisecond

	require.Error(t, f.run(t, defaultFanOutVars()))

	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), status["a"])

	// Precondition, asserted rather than assumed: b and c really did start
	// containers before a failed. If the interleaving this test exists to pin did
	// not happen, fail here loudly instead of passing vacuously below.
	for _, p := range []string{"b", "c"} {
		require.Equal(t, 1, f.engine.createCount(p),
			"precondition: sibling %s must have been in flight when a failed", p)
	}

	// The two in-flight siblings ran to completion.
	for _, p := range []string{"b", "c"} {
		require.Equal(t, string(run.TaskStatusSucceeded), status[p],
			"a sibling already running when fail_fast tripped must be left to finish, not resolved under it")
	}

	// The never-dispatched ones were cancelled, with the one reason string every
	// lane emits.
	for _, p := range []string{"d", "e", "f"} {
		require.Equal(t, string(run.TaskStatusSkipped), status[p],
			"fail_fast must cancel sibling %s, which was never dispatched", p)
		require.Equal(t, 0, f.engine.createCount(p),
			"sibling %s must never have started a container", p)
	}
	for _, r := range f.instanceRows(t) {
		if r.Status == string(run.TaskStatusSkipped) {
			require.Equal(t, "fan-out group failed fast", r.Error, "partition %s", r.PartitionValue)
		}
	}
}

// TestFanOutLocalUnrecordedOutcomeIsNotBlamedOnFailFast pins the reason string
// on the one instance whose outcome is genuinely unknown.
//
// The sweep resolves any row still non-terminal, which under fail_fast meant
// stamping "fan-out group failed fast" on ALL of them. But the loop drains every
// in-flight instance before it sweeps, so a `running` row at that point does not
// mean the group's policy cancelled it — it means the container finished and the
// completion WRITE failed, and the work may well have SUCCEEDED. That row's
// error column is what `caesium run partitions` and `caesium why --partition`
// display, so the instance we know least about was the one being given a
// confident, wrong explanation.
//
// The row must still be RESOLVED, not left running: local mode has no recovery
// owner, so an unresolved row is stranded forever and hangs the run's own
// accounting. Resolve it, but say the true thing about it.
func TestFanOutLocalUnrecordedOutcomeIsNotBlamedOnFailFast(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureFailFast,
	}, 0)
	// b's container succeeds; only its completion write fails. That write failure
	// is itself what trips fail_fast, so no sibling has to fail to reach the
	// branch under test — which keeps the test free of any inter-instance race.
	f.failCompletionWriteFor(t, "b")

	require.Error(t, f.run(t, defaultFanOutVars()))

	rows := f.instanceRows(t)
	byPart := make(map[string]models.TaskRun, len(rows))
	for _, r := range rows {
		byPart[r.PartitionValue] = r
	}

	require.Equal(t, string(run.TaskStatusSkipped), byPart["b"].Status,
		"a row whose completion write failed must still be resolved — local mode has no recovery owner to revisit it")
	require.Equal(t, "fan-out instance outcome unrecorded: completion write failed", byPart["b"].Error,
		"a store write failure must not be reported as the group's failure policy")

	// The siblings that did record an outcome are untouched and correct.
	for _, p := range []string{"a", "c"} {
		require.Equal(t, string(run.TaskStatusSucceeded), byPart[p].Status,
			"sibling %s recorded its own outcome and must keep it", p)
	}
}

// --- (f) ordering from the store --------------------------------------------

// TestFanOutLocalOrderingFollowsStoreOutstandingPredecessors pins that in-group
// ordering is driven by the instance rows' outstanding_predecessors column —
// the same scalar the distributed claimer gates on — rather than a private
// in-memory counter in the local loop.
func TestFanOutLocalOrderingFollowsStoreOutstandingPredecessors(t *testing.T) {
	f := newFanOutFixture(t,
		`[{"key":"a"},{"key":"b","dependsOn":["a"]},{"key":"c","dependsOn":["b"]}]`,
		&schema.FanOut{From: "list", MaxPartitions: 16}, 0)

	require.NoError(t, f.run(t, defaultFanOutVars()))

	require.Equal(t, []string{"a", "b", "c"}, f.engine.partitionStartOrder(),
		"a dependsOn chain must execute in order")

	for _, r := range f.instanceRows(t) {
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status)
		require.Equal(t, 0, r.OutstandingPredecessors,
			"every terminal instance must have been released to zero")
	}
}

// TestFanOutLocalDiamondOrdering pins that a diamond (a -> b, a -> c, b+c -> d)
// runs d only after both b and c are terminal.
func TestFanOutLocalDiamondOrdering(t *testing.T) {
	f := newFanOutFixture(t,
		`[{"key":"a"},{"key":"b","dependsOn":["a"]},{"key":"c","dependsOn":["a"]},{"key":"d","dependsOn":["b","c"]}]`,
		&schema.FanOut{From: "list", MaxPartitions: 16}, 0)

	require.NoError(t, f.run(t, defaultFanOutVars()))

	order := f.engine.partitionStartOrder()
	require.Len(t, order, 4)
	pos := map[string]int{}
	for i, p := range order {
		pos[p] = i
	}
	require.Less(t, pos["a"], pos["b"])
	require.Less(t, pos["a"], pos["c"])
	require.Less(t, pos["b"], pos["d"])
	require.Less(t, pos["c"], pos["d"])
}

// TestFanOutLocalFailureSkipsTransitiveDependents pins the load-bearing skip
// cascade: a failed instance's transitive in-group dependents resolve `skipped`
// rather than hanging the run to its timeout.
func TestFanOutLocalFailureSkipsTransitiveDependents(t *testing.T) {
	f := newFanOutFixture(t,
		`[{"key":"ok"},{"key":"bad"},{"key":"dep","dependsOn":["bad"]},{"key":"deep","dependsOn":["dep"]}]`,
		&schema.FanOut{
			From:          "list",
			MaxPartitions: 16,
			FailurePolicy: schema.FanOutFailureContinue,
		}, 0)
	f.engine.createErrByPartition["bad"] = fmt.Errorf("boom")

	err := f.run(t, defaultFanOutVars())
	require.Error(t, err)

	status := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusSucceeded), status["ok"])
	require.Equal(t, string(run.TaskStatusFailed), status["bad"])
	require.Equal(t, string(run.TaskStatusSkipped), status["dep"])
	require.Equal(t, string(run.TaskStatusSkipped), status["deep"],
		"the skip cascade must be transitive")
}

// --- (g) run completion accounting -------------------------------------------

// TestFanOutLocalRunCompletionCountsInstances pins that the run reaches a
// terminal state without tripping the "remaining tasks may be waiting on
// unresolved dependencies" guard once a group expands N instance rows.
func TestFanOutLocalRunCompletionCountsInstances(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d","e"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)

	require.NoError(t, f.run(t, defaultFanOutVars()))

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)
	require.Equal(t, string(run.StatusSucceeded), jobRun.Status)
	require.Empty(t, jobRun.Error)

	snapshot, err := f.store.Get(jobRun.ID)
	require.NoError(t, err)
	group := taskRunByID(snapshot, f.fanned)
	require.NotNil(t, group)
	require.Equal(t, run.TaskStatusSucceeded, group.Status,
		"the collapsed group must resolve succeeded")
}

// --- group identity for downstream steps -------------------------------------

// TestFanOutGroupIdentityFeedsDownstreamPredecessorHashes pins that a fanned
// predecessor contributes EXACTLY ONE aggregate entry to a downstream step's
// cache identity, and that the value the local executor folds in is the same one
// the SQL read path (store.PredecessorHashes) computes for the distributed lane.
//
// Before this the local lane never set taskHashes for a fanned group, so a
// downstream step's identity was blind to its input changing — and N entries
// instead of one would change the SHAPE of the key, re-keying the whole
// downstream subtree whenever a single partition was added or removed.
func TestFanOutGroupIdentityFeedsDownstreamPredecessorHashes(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.enableStepCache(t)
	f.addDownstream(t)

	require.NoError(t, f.run(t, defaultFanOutVars()))

	rows := f.instanceRows(t)
	require.Len(t, rows, 3)

	// Instance hashes in partition-index order, which is what GroupIdentityHash
	// requires and what instanceRows already orders by.
	instanceHashes := make([]string, 0, len(rows))
	for _, r := range rows {
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status)
		require.NotEmpty(t, r.Hash)
		instanceHashes = append(instanceHashes, r.Hash)
	}
	wantGroupHash := run.GroupIdentityHash(instanceHashes)
	require.NotEmpty(t, wantGroupHash)
	require.NotContains(t, instanceHashes, wantGroupHash,
		"the group hash must be an aggregate, not one instance's hash")

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)

	// (1) Cross-lane parity: the SQL read path the distributed lane uses agrees.
	fromStore, err := f.store.PredecessorHashes(jobRun.ID, f.downstream)
	require.NoError(t, err)
	require.Equal(t, []string{wantGroupHash}, fromStore,
		"store.PredecessorHashes must return exactly one aggregate entry for the fanned predecessor")

	// (2) The local executor folded that same single entry into the downstream
	// step's identity — read back from its persisted hash-input blob.
	var publish models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", jobRun.ID, f.downstream).
		First(&publish).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), publish.Status)
	require.NotEmpty(t, publish.HashInputBlob, "downstream step must persist its hash-input blob")

	var blob struct {
		PredecessorHashes []string `json:"predecessorHashes"`
	}
	require.NoError(t, json.Unmarshal(publish.HashInputBlob, &blob))
	require.Equal(t, []string{wantGroupHash}, blob.PredecessorHashes,
		"the downstream identity must fold exactly one entry for the fanned predecessor")
}

// TestFanOutRehydratesGroupOnRetriedRun pins that a run whose producer is
// already terminal still recognizes its fanned step as a GROUP, and that a
// retry re-enters that group executing ONLY the reset instances.
//
// fanOutGroups is normally seeded by the producer's completion payload. A
// retried run does not re-execute the producer (RetryFromFailure keeps it
// terminal-successful), so that payload never arrives; before rehydration the
// local loop treated the group as one ordinary task and every
// catalog-task-keyed store write matched N instance rows, failing the retried
// run with "multiple task instances match (run, task)".
func TestFanOutRehydratesGroupOnRetriedRun(t *testing.T) {
	f := newFanOutFixture(t,
		`[{"key":"a"},{"key":"b","dependsOn":["a"]},{"key":"c","dependsOn":["b"]},{"key":"solo"}]`,
		&schema.FanOut{
			From:          "list",
			MaxPartitions: 16,
			FailurePolicy: schema.FanOutFailureContinue,
		}, 0)
	f.engine.createErrByPartition["a"] = fmt.Errorf("boom")

	require.Error(t, f.run(t, defaultFanOutVars()))

	before := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), before["a"])
	require.Equal(t, string(run.TaskStatusSkipped), before["b"], "a dependent of a failed instance is skipped")
	require.Equal(t, string(run.TaskStatusSkipped), before["c"], "the skip cascade is transitive")
	require.Equal(t, string(run.TaskStatusSucceeded), before["solo"],
		"an independent sibling under failurePolicy: continue still runs")

	idsBefore := map[string]uuid.UUID{}
	for _, r := range f.instanceRows(t) {
		idsBefore[r.PartitionValue] = r.ID
	}
	soloCreatesBefore := f.engine.createCount("solo")
	require.Equal(t, 1, soloCreatesBefore)

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)

	// Fix the step and retry the SAME run. The producer stays terminal-success,
	// so no expansion payload is produced this time.
	delete(f.engine.createErrByPartition, "a")
	_, retryErr := f.store.RetryFromFailure(jobRun.ID)
	require.NoError(t, retryErr)

	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	require.NoError(t, New(&models.Job{ID: f.jobID}, opts...).Run(context.Background()),
		"the retried run must re-enter the already-expanded group")

	after := f.instanceRows(t)
	require.Len(t, after, 4, "a retry reuses the recorded instances; it must not re-expand the group")
	for _, r := range after {
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status, "partition %s", r.PartitionValue)
		require.Equal(t, idsBefore[r.PartitionValue], r.ID,
			"partition %s must keep its task_run_id across the retry", r.PartitionValue)
	}

	// Only the reset instances executed: the preserved sibling was not re-run.
	require.Equal(t, soloCreatesBefore, f.engine.createCount("solo"),
		"a terminal-success instance must be left alone by the retry")
	require.Equal(t, 2, f.engine.createCount("a"), "the failed instance re-executes")
	require.Equal(t, 1, f.engine.createCount("b"), "a skipped dependent executes once, after the retry")
	require.Equal(t, 1, f.engine.createCount("c"), "the transitive dependent executes once, after the retry")

	// The reset chain still respected its in-group order on the retried run.
	order := f.engine.partitionStartOrder()
	pos := map[string]int{}
	for i, p := range order {
		pos[p] = i
	}
	require.Less(t, pos["a"], pos["b"], "the retried chain must still run a before b")
	require.Less(t, pos["b"], pos["c"], "the retried chain must still run b before c")
}
