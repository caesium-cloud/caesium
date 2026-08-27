package worker

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/require"
)

// A fanned instance's identity hash is not a caching artifact. It is what makes
// ONE PARTITION addressable: `caesium receipt get`, `caesium why --partition`,
// `run retry --partition` and the receipt's per-partition attestation all match
// on it, and the run receipt asserts every partition is a distinct unit of work
// WITH ITS OWN identity hash.
//
// The local lane already treats it that way — internal/job/job.go computes and
// persists the per-instance hash outside its `if cacheCfg.Enabled` block, and
// gates only the cache lookup and the publish. The worker gated the whole
// identity block on caching, so a distributed run with the cache off left every
// instance row's hash empty and the partitions of a group stopped being
// distinguishable units of work.

// TestWorkerPersistsInstanceIdentityWithCachingDisabled is the load-bearing
// case: caching OFF, and the executed instance must still carry its own
// identity.
func TestWorkerPersistsInstanceIdentityWithCachingDisabled(t *testing.T) {
	part := pkgtask.Partition{Key: "shard-0", Attributes: map[string]string{"rows": "100"}}
	f := seedFanOutTaskRun(t, "fanout-identity-nocache-job", `{"from":"producer"}`, part, 3, false)
	require.False(t, f.taskRun.CacheEnabled, "this scenario is about the cache being OFF")

	executeCapturingEnv(t, f)

	var executed models.TaskRun
	require.NoError(t, f.db.First(&executed, "id = ?", f.taskRun.ID).Error)
	require.NotEmpty(t, executed.Hash,
		"a fanned instance must persist its identity hash whether or not caching is on; "+
			"without it the run receipt cannot attest the partition as its own unit of work")
	require.NotEmpty(t, executed.HashInputBlob,
		"the identity's decomposition must be persisted too, or `caesium why --partition` cannot explain it")

	// The write is still addressed to ONE row.
	var others []models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ? AND id <> ?", f.jobRun.ID, f.task.ID, f.taskRun.ID).
		Find(&others).Error)
	require.Len(t, others, 2)
	for _, row := range others {
		require.Empty(t, row.Hash,
			"partition %s did not execute and must not inherit a sibling's identity", row.PartitionValue)
	}

	// Persisting the identity must not have switched caching back on: nothing
	// may be looked up or published when the step disabled it.
	entries, err := cache.NewStore(f.db).ListByJob(f.jobRun.JobID)
	require.NoError(t, err)
	require.Empty(t, entries,
		"caching is off; the identity write must not publish a cache entry")
}

// TestWorkerInstanceIdentityIsRealWithCachingDisabled pins that the hash written
// with caching off is the SAME per-partition identity, not a constant that
// happens to be non-empty: two instances differing only in their partition
// attributes must hash differently, exactly as they do with caching on
// (TestWorkerCacheIdentityIncludesPartitionAttributes).
func TestWorkerInstanceIdentityIsRealWithCachingDisabled(t *testing.T) {
	hashFor := func(attrs map[string]string) string {
		part := pkgtask.Partition{Key: "shard-1", Attributes: attrs}
		f := seedFanOutTaskRun(t, "fanout-identity-real-job", `{"from":"producer"}`, part, 1, false)
		executeCapturingEnv(t, f)

		var row models.TaskRun
		require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
		require.NotEmpty(t, row.Hash)
		return row.Hash
	}

	require.NotEqual(t, hashFor(map[string]string{"rows": "100"}), hashFor(map[string]string{"rows": "999"}),
		"the identity written with caching off must still be derived from the partition")
}

// TestWorkerUnfannedTaskKeepsCacheGatedIdentity pins the OTHER half of matching
// the local lane: an unfanned task's hash write stays gated on caching, because
// that is what internal/job/job.go does for a non-fanned step. Un-gating it here
// would make the two lanes disagree in the opposite direction.
func TestWorkerUnfannedTaskKeepsCacheGatedIdentity(t *testing.T) {
	f := seedFanOutTaskRun(t, "unfanned-identity-job", "", pkgtask.Partition{}, 1, false)
	// seedFanOutTaskRun stamps PartitionCount from `siblings`; an unfanned row
	// carries neither a partition value nor a count.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.taskRun.ID).Updates(map[string]any{
		"partition_value": "",
		"partition_count": 0,
	}).Error)
	require.NoError(t, f.db.First(f.taskRun, "id = ?", f.taskRun.ID).Error)

	executeCapturingEnv(t, f)

	var executed models.TaskRun
	require.NoError(t, f.db.First(&executed, "id = ?", f.taskRun.ID).Error)
	require.Empty(t, executed.Hash,
		"an unfanned task with caching off must keep the local lane's behaviour: no identity write")
}
