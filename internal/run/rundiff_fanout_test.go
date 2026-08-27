package run

import (
	"context"
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

// rundiff_fanout_test.go proves the run diff aligns fan-out instances by
// partition VALUE and never by index. Index alignment looks right until a
// producer emits the same units in a different order (or drops one), at which
// point every partition reads as both added and removed and every hash reads as
// changed — the diff becomes noise exactly when an operator needs it most.

// seedFanOutDiffInstance writes one instance of a fanned task. index is the
// emission order WITHIN that run, deliberately decoupled from the partition
// value so the alignment rule is observable.
func seedFanOutDiffInstance(
	t *testing.T,
	db *gorm.DB,
	runID, taskID uuid.UUID,
	partition string,
	index int,
	input cache.HashInput,
	status string,
) uuid.UUID {
	t.Helper()

	id := uuid.New()
	now := time.Now().UTC().Add(time.Duration(index) * time.Second)
	input.Partition = partition
	require.NoError(t, db.Create(&models.TaskRun{
		ID:               id,
		JobRunID:         runID,
		TaskID:           taskID,
		AtomID:           uuid.New(),
		Engine:           models.AtomEngineDocker,
		Image:            input.Image,
		Command:          `["echo"]`,
		Status:           status,
		Attempt:          1,
		MaxAttempts:      1,
		CacheEnabled:     true,
		Hash:             input.Compute(),
		HashInputBlob:    datatypes.JSON(blobBytes(t, input)),
		TerminalSequence: int64(index + 1),
		PartitionValue:   partition,
		PartitionIndex:   index,
		PartitionCount:   3,
		StartedAt:        &now,
		CompletedAt:      &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error)
	return id
}

func TestDiffRuns_FanOutAlignsByPartitionValueNotIndex(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	base := time.Now().UTC().Add(-time.Hour)
	leftRunID := seedRunDiffRun(t, db, jobID, base, "cron", "nightly", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "cron", "nightly", nil)
	taskID := seedRunDiffTask(t, db, jobID, "process")

	rows := func(partition, rowCount string) cache.HashInput {
		return cache.HashInput{
			TaskName: "process", Image: "alpine:3.23",
			PredecessorOutputs: map[string]map[string]string{"list": {"row_count": rowCount}},
		}
	}

	// Left run emitted a, b, c (indices 0,1,2).
	seedFanOutDiffInstance(t, db, leftRunID, taskID, "a", 0, rows("a", "1"), string(TaskStatusSucceeded))
	seedFanOutDiffInstance(t, db, leftRunID, taskID, "b", 1, rows("b", "2"), string(TaskStatusSucceeded))
	seedFanOutDiffInstance(t, db, leftRunID, taskID, "c", 2, rows("c", "3"), string(TaskStatusSucceeded))

	// Right run emitted d, c, b (indices 0,1,2): "a" is gone, "d" is new, and the
	// shared partitions arrive in a DIFFERENT order. b is unchanged; c's upstream
	// row count moved 3 -> 4.
	seedFanOutDiffInstance(t, db, rightRunID, taskID, "d", 0, rows("d", "9"), string(TaskStatusSucceeded))
	seedFanOutDiffInstance(t, db, rightRunID, taskID, "c", 1, rows("c", "4"), string(TaskStatusSucceeded))
	seedFanOutDiffInstance(t, db, rightRunID, taskID, "b", 2, rows("b", "2"), string(TaskStatusSucceeded))

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)

	require.Equal(t, []string{"process:d"}, diff.PartitionsAdded)
	require.Equal(t, []string{"process:a"}, diff.PartitionsRemoved)
	require.Empty(t, diff.TasksAdded, "partition churn is not task churn")
	require.Empty(t, diff.TasksRemoved)

	byPartition := map[string]RunDiffTask{}
	for _, task := range diff.Tasks {
		require.Equal(t, "process", task.TaskName)
		byPartition[task.Partition] = task
	}
	require.Len(t, byPartition, 2, "only the shared partitions pair up: %+v", diff.Tasks)

	require.Equal(t, RunDiffVerdictWouldCacheHit, byPartition["b"].Verdict,
		"partition b is byte-identical across runs; index alignment would have paired it with c")
	require.True(t, byPartition["b"].HashEqual)

	require.Equal(t, RunDiffVerdictReran, byPartition["c"].Verdict)
	change, ok := findChange(byPartition["c"].Changes, "predecessorOutputs.list.row_count")
	require.True(t, ok, "expected c's changed row_count, got %+v", byPartition["c"].Changes)
	require.Equal(t, "3", change.Before)
	require.Equal(t, "4", change.After)
}

// TestDiffRuns_UnfannedDiffReportsNoPartitionChurn keeps the unfanned answer
// unchanged: no partition fields anywhere.
func TestDiffRuns_UnfannedDiffReportsNoPartitionChurn(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	base := time.Now().UTC().Add(-time.Hour)
	leftRunID := seedRunDiffRun(t, db, jobID, base, "cron", "nightly", nil)
	rightRunID := seedRunDiffRun(t, db, jobID, base.Add(time.Hour), "cron", "nightly", nil)
	taskID := seedRunDiffTask(t, db, jobID, "extract")

	input := cache.HashInput{TaskName: "extract", Image: "alpine:3.23"}
	seedRunDiffTaskRun(t, db, leftRunID, taskID, 1, input, string(TaskStatusSucceeded), nil)
	seedRunDiffTaskRun(t, db, rightRunID, taskID, 1, input, string(TaskStatusSucceeded), nil)

	diff, err := store.DiffRuns(ctx, jobID, leftRunID, rightRunID)
	require.NoError(t, err)
	require.Empty(t, diff.PartitionsAdded)
	require.Empty(t, diff.PartitionsRemoved)
	require.Len(t, diff.Tasks, 1)
	require.Empty(t, diff.Tasks[0].Partition)
}
