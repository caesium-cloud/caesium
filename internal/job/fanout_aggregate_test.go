package job

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fanout_aggregate_test.go pins how a fanned step's fan-in aggregate and
// aggregate identity hash are rebuilt when the group is re-entered.
//
// runFannedGroup's byPartition / hashByInstance maps only ever describe the
// instances THIS invocation dispatched. After a retry that preserved the
// succeeded siblings — `caesium run retry --partition`, or RetryFromFailure —
// that is a single instance, so the rebuilt aggregate reported the outputs of
// one partition out of N and the group hash folded one instance instead of N.
// Both are read by downstream steps: the first as CAESIUM_OUTPUT_<STEP>_<KEY>,
// the second as the fanned predecessor's single PredecessorHashes entry. So a
// retry silently changed what the downstream saw AND re-keyed its whole cached
// subtree.

// downstreamBlob decodes the persisted hash-input blob of the fixture's
// `publish` step, which is where the values a downstream folded in are
// observable after the fact.
func downstreamBlob(t *testing.T, f *fanOutFixture, runID uuid.UUID) struct {
	PredecessorHashes  []string                     `json:"predecessorHashes"`
	PredecessorOutputs map[string]map[string]string `json:"predecessorOutputs"`
} {
	t.Helper()

	var publish models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND task_id = ?", runID, f.downstream).
		First(&publish).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), publish.Status,
		"the downstream step must have run on the retried run")
	require.NotEmpty(t, publish.HashInputBlob, "downstream step must persist its hash-input blob")

	var blob struct {
		PredecessorHashes  []string                     `json:"predecessorHashes"`
		PredecessorOutputs map[string]map[string]string `json:"predecessorOutputs"`
	}
	require.NoError(t, json.Unmarshal(publish.HashInputBlob, &blob))
	return blob
}

// TestFanOutRetriedGroupRebuildsFullAggregateAndHash is the P1: after a retry
// that re-executed ONE partition, the group's fan-in aggregate must still carry
// every sibling's output and its identity hash must still fold every sibling.
func TestFanOutRetriedGroupRebuildsFullAggregateAndHash(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.addDownstream(t)

	f.engine.logsByPartition["a"] = "##caesium::output {\"rows\":\"1\"}\n"
	f.engine.logsByPartition["b"] = "##caesium::output {\"rows\":\"2\"}\n"
	f.engine.logsByPartition["c"] = "##caesium::output {\"rows\":\"3\"}\n"

	// Partition "b" fails on the first run; its siblings succeed and are
	// preserved by the retry.
	f.engine.createErrByPartition["b"] = fmt.Errorf("boom")
	require.Error(t, f.run(t, defaultFanOutVars()))

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)

	delete(f.engine.createErrByPartition, "b")
	_, retryErr := f.store.RetryFromFailure(jobRun.ID)
	require.NoError(t, retryErr)

	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	require.NoError(t, New(&models.Job{ID: f.jobID}, opts...).Run(context.Background()))

	rows := f.instanceRowsFor(t, jobRun.ID)
	require.Len(t, rows, 3)
	instanceHashes := make([]string, 0, len(rows))
	for _, r := range rows {
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status, "partition %s", r.PartitionValue)
		require.NotEmpty(t, r.Hash, "partition %s must carry its own identity", r.PartitionValue)
		instanceHashes = append(instanceHashes, r.Hash)
	}

	// Only "b" re-executed; a and c kept their first-run containers.
	require.Equal(t, 1, f.engine.createCount("a"))
	require.Equal(t, 2, f.engine.createCount("b"))
	require.Equal(t, 1, f.engine.createCount("c"))

	// The group hash a fresh run would produce: every terminal-success instance
	// in partition-index order. Rebuilt from the observed instances only, this
	// was GroupIdentityHash([b]) instead.
	wantGroupHash := run.GroupIdentityHash(instanceHashes)
	require.NotEmpty(t, wantGroupHash)

	blob := downstreamBlob(t, f, jobRun.ID)
	require.Equal(t, []string{wantGroupHash}, blob.PredecessorHashes,
		"a retried group must present the same aggregate identity a fresh run would")

	// Cross-lane parity: the SQL read path the distributed lane uses agrees.
	fromStore, err := f.store.PredecessorHashes(jobRun.ID, f.downstream)
	require.NoError(t, err)
	require.Equal(t, []string{wantGroupHash}, fromStore)

	// The fan-in aggregate carries every partition, not just the retried one.
	aggregate := blob.PredecessorOutputs["process"]
	require.NotEmpty(t, aggregate, "the downstream must see the fanned predecessor's aggregate")
	require.Equal(t, "3", aggregate["PARTITION_COUNT"],
		"the aggregate must cover all three partitions, not only the one this invocation ran")
	require.Equal(t, "3", aggregate["SUCCEEDED"])
	require.Equal(t, "0", aggregate["FAILED"])

	var rowsByPartition map[string]string
	require.NoError(t, json.Unmarshal([]byte(aggregate["rows"]), &rowsByPartition))
	require.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, rowsByPartition,
		"the preserved siblings' outputs must be hydrated from their rows")
}

// TestFanOutFreshGroupAggregateIsUnchanged is the control: a group that runs
// every instance in one invocation produces exactly the same aggregate and
// group hash it did before the rows became the source of truth.
func TestFanOutFreshGroupAggregateIsUnchanged(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
	}, 0)
	f.addDownstream(t)

	f.engine.logsByPartition["a"] = "##caesium::output {\"rows\":\"1\"}\n"
	f.engine.logsByPartition["b"] = "##caesium::output {\"rows\":\"2\"}\n"
	f.engine.logsByPartition["c"] = "##caesium::output {\"rows\":\"3\"}\n"

	require.NoError(t, f.run(t, defaultFanOutVars()))

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)

	rows := f.instanceRowsFor(t, jobRun.ID)
	require.Len(t, rows, 3)
	instanceHashes := make([]string, 0, len(rows))
	for _, r := range rows {
		instanceHashes = append(instanceHashes, r.Hash)
	}

	blob := downstreamBlob(t, f, jobRun.ID)
	require.Equal(t, []string{run.GroupIdentityHash(instanceHashes)}, blob.PredecessorHashes)

	aggregate := blob.PredecessorOutputs["process"]
	require.Equal(t, "3", aggregate["PARTITION_COUNT"])
	var rowsByPartition map[string]string
	require.NoError(t, json.Unmarshal([]byte(aggregate["rows"]), &rowsByPartition))
	require.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, rowsByPartition)
}
