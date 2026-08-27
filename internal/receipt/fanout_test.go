package receipt

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fanout_test.go pins the receipt's fan-out contract: a fanned step contributes
// one attested entry PER PARTITION, and an unfanned run's digest is unchanged by
// the schema addition (no Version bump, so receipts committed before fan-out
// shipped still verify).

func seedFanOutRun(t *testing.T, db *gorm.DB, jobID, runID uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: "succeeded", StartedAt: now,
	}).Error)

	taskID := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "process"}).Error)

	for i, value := range []string{"a", "b", "c"} {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "echo",
			Status: "succeeded",
			// Distinct identity hashes: that IS the point — each partition is a
			// different unit of work with its own cache key.
			Hash:                "hash-" + value,
			ResolvedImageDigest: "sha256:" + value,
			PartitionValue:      value, PartitionIndex: i, PartitionCount: 3,
			Attempt: 1, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	return taskID
}

// TestBuild_FannedStepAttestsEveryPartition is the regression this file exists
// for: terminalAttempts used to collapse by TaskID alone, so a 3-partition step
// produced ONE entry — the receipt silently attested a single arbitrary
// partition and dropped two thirds of the run's work.
func TestBuild_FannedStepAttestsEveryPartition(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID, runID := uuid.New(), uuid.New()
	seedFanOutRun(t, db, jobID, runID)

	r, err := Build(context.Background(), db, runID)
	require.NoError(t, err)

	require.Len(t, r.Tasks, 3, "one entry per partition, not one per task")
	require.Equal(t, []string{"a", "b", "c"},
		[]string{r.Tasks[0].Partition, r.Tasks[1].Partition, r.Tasks[2].Partition},
		"entries sort by (task, partition) so the aggregation is deterministic")
	for _, entry := range r.Tasks {
		require.Equal(t, "process", entry.TaskName)
		require.Equal(t, "hash-"+entry.Partition, entry.IdentityHash)
		require.Equal(t, "process["+entry.Partition+"]", entry.Label())
	}
	require.False(t, r.Degraded, "every instance is digest-pinned")
}

// TestTerminalAttempts_KeyIsTaskPlusPartition pins the collapse key directly.
// Keyed on TaskID alone (the pre-fan-out behavior) three sibling partitions
// collapse to one entry; keyed on (TaskID, PartitionValue) they stay three while
// two ATTEMPTS of the same partition still collapse to the terminal one.
//
// It exercises terminalAttempts rather than Build because the task_runs unique
// index is (job_run_id, task_id, partition_index): a retry updates its row in
// place rather than inserting a second one, so the retry case is only reachable
// through the function itself (and through legacy rows written before that
// index existed).
func TestTerminalAttempts_KeyIsTaskPlusPartition(t *testing.T) {
	taskID := uuid.New()
	now := time.Now().UTC()
	mk := func(partition string, attempt int, hash string) models.TaskRun {
		return models.TaskRun{
			ID: uuid.New(), TaskID: taskID, PartitionValue: partition,
			Attempt: attempt, Hash: hash, UpdatedAt: now.Add(time.Duration(attempt) * time.Minute),
		}
	}

	got := terminalAttempts([]models.TaskRun{
		mk("a", 1, "ha"),
		mk("b", 1, "hb-1"),
		mk("b", 2, "hb-2"),
		mk("c", 1, "hc"),
	})

	require.Len(t, got, 3, "three partitions, with b's two attempts collapsed")
	hashes := map[string]string{}
	for _, row := range got {
		hashes[row.PartitionValue] = row.Hash
	}
	require.Equal(t, map[string]string{"a": "ha", "b": "hb-2", "c": "hc"}, hashes)
}

// TestCanonicalTaskLine_UnfannedBytesUnchanged is the compatibility guard: the
// partition segment is appended ONLY for a fan-out instance, so every digest
// committed before fan-out shipped still re-derives identically and Version
// stays at 1.
func TestCanonicalTaskLine_UnfannedBytesUnchanged(t *testing.T) {
	entry := TaskEntry{
		TaskName: "extract", IdentityHash: "abc", Image: "alpine:3.23",
		ResolvedImageDigest: "sha256:deadbeef", DigestPinned: true,
	}
	require.Equal(t,
		"task\x00extract\x00hash\x00abc\x00image\x00alpine:3.23\x00digest\x00sha256:deadbeef\x00pinned\x00true\n",
		canonicalTaskLine(entry))

	fanned := entry
	fanned.Partition = "a"
	require.Equal(t,
		"task\x00extract\x00hash\x00abc\x00image\x00alpine:3.23\x00digest\x00sha256:deadbeef\x00pinned\x00true\x00partition\x00a\n",
		canonicalTaskLine(fanned))
	require.Equal(t, 1, Version, "adding an omit-when-empty segment must not bump the schema version")
}

// TestComputeDigest_PartitionsAreDistinguished proves two partitions of one step
// cannot collide into the same digest contribution — which is what would happen
// if the partition were left out of the canonical line while both entries shared
// a task name.
func TestComputeDigest_PartitionsAreDistinguished(t *testing.T) {
	a := TaskEntry{TaskName: "process", Partition: "a", IdentityHash: "h", Image: "img", DigestPinned: true, ResolvedImageDigest: "sha256:x"}
	b := a
	b.Partition = "b"

	require.NotEqual(t,
		computeDigest([]TaskEntry{a, a}, "", ""),
		computeDigest([]TaskEntry{a, b}, "", ""),
		"a receipt over partitions a+b must not equal one over a+a")
}

// TestDiffTasks_DriftInOnePartitionIsDetected is the verify-side half: pairing
// entries by name alone collapsed a fanned step's N entries onto one map slot,
// so a moved image tag on partition "b" verified CLEAN.
func TestDiffTasks_DriftInOnePartitionIsDetected(t *testing.T) {
	committed := &Receipt{Tasks: []TaskEntry{
		{TaskName: "process", Partition: "a", IdentityHash: "ha", Image: "img", ResolvedImageDigest: "sha256:a", DigestPinned: true},
		{TaskName: "process", Partition: "b", IdentityHash: "hb", Image: "img", ResolvedImageDigest: "sha256:b", DigestPinned: true},
	}}
	committed.finalize()

	rederived := &Receipt{Tasks: []TaskEntry{
		{TaskName: "process", Partition: "a", IdentityHash: "ha", Image: "img", ResolvedImageDigest: "sha256:a", DigestPinned: true},
		{TaskName: "process", Partition: "b", IdentityHash: "hb", Image: "img", ResolvedImageDigest: "sha256:MOVED", DigestPinned: true},
	}}
	rederived.finalize()

	drifts := diffTasks(committed, rederived)
	require.Len(t, drifts, 1)
	require.Equal(t, DriftImageDigest, drifts[0].Kind)
	require.Equal(t, "process[b]", drifts[0].Task, "drift must name the instance, not just the step")
}

// TestDiffTasks_VanishedPartitionIsReported: a partition present in the
// committed receipt but absent from current state is a task_missing drift keyed
// to the instance.
func TestDiffTasks_VanishedPartitionIsReported(t *testing.T) {
	committed := &Receipt{Tasks: []TaskEntry{
		{TaskName: "process", Partition: "a", IdentityHash: "ha", Image: "img", ResolvedImageDigest: "sha256:a", DigestPinned: true},
		{TaskName: "process", Partition: "b", IdentityHash: "hb", Image: "img", ResolvedImageDigest: "sha256:b", DigestPinned: true},
	}}
	committed.finalize()

	rederived := &Receipt{Tasks: committed.Tasks[:1]}
	rederived.finalize()

	drifts := diffTasks(committed, rederived)
	require.Len(t, drifts, 1)
	require.Equal(t, DriftTaskMissing, drifts[0].Kind)
	require.Equal(t, "process[b]", drifts[0].Task)
}

// TestFinalize_DegradedTasksNamePartitions keeps the degraded summary usable on
// a fanned run: "process, process, process" would be useless.
func TestFinalize_DegradedTasksNamePartitions(t *testing.T) {
	r := &Receipt{Tasks: []TaskEntry{
		{TaskName: "process", Partition: "a", IdentityHash: "ha", Image: "vendor/extract:latest"},
		{TaskName: "process", Partition: "b", IdentityHash: "hb", Image: "vendor/extract:latest"},
	}}
	for i := range r.Tasks {
		markDegraded(&r.Tasks[i])
	}
	r.finalize()

	require.True(t, r.Degraded)
	require.Equal(t, []string{"process[a]", "process[b]"}, r.DegradedTasks)
}
