package job

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// fanout_logs_test.go pins the WRITE half of the fan-out log contract: each
// instance's container output must land on ITS OWN task_runs row.
//
// The read half (api/rest/controller/job/run/logs.go) serves this snapshot
// whenever no live container stream is available — which, since every engine's
// Stop is stop-AND-remove, is every request for a task that has finished. If the
// snapshot were broadcast across siblings (the pre-fan-out
// `WHERE job_run_id = ? AND task_id = ?` write) or skipped for fanned
// instances, every partition's log would read back identical or empty, and the
// only surface that would notice is an end-to-end log fetch.

// TestFanOutLocalPersistsPerInstanceLogSnapshot: N partitions, N distinct
// container logs, N distinct log_text values.
func TestFanOutLocalPersistsPerInstanceLogSnapshot(t *testing.T) {
	partitions := []string{"a", "b", "c"}
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	for _, p := range partitions {
		f.engine.logsByPartition[p] = "partition=" + p + "\n"
	}

	require.NoError(t, f.run(t, defaultFanOutVars()))

	rows := f.instanceRows(t)
	require.Len(t, rows, len(partitions))

	seen := map[string]string{}
	for _, row := range rows {
		require.Equal(t, string(run.TaskStatusSucceeded), row.Status, "partition %s", row.PartitionValue)
		require.NotEmpty(t, row.LogText,
			"partition %s persisted no log snapshot; the logs endpoint has nothing to serve once the container is removed",
			row.PartitionValue)
		require.Contains(t, row.LogText, "partition="+row.PartitionValue,
			"partition %s got a sibling's log", row.PartitionValue)
		require.False(t, row.LogTruncated)
		seen[row.PartitionValue] = row.LogText
	}
	require.Len(t, seen, len(partitions), "each instance must carry its own snapshot, not a shared one")
}

// TestFanOutLocalPersistsLogSnapshotOnFailedInstance: the failing partition's
// output is the whole point of fetching its log, so a non-zero exit must not
// cost the snapshot — and must not overwrite its succeeded siblings'.
func TestFanOutLocalPersistsLogSnapshotOnFailedInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	for _, p := range []string{"a", "b", "c"} {
		f.engine.logsByPartition[p] = "partition=" + p + "\n"
	}
	f.engine.resultByPartition["b"] = "failure"

	_ = f.run(t, defaultFanOutVars())

	rows := f.instanceRows(t)
	require.Len(t, rows, 3)

	byPartition := map[string]models.TaskRun{}
	for _, row := range rows {
		byPartition[row.PartitionValue] = row
	}

	failed := byPartition["b"]
	require.Equal(t, string(run.TaskStatusFailed), failed.Status)
	require.Contains(t, failed.LogText, "partition=b",
		"a failed instance must still persist its own log")

	for _, p := range []string{"a", "c"} {
		require.Contains(t, byPartition[p].LogText, "partition="+p,
			"the failed instance's snapshot must not have overwritten sibling %s", p)
	}
}
