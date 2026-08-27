package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/cache"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TestOwnerSink_EveryRouteCarriesTaskRunID pins the D5b route-completeness
// defect: only the succeeded route stamped TaskRunID, so a fanned instance's
// failure or cache hit arrived at the owner with TaskRunID == uuid.Nil and fell
// back to a catalog task id that names N rows.
func TestOwnerSink_EveryRouteCarriesTaskRunID(t *testing.T) {
	task := sampleTaskRun()

	routes := map[string]func(s *ownerSink) error{
		"succeeded": func(s *ownerSink) error {
			return s.Succeeded(context.Background(), task, "success", nil, nil)
		},
		"succeeded_with_partitions": func(s *ownerSink) error {
			return s.SucceededWithPartitions(context.Background(), task, "success", nil, nil, []pkgtask.Partition{{Key: "a"}})
		},
		"failed": func(s *ownerSink) error {
			return s.Failed(context.Background(), task, errors.New("boom"))
		},
		"cached": func(s *ownerSink) error {
			return s.Cached(context.Background(), task, run.CacheHitSource{}, "success", nil, nil)
		},
	}

	for name, call := range routes {
		t.Run(name, func(t *testing.T) {
			p := &recordingPoster{}
			sink := newOwnerSink(ownerMeta(), p.post)
			require.NoError(t, call(sink))
			require.Len(t, p.calls, 1)
			require.Equal(t, task.ID, p.calls[0].TaskRunID,
				"every completion route must carry the instance TaskRun id")
			require.Equal(t, task.TaskID, p.calls[0].TaskID)
			require.Equal(t, task.JobRunID, p.calls[0].RunID)
		})
	}
}

// fanOutTaskRunFixture seeds a job with one fanned step and returns the store,
// the DB, and the instance TaskRun row to execute.
type fanOutTaskRunFixture struct {
	db      *gorm.DB
	store   *run.Store
	taskRun *models.TaskRun
	jobRun  *models.JobRun
	task    *models.Task
}

func seedFanOutTaskRun(t *testing.T, alias string, fanOutConfig string, part pkgtask.Partition, siblings int, cacheEnabled bool) fanOutTaskRunFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "fo-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: alias, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "shard", CreatedAt: now, UpdatedAt: now}
	if fanOutConfig != "" {
		task.FanOutConfig = datatypes.JSON([]byte(fanOutConfig))
	}
	require.NoError(t, db.Create(task).Error)

	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	attrs, err := json.Marshal(part.Attributes)
	require.NoError(t, err)
	deps, err := json.Marshal(part.DependsOn)
	require.NoError(t, err)

	mk := func(index int, p pkgtask.Partition, attrsJSON, depsJSON []byte) *models.TaskRun {
		tr := &models.TaskRun{
			ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(run.TaskStatusRunning), ClaimedBy: "node-a", Attempt: 1, MaxAttempts: 1,
			PartitionValue: p.Key, PartitionIndex: index, PartitionCount: siblings,
			PartitionFingerprint: p.Fingerprint,
			CacheEnabled:         cacheEnabled,
			CreatedAt:            now, UpdatedAt: now,
		}
		if len(p.Attributes) > 0 {
			tr.PartitionAttributes = datatypes.JSON(attrsJSON)
		}
		if len(p.DependsOn) > 0 {
			tr.PartitionDependsOn = datatypes.JSON(depsJSON)
		}
		require.NoError(t, db.Create(tr).Error)
		return tr
	}

	taskRun := mk(0, part, attrs, deps)
	for i := 1; i < siblings; i++ {
		mk(i, pkgtask.Partition{Key: part.Key + "-sib"}, nil, nil)
	}

	return fanOutTaskRunFixture{db: db, store: store, taskRun: taskRun, jobRun: jobRun, task: task}
}

func executeCapturingEnv(t *testing.T, f fanOutTaskRunFixture) map[string]string {
	t.Helper()
	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{
		store:     f.store,
		localSink: NewLocalSink(f.store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq, "the executor must have created a container")
	return engine.createReq.Spec.Env
}

// TestWorkerPartitionJSONIsFullNormalizedObject pins the distributed lane's
// CAESIUM_PARTITION_JSON: it must equal the normalized partition object, which
// includes dependsOn and attributes (both persisted on the instance row), not
// just key + fingerprint.
func TestWorkerPartitionJSONIsFullNormalizedObject(t *testing.T) {
	part := pkgtask.Partition{
		Key:         "region=us-east-1",
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		DependsOn:   []string{"region=eu-west-1"},
		Attributes:  map[string]string{"rows": "128", "tier": "gold"},
	}
	f := seedFanOutTaskRun(t, "fanout-json-job", `{"from":"producer"}`, part, 1, false)

	env := executeCapturingEnv(t, f)

	want, err := part.CanonicalJSON()
	require.NoError(t, err)
	require.Equal(t, string(want), env[jobdefschema.FanOutPartitionJSONEnv],
		"CAESIUM_PARTITION_JSON must be the normalized partition object")
	require.Equal(t, part.Key, env[jobdefschema.DefaultFanOutEnv])
}

// TestWorkerHonoursCustomFanOutEnv pins that the distributed lane respects a
// custom fanOut.env, which validation allows and the local lane already honours.
func TestWorkerHonoursCustomFanOutEnv(t *testing.T) {
	part := pkgtask.Partition{Key: "shard-7"}
	f := seedFanOutTaskRun(t, "fanout-env-job", `{"from":"producer","env":"SHARD_ID"}`, part, 1, false)

	env := executeCapturingEnv(t, f)

	require.Equal(t, "shard-7", env["SHARD_ID"], "a custom fanOut.env must be honoured by the worker lane")
	require.NotContains(t, env, jobdefschema.DefaultFanOutEnv,
		"the default name must not also be injected when fanOut.env renames it")
	require.NotEmpty(t, env[jobdefschema.FanOutPartitionJSONEnv],
		"CAESIUM_PARTITION_JSON is fixed and injected regardless of fanOut.env")
}

// TestWorkerCacheIdentityIncludesPartitionAttributes pins that the two lanes do
// not disagree about an instance's cache identity: partition attributes enter
// the hash on the worker path exactly as they do at internal/job/job.go.
func TestWorkerCacheIdentityIncludesPartitionAttributes(t *testing.T) {
	hashFor := func(attrs map[string]string) string {
		part := pkgtask.Partition{Key: "shard-1", Attributes: attrs}
		f := seedFanOutTaskRun(t, "fanout-hash-job", `{"from":"producer"}`, part, 1, true)
		executeCapturingEnv(t, f)

		var row models.TaskRun
		require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
		require.NotEmpty(t, row.Hash, "the worker lane must persist a cache identity hash")
		return row.Hash
	}

	a := hashFor(map[string]string{"rows": "100"})
	b := hashFor(map[string]string{"rows": "999"})
	require.NotEqual(t, a, b,
		"partition attributes must enter the worker lane's cache identity")
}

// --- Cached producer expansion (cache entries carry partitions) -------------

// partitionEmittingEngine is a capture engine whose container "prints" a
// partitions marker, so the executor's real marker-capture path runs.
type partitionEmittingEngine struct {
	captureCreateEngine
	logs string
}

func (e *partitionEmittingEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(e.logs)), nil
}

// recordingSink delegates to a real sink and records what the cache-hit route
// was handed, so a test can assert the partitions reached the completion seam.
type recordingSink struct {
	inner            CompletionSink
	cachedCalls      int
	cachedPartitions []pkgtask.Partition
}

func (s *recordingSink) Succeeded(ctx context.Context, tr *models.TaskRun, result string, out map[string]string, br []string) error {
	return s.inner.Succeeded(ctx, tr, result, out, br)
}

func (s *recordingSink) SucceededWithPartitions(ctx context.Context, tr *models.TaskRun, result string, out map[string]string, br []string, parts []pkgtask.Partition) error {
	if withParts, ok := s.inner.(interface {
		SucceededWithPartitions(context.Context, *models.TaskRun, string, map[string]string, []string, []pkgtask.Partition) error
	}); ok {
		return withParts.SucceededWithPartitions(ctx, tr, result, out, br, parts)
	}
	return s.inner.Succeeded(ctx, tr, result, out, br)
}

func (s *recordingSink) Failed(ctx context.Context, tr *models.TaskRun, err error) error {
	return s.inner.Failed(ctx, tr, err)
}

func (s *recordingSink) Cached(ctx context.Context, tr *models.TaskRun, src run.CacheHitSource, result string, out map[string]string, br []string) error {
	s.cachedCalls++
	return s.inner.Cached(ctx, tr, src, result, out, br)
}

func (s *recordingSink) CachedWithPartitions(ctx context.Context, tr *models.TaskRun, src run.CacheHitSource, result string, out map[string]string, br []string, parts []pkgtask.Partition) error {
	s.cachedCalls++
	s.cachedPartitions = parts
	if withParts, ok := s.inner.(cachedPartitionSink); ok {
		return withParts.CachedWithPartitions(ctx, tr, src, result, out, br, parts)
	}
	return s.inner.Cached(ctx, tr, src, result, out, br)
}

// seedProducerTaskRun seeds an UNFANNED, cache-enabled task run — a fan-out
// producer, which emits partitions but carries none itself.
func seedProducerTaskRun(t *testing.T, alias string) fanOutTaskRunFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "prod-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: alias, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "list", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)

	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	taskRun := &models.TaskRun{
		ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomModel.ID,
		Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
		Status: string(run.TaskStatusRunning), ClaimedBy: "node-a", Attempt: 1, MaxAttempts: 1,
		CacheEnabled: true, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(taskRun).Error)

	return fanOutTaskRunFixture{db: db, store: store, taskRun: taskRun, jobRun: jobRun, task: task}
}

const producerMarkerLog = `starting
##caesium::partitions [{"key":"a","fingerprint":"sha256:` +
	`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
	`{"key":"b","dependsOn":["a"],"rows":"128"}]
done
`

func wantProducerPartitions() []pkgtask.Partition {
	return []pkgtask.Partition{
		{Key: "a", Fingerprint: "sha256:" + strings.Repeat("a", 64)},
		{Key: "b", DependsOn: []string{"a"}, Attributes: map[string]string{"rows": "128"}},
	}
}

// TestWorkerCachesProducerPartitions asserts a producer's partition list is
// written onto its cache entry.  Without it the entry is useless to fan-out: a
// later hit replays result and output but the group has nothing to expand from.
func TestWorkerCachesProducerPartitions(t *testing.T) {
	f := seedProducerTaskRun(t, "producer-cache-job")

	engine := &partitionEmittingEngine{logs: producerMarkerLog}
	executor := &runtimeExecutor{
		store:     f.store,
		localSink: NewLocalSink(f.store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), f.taskRun)

	entries, err := cache.NewStore(f.db).ListByJob(f.jobRun.JobID)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the producer's successful result must be cached")
	require.Equal(t, wantProducerPartitions(), entries[0].Partitions,
		"the cache entry must carry the producer's partition list")
}

// TestWorkerCacheHitPassesPartitionsToSink drives the whole loop: run the
// producer cold to populate the cache, reset its row, run again to take the hit,
// and assert the completion seam received the cached partitions.  This is the
// seam that lets a cached producer expand its group.
func TestWorkerCacheHitPassesPartitionsToSink(t *testing.T) {
	f := seedProducerTaskRun(t, "producer-cachehit-job")

	newExecutor := func(sink CompletionSink) *runtimeExecutor {
		engine := &partitionEmittingEngine{logs: producerMarkerLog}
		return &runtimeExecutor{
			store:     f.store,
			localSink: sink,
			engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
				return engine, nil
			},
		}
	}

	// Cold run populates the cache.
	newExecutor(NewLocalSink(f.store)).Execute(context.Background(), f.taskRun)
	entries, err := cache.NewStore(f.db).ListByJob(f.jobRun.JobID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NotEmpty(t, entries[0].Partitions)

	// Reset the row so the same identity is executed again — now a hit.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.taskRun.ID).Updates(map[string]any{
		"status":       string(run.TaskStatusRunning),
		"result":       "",
		"completed_at": nil,
		"partitions":   nil,
	}).Error)

	rec := &recordingSink{inner: NewLocalSink(f.store)}
	fresh := *f.taskRun
	fresh.Status = string(run.TaskStatusRunning)
	newExecutor(rec).Execute(context.Background(), &fresh)

	require.Equal(t, 1, rec.cachedCalls, "the second run must be a cache hit")
	require.Equal(t, wantProducerPartitions(), rec.cachedPartitions,
		"a cache-hit producer must hand its partitions to the completion route")
}

// TestOwnerSink_CachedCarriesPartitions pins the owner half: a cached producer
// on a dispatched task reports over /internal/complete, and CompleteRequest must
// carry the partitions for status `cached` exactly as it does for `succeeded` —
// OwnerManager.CompleteInstance already plans an expansion for both statuses, so
// dropping them here is what makes the group collapse in owner mode.
func TestOwnerSink_CachedCarriesPartitions(t *testing.T) {
	p := &recordingPoster{}
	sink := newOwnerSink(ownerMeta(), p.post)
	task := sampleTaskRun()
	parts := wantProducerPartitions()

	require.NoError(t, sink.CachedWithPartitions(context.Background(), task,
		run.CacheHitSource{}, "success", map[string]string{"n": "2"}, nil, parts))

	require.Len(t, p.calls, 1)
	require.Equal(t, string(run.TaskStatusCached), p.calls[0].Status)
	require.Equal(t, parts, p.calls[0].Partitions,
		"the cached completion envelope must carry the producer's partitions")
	require.Equal(t, task.ID, p.calls[0].TaskRunID)
}

// TestWorkerPersistsHashOnTheInstanceRow pins that the distributed lane's
// identity write addresses the INSTANCE.
//
// The call passed taskRun.TaskID, which for a fanned step names N sibling rows.
// SetTaskHashWithBlob resolves its reference through loadTaskRunByIDOrUnique, so
// the write was refused with ErrAmbiguousTaskRun and swallowed as a log.Warn:
// the instance's hash and hash-input blob were never persisted at all, leaving
// `caesium why --partition`, `receipt get` and `run retry --partition` with no
// identity to match. Siblings=3 is what makes the catalog id genuinely
// ambiguous — a 1-instance fixture cannot catch this.
func TestWorkerPersistsHashOnTheInstanceRow(t *testing.T) {
	part := pkgtask.Partition{Key: "shard-0", Attributes: map[string]string{"rows": "100"}}
	f := seedFanOutTaskRun(t, "fanout-instance-hash-job", `{"from":"producer"}`, part, 3, true)

	// The catalog task id really does name three rows here.
	var siblingCount int64
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.jobRun.ID, f.task.ID).
		Count(&siblingCount).Error)
	require.Equal(t, int64(3), siblingCount)

	executeCapturingEnv(t, f)

	var executed models.TaskRun
	require.NoError(t, f.db.First(&executed, "id = ?", f.taskRun.ID).Error)
	require.NotEmpty(t, executed.Hash,
		"the executed instance must persist its own identity hash")
	require.NotEmpty(t, executed.HashInputBlob,
		"the executed instance must persist its own hash-input blob, or `caesium why --partition` cannot explain it")

	// The write must not have leaked onto the siblings.
	var others []models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ? AND id <> ?", f.jobRun.ID, f.task.ID, f.taskRun.ID).
		Find(&others).Error)
	require.Len(t, others, 2)
	for _, row := range others {
		require.Empty(t, row.Hash,
			"partition %s did not execute and must not inherit a sibling's identity", row.PartitionValue)
		require.Empty(t, row.HashInputBlob)
	}
}
