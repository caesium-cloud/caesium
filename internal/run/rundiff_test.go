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

func TestDiffRuns_RunParamChangeSurfacesOnTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := seedRunDiffTask(t, db, jobID, "load")
	leftRunID := seedRunDiffRun(t, db, jobID, time.Now().UTC(), "cron", "nightly", map[string]string{"region": "us"})
	rightRunID := seedRunDiffRun(t, db, jobID, time.Now().UTC().Add(time.Hour), "cron", "nightly", map[string]string{"region": "eu"})

	leftHI := cache.HashInput{TaskName: "load", Image: "alpine:3.23", RunParams: map[string]string{"region": "us"}}
	rightHI := cache.HashInput{TaskName: "load", Image: "alpine:3.23", RunParams: map[string]string{"region": "eu"}}
	seedRunDiffTaskRun(t, db, leftRunID, taskID, 1, leftHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, taskID, 1, rightHI, string(TaskStatusSucceeded), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Len(t, diff.Tasks, 1)
	require.Equal(t, RunDiffVerdictReran, diff.Tasks[0].Verdict)

	taskChange, ok := findChange(diff.Tasks[0].Changes, "runParams.region")
	require.True(t, ok, "expected task-level run param change, got %+v", diff.Tasks[0].Changes)
	require.Equal(t, "us", taskChange.Before)
	require.Equal(t, "eu", taskChange.After)

	paramChange, ok := findChange(diff.ParamChanges, "params.region")
	require.True(t, ok, "expected run-level param change, got %+v", diff.ParamChanges)
	require.Equal(t, "us", paramChange.Before)
	require.Equal(t, "eu", paramChange.After)
}

func TestDiffRuns_IdenticalInputsWouldCacheHit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	extractID := seedRunDiffTask(t, db, jobID, "extract")
	loadID := seedRunDiffTask(t, db, jobID, "load")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "manual", "", map[string]string{"batch": "42"})
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "", map[string]string{"batch": "42"})

	extractHI := cache.HashInput{TaskName: "extract", Image: "alpine:3.23", RunParams: map[string]string{"batch": "42"}}
	loadHI := cache.HashInput{
		TaskName: "load",
		Image:    "alpine:3.23",
		PredecessorOutputs: map[string]map[string]string{
			"extract": {"rows": "10"},
		},
		RunParams: map[string]string{"batch": "42"},
	}
	seedRunDiffTaskRun(t, db, leftRunID, extractID, 1, extractHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, leftRunID, loadID, 1, loadHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, extractID, 1, extractHI, string(TaskStatusCached), nil)
	seedRunDiffTaskRun(t, db, rightRunID, loadID, 1, loadHI, string(TaskStatusCached), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Empty(t, diff.TasksAdded)
	require.Empty(t, diff.TasksRemoved)
	require.Empty(t, diff.ParamChanges)
	require.Len(t, diff.Tasks, 2)
	for _, task := range diff.Tasks {
		require.Equal(t, RunDiffVerdictWouldCacheHit, task.Verdict, "task=%s", task.TaskName)
		require.True(t, task.HashEqual, "task=%s", task.TaskName)
		require.Empty(t, task.Changes, "task=%s", task.TaskName)
		require.Empty(t, task.Degraded, "task=%s", task.TaskName)
	}
}

func TestDiffRuns_TasksAddedAndRemoved(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	sharedID := seedRunDiffTask(t, db, jobID, "shared")
	oldID := seedRunDiffTask(t, db, jobID, "old")
	newID := seedRunDiffTask(t, db, jobID, "new")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "manual", "", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "", nil)

	sharedHI := cache.HashInput{TaskName: "shared", Image: "alpine:3.23"}
	oldHI := cache.HashInput{TaskName: "old", Image: "alpine:3.23"}
	newHI := cache.HashInput{TaskName: "new", Image: "alpine:3.23"}
	seedRunDiffTaskRun(t, db, leftRunID, sharedID, 1, sharedHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, sharedID, 1, sharedHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, leftRunID, oldID, 1, oldHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, newID, 1, newHI, string(TaskStatusSucceeded), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Equal(t, []string{"new"}, diff.TasksAdded)
	require.Equal(t, []string{"old"}, diff.TasksRemoved)
	require.Len(t, diff.Tasks, 1)
	require.Equal(t, "shared", diff.Tasks[0].TaskName)
}

func TestDiffRuns_DegradedBlobDoesNotStopRestOfDiff(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	goodID := seedRunDiffTask(t, db, jobID, "good")
	degradedID := seedRunDiffTask(t, db, jobID, "degraded")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "manual", "", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "", nil)

	goodHI := cache.HashInput{TaskName: "good", Image: "alpine:3.23"}
	degradedHI := cache.HashInput{TaskName: "degraded", Image: "alpine:3.23"}
	versionMismatchBlob := withBlobVersion(t, blobBytes(t, degradedHI), 999)

	seedRunDiffTaskRun(t, db, leftRunID, goodID, 1, goodHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, goodID, 1, goodHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, leftRunID, degradedID, 1, degradedHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, degradedID, 1, degradedHI, string(TaskStatusSucceeded), versionMismatchBlob)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Len(t, diff.Tasks, 2)

	byName := map[string]RunDiffTask{}
	for _, task := range diff.Tasks {
		byName[task.TaskName] = task
	}
	require.Equal(t, RunDiffVerdictWouldCacheHit, byName["good"].Verdict)
	require.Empty(t, byName["good"].Degraded)
	require.Equal(t, RunDiffVerdictDegraded, byName["degraded"].Verdict)
	require.Contains(t, byName["degraded"].Degraded, "blob version mismatch")
}

func TestDiffRuns_UsesLatestTerminalAttemptByTaskName(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := seedRunDiffTask(t, db, jobID, "retrying")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "manual", "", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "", nil)

	oldAttemptHI := cache.HashInput{TaskName: "retrying", Image: "busybox:1.36.1"}
	latestHI := cache.HashInput{TaskName: "retrying", Image: "alpine:3.23"}
	seedRunDiffTaskRun(t, db, leftRunID, taskID, 1, latestHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, taskID, 1, oldAttemptHI, string(TaskStatusFailed), nil)
	seedRunDiffTaskRun(t, db, rightRunID, taskID, 2, latestHI, string(TaskStatusSucceeded), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Len(t, diff.Tasks, 1)
	require.Equal(t, 2, diff.Tasks[0].RightAttempt)
	require.Equal(t, RunDiffVerdictWouldCacheHit, diff.Tasks[0].Verdict)
}

func TestDiffRuns_CorruptBlobDegradesOnlyThatTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	goodID := seedRunDiffTask(t, db, jobID, "good")
	corruptID := seedRunDiffTask(t, db, jobID, "corrupt")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "manual", "", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "", nil)

	goodHI := cache.HashInput{TaskName: "good", Image: "alpine:3.23"}
	corruptHI := cache.HashInput{TaskName: "corrupt", Image: "alpine:3.23"}

	seedRunDiffTaskRun(t, db, leftRunID, goodID, 1, goodHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, goodID, 1, goodHI, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, leftRunID, corruptID, 1, corruptHI, string(TaskStatusSucceeded), nil)
	// A non-empty, non-JSON persisted blob makes DiffHashInputBlobs return a
	// decode error (distinct from the missing/oversized/version-mismatch
	// degrade paths). It must degrade only this task, not abort the run diff.
	seedRunDiffTaskRun(t, db, rightRunID, corruptID, 1, corruptHI, string(TaskStatusSucceeded), []byte("}{ not valid json"))

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Len(t, diff.Tasks, 2)

	byName := map[string]RunDiffTask{}
	for _, task := range diff.Tasks {
		byName[task.TaskName] = task
	}
	require.Equal(t, RunDiffVerdictWouldCacheHit, byName["good"].Verdict)
	require.Empty(t, byName["good"].Degraded)
	require.Equal(t, RunDiffVerdictDegraded, byName["corrupt"].Verdict)
	require.Contains(t, byName["corrupt"].Degraded, "decode hash-input blob")
}

func TestDiffRuns_TriggerChangeSurfaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := seedRunDiffTask(t, db, jobID, "load")
	base := time.Now().UTC()
	leftRunID := seedRunDiffRun(t, db, jobID, base, "cron", "nightly", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "manual", "adhoc", nil)

	hi := cache.HashInput{TaskName: "load", Image: "alpine:3.23"}
	seedRunDiffTaskRun(t, db, leftRunID, taskID, 1, hi, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, taskID, 1, hi, string(TaskStatusSucceeded), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)

	typeChange, ok := findChange(diff.TriggerChanges, "trigger.type")
	require.True(t, ok, "expected trigger.type change, got %+v", diff.TriggerChanges)
	require.Equal(t, "cron", typeChange.Before)
	require.Equal(t, "manual", typeChange.After)

	aliasChange, ok := findChange(diff.TriggerChanges, "trigger.alias")
	require.True(t, ok, "expected trigger.alias change, got %+v", diff.TriggerChanges)
	require.Equal(t, "nightly", aliasChange.Before)
	require.Equal(t, "adhoc", aliasChange.After)
}

func seedRunDiffRun(t *testing.T, db *gorm.DB, jobID uuid.UUID, startedAt time.Time, triggerType, triggerAlias string, params map[string]string) uuid.UUID {
	t.Helper()

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:           runID,
		JobID:        jobID,
		Status:       string(StatusSucceeded),
		TriggerType:  triggerType,
		TriggerAlias: triggerAlias,
		Params:       mustJSON(t, params),
		StartedAt:    startedAt,
		CreatedAt:    startedAt,
		UpdatedAt:    startedAt,
	}).Error)
	return runID
}

func seedRunDiffTask(t *testing.T, db *gorm.DB, jobID uuid.UUID, name string) uuid.UUID {
	t.Helper()

	taskID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Task{
		ID:        taskID,
		JobID:     jobID,
		AtomID:    uuid.New(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return taskID
}

func seedRunDiffTaskRun(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID, attempt int, input cache.HashInput, status string, blobOverride []byte) uuid.UUID {
	t.Helper()

	id := uuid.New()
	now := time.Now().UTC().Add(time.Duration(attempt) * time.Second)
	blob := blobOverride
	if blob == nil {
		blob = blobBytes(t, input)
	}
	hash := input.Compute()
	require.NoError(t, db.Create(&models.TaskRun{
		ID:               id,
		JobRunID:         runID,
		TaskID:           taskID,
		AtomID:           uuid.New(),
		Engine:           models.AtomEngineDocker,
		Image:            input.Image,
		Command:          `["echo"]`,
		Status:           status,
		Attempt:          attempt,
		MaxAttempts:      attempt,
		CacheEnabled:     true,
		Hash:             hash,
		HashInputBlob:    datatypes.JSON(blob),
		TerminalSequence: int64(attempt),
		StartedAt:        &now,
		CompletedAt:      &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error)
	return id
}

func mustJSON(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return datatypes.JSON(data)
}

func withBlobVersion(t *testing.T, blob []byte, version int) []byte {
	t.Helper()

	var raw map[string]any
	require.NoError(t, json.Unmarshal(blob, &raw))
	raw["blobVersion"] = version
	out, err := json.Marshal(raw)
	require.NoError(t, err)
	return out
}
