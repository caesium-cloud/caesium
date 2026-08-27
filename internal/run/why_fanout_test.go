package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// why_fanout_test.go covers the E3 contract for `caesium why` on a fanned step:
// the group summary by default, one instance under a --partition selector, an
// enumerating error for an unknown partition, and — the regression that matters
// most — an unfanned task whose explanation is byte-identical to the
// pre-fan-out shape.

// seedFanOutWhy creates a job/run with `producer` (unfanned) and `process`
// fanned into partitions a/b/c, statuses given by statuses. Returns run + task
// ids.
func seedFanOutWhy(t *testing.T, db *gorm.DB, jobID uuid.UUID, statuses []TaskStatus) (runID, taskID uuid.UUID, instanceIDs []uuid.UUID) {
	t.Helper()

	runID = uuid.New()
	taskID = uuid.New()
	base := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusRunning),
		TriggerType: "cron", TriggerAlias: "nightly", StartedAt: base,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "process"}).Error)

	values := []string{"a", "b", "c"}
	for i, status := range statuses {
		started := base.Add(time.Duration(i) * time.Second)
		completed := started.Add(10 * time.Second)
		hi := cache.HashInput{TaskName: "process", Image: "alpine:3.23", Partition: values[i]}
		errText := ""
		if status == TaskStatusFailed {
			errText = "exit status 1 on partition " + values[i]
		}
		id := uuid.New()
		instanceIDs = append(instanceIDs, id)
		require.NoError(t, db.Create(&models.TaskRun{
			ID: id, JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
			Status: string(status), CacheEnabled: true,
			CacheHit:      status == TaskStatusCached,
			Hash:          hi.Compute(),
			HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
			Error:         errText,
			Attempt:       1,

			PartitionValue: values[i],
			PartitionIndex: i,
			PartitionCount: len(statuses),

			StartedAt: &started, CompletedAt: &completed,
			CreatedAt: started, UpdatedAt: completed,
		}).Error)
	}
	return runID, taskID, instanceIDs
}

func TestWhyTask_FannedGroupReturnsGroupSummary(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	runID, taskID, ids := seedFanOutWhy(t, db, jobID,
		[]TaskStatus{TaskStatusSucceeded, TaskStatusFailed, TaskStatusSucceeded})

	exp, err := store.WhyTask(ctx, runID, "process")
	require.NoError(t, err)

	require.NotNil(t, exp.Group, "a fanned step must answer with the group summary, not an arbitrary sibling")
	require.Nil(t, exp.TaskRunID, "a group summary names no single instance")
	require.Equal(t, taskID, exp.TaskID)
	require.Equal(t, 3, exp.Group.PartitionCount)
	require.Equal(t, map[string]int{"succeeded": 2, "failed": 1}, exp.Group.StatusCounts)
	require.Equal(t, []string{"a", "b", "c"}, exp.Group.Partitions)
	require.Equal(t, string(TaskStatusFailed), exp.Status, "a group with a failed instance is failed")

	require.NotNil(t, exp.Group.FirstFailure)
	require.Equal(t, "b", exp.Group.FirstFailure.Partition)
	require.Equal(t, 1, exp.Group.FirstFailure.PartitionIndex)
	require.Equal(t, ids[1], exp.Group.FirstFailure.TaskRunID)
	require.Contains(t, exp.Group.FirstFailure.Error, "exit status 1")

	// Aggregate timing: first start -> last end (12s across the 3 staggered rows).
	require.Positive(t, exp.Group.DurationMS)

	require.Equal(t, "per_partition", exp.Baseline.Kind,
		"the group has N baselines; saying 'none' would read as 'no prior run'")
	require.Nil(t, exp.Diff, "a group has no single field diff")
	require.Contains(t, exp.Summary, "FANNED GROUP")
	require.Contains(t, exp.Summary, "--partition")
	require.Contains(t, exp.Summary, `first failure partition "b"`)
}

func TestWhyTask_PartitionSelectorPicksThatInstance(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	runID, _, ids := seedFanOutWhy(t, db, jobID,
		[]TaskStatus{TaskStatusSucceeded, TaskStatusFailed, TaskStatusSucceeded})

	exp, err := store.WhyTaskPartition(ctx, runID, "process", "b")
	require.NoError(t, err)

	require.Nil(t, exp.Group, "an explicit selection is a single-instance explanation")
	require.NotNil(t, exp.TaskRunID)
	require.Equal(t, ids[1], *exp.TaskRunID, "must be instance b's row, not a sibling")
	require.Equal(t, "b", exp.Partition)
	require.Equal(t, string(TaskStatusFailed), exp.Status)
	require.NotNil(t, exp.Diff)
}

func TestWhyTask_UnknownPartitionListsAvailableValues(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	runID, _, _ := seedFanOutWhy(t, db, jobID,
		[]TaskStatus{TaskStatusSucceeded, TaskStatusSucceeded, TaskStatusSucceeded})

	_, err := store.WhyTaskPartition(ctx, runID, "process", "zz")
	require.ErrorIs(t, err, ErrPartitionNotFound)
	// The whole point of the error is that the retry needs no second round trip.
	require.Contains(t, err.Error(), "a, b, c")
}

func TestWhyTask_PartitionSelectorOnUnfannedTaskIsRejected(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	seedUnfannedWhyTask(t, db, jobID, runID, "extract")

	_, err := store.WhyTaskPartition(ctx, runID, "extract", "a")
	require.ErrorIs(t, err, ErrPartitionNotFound)
	require.Contains(t, err.Error(), "is not fanned")
}

// TestWhyTask_UnfannedOutputIsByteIdenticalGolden pins the serialized shape of an
// unfanned explanation. Fan-out added three fields to WhyExplanation
// (taskRunId became a pointer, plus partition and group); all three are
// omit-when-empty precisely so an unfanned answer — the overwhelmingly common
// one, and the one every existing harness asserts against — does not change by
// a single byte.
func TestWhyTask_UnfannedOutputIsByteIdenticalGolden(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	taskID, taskRunID := seedUnfannedWhyTask(t, db, jobID, runID, "extract")

	exp, err := store.WhyTask(ctx, runID, "extract")
	require.NoError(t, err)

	raw, err := json.Marshal(exp)
	require.NoError(t, err)

	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &got))

	// Exactly the pre-fan-out key set: no "partition", no "group", and
	// "taskRunId" still present and still a plain UUID string.
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	require.ElementsMatch(t,
		[]string{"runId", "jobId", "taskId", "taskName", "taskRunId", "verdict", "status",
			"cacheEnabled", "hash", "summary", "trigger", "baseline", "diff"},
		keys,
		"unfanned why output gained or lost a top-level key")

	require.JSONEq(t, `"`+taskRunID.String()+`"`, string(got["taskRunId"]))
	require.JSONEq(t, `"`+taskID.String()+`"`, string(got["taskId"]))
	require.NotContains(t, string(raw), `"partition"`)
	require.NotContains(t, string(raw), `"group"`)
}

// TestWhyTask_BaselineIsScopedToTheSamePartition proves the prior-run baseline
// for a fanned instance is that partition's earlier row — not a sibling's.
// Diffing partition "a" against partition "c" would report every per-partition
// input as a discriminating change and make every explanation useless.
func TestWhyTask_BaselineIsScopedToTheSamePartition(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := uuid.New()
	priorRunID, subjectRunID := uuid.New(), uuid.New()
	base := time.Now().UTC().Add(-time.Hour)

	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "process"}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID: priorRunID, JobID: jobID, Status: string(StatusSucceeded), TriggerType: "cron", StartedAt: base,
	}).Error)

	// Prior run: partition "a" saw row_count 100, partition "b" saw 999.
	priorRows := map[string]string{"a": "100", "b": "999"}
	idx := 0
	priorIDs := map[string]uuid.UUID{}
	for _, value := range []string{"a", "b"} {
		hi := cache.HashInput{TaskName: "process", Image: "alpine:3.23", Partition: value,
			PredecessorOutputs: map[string]map[string]string{"list": {"row_count": priorRows[value]}}}
		id := uuid.New()
		priorIDs[value] = id
		require.NoError(t, db.Create(&models.TaskRun{
			ID: id, JobRunID: priorRunID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
			Status: string(TaskStatusSucceeded), CacheEnabled: true,
			Hash: hi.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
			PartitionValue: value, PartitionIndex: idx, PartitionCount: 2,
			StartedAt: &base, CreatedAt: base, UpdatedAt: base,
		}).Error)
		idx++
	}

	// Subject run: partition "a" now sees 101.
	later := base.Add(time.Hour)
	require.NoError(t, db.Create(&models.JobRun{
		ID: subjectRunID, JobID: jobID, Status: string(StatusSucceeded), TriggerType: "cron", StartedAt: later,
	}).Error)
	subjectRows := map[string]string{"a": "101", "b": "999"}
	idx = 0
	for _, value := range []string{"a", "b"} {
		hi := cache.HashInput{TaskName: "process", Image: "alpine:3.23", Partition: value,
			PredecessorOutputs: map[string]map[string]string{"list": {"row_count": subjectRows[value]}}}
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: subjectRunID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
			Status: string(TaskStatusSucceeded), CacheEnabled: true,
			Hash: hi.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
			PartitionValue: value, PartitionIndex: idx, PartitionCount: 2,
			StartedAt: &later, CreatedAt: later, UpdatedAt: later,
		}).Error)
		idx++
	}

	exp, err := store.WhyTaskPartition(ctx, subjectRunID, "process", "a")
	require.NoError(t, err)
	require.Equal(t, "prior_run", exp.Baseline.Kind)
	require.NotNil(t, exp.Baseline.TaskRunID)
	require.Equal(t, priorIDs["a"], *exp.Baseline.TaskRunID,
		"partition a must be compared against partition a's prior row")

	c, ok := findChange(exp.Diff.Changes, "predecessorOutputs.list.row_count")
	require.True(t, ok, "expected the row_count change, got %+v", exp.Diff.Changes)
	require.Equal(t, "100", c.Before)
	require.Equal(t, "101", c.After)

	// Partition "b" is unchanged between the runs, so it must report no change —
	// which is only true if it was compared against partition b's own prior row.
	expB, err := store.WhyTaskPartition(ctx, subjectRunID, "process", "b")
	require.NoError(t, err)
	require.True(t, expB.Diff.HashEqual, "unchanged partition b must diff clean: %+v", expB.Diff.Changes)
}

func TestWhyTask_SinglePartitionGroupIsStillAGroup(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID, runID, taskID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusSucceeded), TriggerType: "cron", StartedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "process"}).Error)
	hi := cache.HashInput{TaskName: "process", Image: "alpine:3.23", Partition: "only"}
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusSucceeded), CacheEnabled: true,
		Hash: hi.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
		PartitionValue: "only", PartitionIndex: 0, PartitionCount: 1,
		StartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	exp, err := store.WhyTask(ctx, runID, "process")
	require.NoError(t, err)
	require.NotNil(t, exp.Group, "an N=1 fan-out is still fanned; it must not masquerade as an ordinary task")
	require.Equal(t, 1, exp.Group.PartitionCount)
	require.Equal(t, []string{"only"}, exp.Group.Partitions)
}

func TestClassifyGroupVerdict(t *testing.T) {
	cases := []struct {
		name                              string
		cacheEnabled, terminal, allCached bool
		want                              WhyVerdict
	}{
		{"still running", true, false, false, VerdictUnknown},
		{"cache off", false, true, false, VerdictCacheOff},
		{"every instance cached", true, true, true, VerdictCacheHit},
		{"partially cached is a miss", true, true, false, VerdictCacheMiss},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyGroupVerdict(tc.cacheEnabled, tc.terminal, tc.allCached))
		})
	}
}

// seedUnfannedWhyTask writes one ordinary (unfanned) task run and returns its
// task and task-run ids.
func seedUnfannedWhyTask(t *testing.T, db *gorm.DB, jobID, runID uuid.UUID, name string) (taskID, taskRunID uuid.UUID) {
	t.Helper()
	taskID, taskRunID = uuid.New(), uuid.New()
	now := time.Now().UTC()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusSucceeded),
		TriggerType: "manual", StartedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: name}).Error)

	hi := cache.HashInput{TaskName: name, Image: "alpine:3.23"}
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusSucceeded), CacheEnabled: true,
		Hash: hi.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
		StartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	return taskID, taskRunID
}
