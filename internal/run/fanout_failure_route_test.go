package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A container that exits non-zero is NOT reported through FailTaskClaimed. The
// distributed worker treats "the container ran and told us its result" as a
// completion and calls sink.Succeeded with result "failure"
// (internal/worker/runtime_executor.go), which lands on
// CompleteTaskClaimed → completeTask's failure branch. FailTaskClaimed is only
// reached afterwards, when attempts are exhausted, and by then the row is
// already terminal so failTask returns early.
//
// That made the SQL lane's failure policy route-dependent: fail_fast was
// implemented in failTask only, so under CAESIUM_EXECUTION_MODE=distributed a
// failed partition silently degraded to `continue` and every pending sibling
// ran to completion. These tests drive the COMPLETION route — the one the
// worker actually takes — and assert it resolves the group identically.

// failFastFixture expands the five-partition shape both integration scenarios
// use (TestFanOutFailFastCancelsPendingSiblings / …IsTheDefault): `bad` fails,
// `gate` is mid-flight, and x/y/z sit behind `gate` so they are provably
// PENDING at the moment `bad` fails, in any claim order.
func failFastFixture(t *testing.T, fo *jobdefschema.FanOut) (*fanOutFixture, map[string]models.TaskRun) {
	t.Helper()
	f := newFanOutFixture(t, fo)
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "gate"},
		{Key: "x", DependsOn: []string{"gate"}},
		{Key: "y", DependsOn: []string{"gate"}},
		{Key: "z", DependsOn: []string{"gate"}},
	})
	require.NoError(t, err)

	byKey := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		byKey[r.PartitionValue] = r
	}
	require.Len(t, byKey, 5)

	// Both instances that are ready were claimed by the same worker and started;
	// `gate` is still mid-sleep when `bad` reports its non-zero exit.
	for _, key := range []string{"bad", "gate"} {
		require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey[key].ID).Updates(map[string]any{
			"status":     string(TaskStatusRunning),
			"claimed_by": "worker-1",
			"runtime_id": "container-" + key,
		}).Error)
	}
	return f, byKey
}

func instancesByPartition(t *testing.T, f *fanOutFixture) map[string]models.TaskRun {
	t.Helper()
	out := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		out[r.PartitionValue] = r
	}
	return out
}

// TestCompleteTaskClaimedFailureResultFailsFast is the distributed lane's half
// of TestFailTaskFailFastSkipsEveryPendingSibling: the same contract, asserted
// on the route the worker takes.
func TestCompleteTaskClaimedFailureResultFailsFast(t *testing.T) {
	f, byKey := failFastFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureFailFast,
	})

	require.NoError(t, f.store.CompleteTaskClaimed(
		f.runID, byKey["bad"].ID, "failure", "worker-1", nil, nil))

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status,
		"a non-zero container exit arrives as result=failure and must land the row failed")

	seqs := map[int64]string{}
	for _, key := range []string{"x", "y", "z"} {
		r := after[key]
		assert.Equal(t, string(TaskStatusSkipped), r.Status,
			"fail_fast must resolve pending sibling %q on the completion route too", key)
		assert.Equal(t, "fan-out group failed fast", r.Error,
			"the reason string is the contract both lanes emit; partition %s", key)
		require.NotZero(t, r.TerminalSequence,
			"a zero terminal_sequence is invisible to TerminalTaskRunsSince replay; partition %s", key)
		prev, dup := seqs[r.TerminalSequence]
		require.False(t, dup, "partitions %s and %s share terminal_sequence %d", prev, key, r.TerminalSequence)
		seqs[r.TerminalSequence] = key
	}

	// The in-flight sibling keeps its live container: fail_fast resolves PENDING
	// siblings only, on every route.
	assert.Equal(t, string(TaskStatusRunning), after["gate"].Status,
		"fail_fast must not resolve a RUNNING sibling out from under its container")
	assert.Equal(t, "container-gate", after["gate"].RuntimeID)

	// Each resolved instance carries its own task_skipped event.
	assert.ElementsMatch(t, []string{"x", "y", "z"}, taskSkippedPartitions(t, f))

	// The group is not all-terminal yet — `gate` finishing is what completes it —
	// so the fan-in has not been advanced.
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		allTerm, err := f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		require.NoError(t, err)
		assert.False(t, allTerm)
		return nil
	}))
}

// TestCompleteTaskClaimedFailureResultFailFastIsTheDefault pins the schema
// default on the completion route: an absent failurePolicy IS fail_fast. This
// is the shape TestFanOutFailFastIsTheDefault drives end to end.
func TestCompleteTaskClaimedFailureResultFailFastIsTheDefault(t *testing.T) {
	f, byKey := failFastFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	require.NoError(t, f.store.CompleteTaskClaimed(
		f.runID, byKey["bad"].ID, "failure", "worker-1", nil, nil))

	after := instancesByPartition(t, f)
	for _, key := range []string{"x", "y", "z"} {
		assert.Equal(t, string(TaskStatusSkipped), after[key].Status,
			"an unset failurePolicy must behave as fail_fast on the completion route; partition %s", key)
		assert.Equal(t, "fan-out group failed fast", after[key].Error)
	}
}

// TestCompleteTaskClaimedFailureResultContinueSkipsOnlyDependents is the
// contrast: `continue` still resolves only the failure's transitive in-group
// dependents, so the completion route did not simply gain fail_fast
// unconditionally.
func TestCompleteTaskClaimedFailureResultContinueSkipsOnlyDependents(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "dep", DependsOn: []string{"bad"}},
		{Key: "free"},
	})
	require.NoError(t, err)
	byKey := instancesByPartition(t, f)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey["bad"].ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"claimed_by": "worker-1",
	}).Error)

	require.NoError(t, f.store.CompleteTaskClaimed(
		f.runID, byKey["bad"].ID, "failure", "worker-1", nil, nil))

	after := instancesByPartition(t, f)
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status)
	assert.Equal(t, string(TaskStatusSkipped), after["dep"].Status)
	assert.Equal(t, "fan-out dependency bad failed", after["dep"].Error,
		"the dependency cascade keeps its own reason under `continue`")
	assert.Equal(t, string(TaskStatusPending), after["free"].Status,
		"`continue` must not resolve an independent sibling on the completion route either")
}

// TestCompleteTaskFailureResultLeavesUnfannedTasksAlone pins that routing the
// failure branch through the shared resolution did not give an ORDINARY task
// fan-out consequences: an unfanned row has no group, so nothing else in the
// run may be touched by its failure.
func TestCompleteTaskFailureResultLeavesUnfannedTasksAlone(t *testing.T) {
	f := newFanOutFixture(t, nil)

	require.NoError(t, f.store.CompleteTask(f.runID, f.producer.ID, "failure", nil, nil))

	producer := f.producerRow(t)
	assert.Equal(t, string(TaskStatusFailed), producer.Status)

	consumer, err := loadUniqueTaskRun(f.db, f.runID, f.consumer.ID)
	require.NoError(t, err)
	assert.Equal(t, string(TaskStatusPending), consumer.Status,
		"an unfanned failure must not resolve its successors through the fan-out path")
}
