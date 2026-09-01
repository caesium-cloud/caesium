package run

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// addTerminalUpstream wires a second cross-step predecessor (`warm`) into the
// fixture's consumer and records its single TaskRun in the given terminal
// status, so the consumer's predecessor set is terminal but not all-success.
func addTerminalUpstream(t *testing.T, f *fanOutFixture, status TaskStatus) *models.Task {
	t.Helper()
	var atomRow models.Atom
	require.NoError(t, f.db.First(&atomRow, "id = ?", f.producer.AtomID).Error)
	warm := &models.Task{ID: uuid.New(), JobID: f.jobID, AtomID: atomRow.ID, Name: "warm", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	require.NoError(t, f.db.Create(warm).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: f.jobID, FromTaskID: warm.ID, ToTaskID: f.consumer.ID,
	}).Error)
	require.NoError(t, f.store.RegisterTasks(f.runID, []RegisterTaskInput{
		{Task: warm, Atom: &atomRow, OutstandingPredecessors: 0},
	}))
	row, err := loadUniqueTaskRun(f.db, f.runID, warm.ID)
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, row.ID, status, nil)
	return warm
}

// finishFannedRun records the producer as succeeded, marks every consumer
// instance as having been dispatched (outstanding_predecessors = 0), applies
// the given per-index instance outcomes, and finalizes the run as failed.
func finishFannedRun(t *testing.T, f *fanOutFixture, outcomes ...TaskStatus) []models.TaskRun {
	t.Helper()
	setInstanceOutcome(t, f.db, f.producerRow(t).ID, TaskStatusSucceeded, nil)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", 0).Error)
	rows := f.instances(t)
	require.Len(t, rows, len(outcomes))
	for i, status := range outcomes {
		setInstanceOutcome(t, f.db, rows[i].ID, status, nil)
	}
	require.NoError(t, f.store.Complete(f.runID, errors.New("fan-out failed")))
	return rows
}

// TestRetryPartitionReseedsCrossStepPredecessorsByTriggerRule pins the reachable
// half of the P1: a consumer with a failure-tolerant trigger rule (all_done)
// legitimately ran after an upstream failure. Retrying one of its failed
// instances must re-seed outstanding_predecessors by the consumer's own rule,
// not by "every predecessor group is all-success" — otherwise the reset row is
// blocked forever, the completion fence ignores it, and the accepted retry is
// stranded on a terminal run.
func TestRetryPartitionReseedsCrossStepPredecessorsByTriggerRule(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.consumer.ID).
		Update("trigger_rule", jobdefschema.TriggerRuleAllDone).Error)
	addTerminalUpstream(t, f, TaskStatusFailed)
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusSucceeded)

	reset, reopened, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	require.True(t, reopened, "a terminal run must be reopened for the retry")
	require.Equal(t, 0, reset.OutstandingPredecessors,
		"all_done is satisfied by a terminal-failed predecessor group; the reset instance must be immediately dispatchable")

	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", rows[0].ID).Error)
	require.Equal(t, 0, persisted.OutstandingPredecessors)
	require.True(t, persisted.PartitionRetryPending)
}

// TestRetryPartitionRefusesInstanceBlockedByTriggerRule is the fail-closed
// half: when every predecessor group is terminal and the consumer's rule is
// NOT satisfied, nothing in this run will ever release the reset row. The
// store must refuse before any mutation rather than accept a retry it cannot
// execute.
func TestRetryPartitionRefusesInstanceBlockedByTriggerRule(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	addTerminalUpstream(t, f, TaskStatusFailed)
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusSucceeded)

	_, reopened, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.ErrorIs(t, err, ErrPartitionRetryBlocked)
	require.False(t, reopened)

	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", rows[0].ID).Error)
	require.Equal(t, string(TaskStatusFailed), persisted.Status, "a refused retry must not reset the instance")
	require.False(t, persisted.PartitionRetryPending, "a refused retry must not leave retry provenance behind")

	got, err := f.store.Get(f.runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status, "a refused retry must not reopen the run")
}

// TestRetryPartitionRefusesInstanceBlockedByFailedInGroupDependency covers the
// in-group form: an ordered partition whose dependsOn sibling is terminal but
// not a success can never become ready inside this run.
func TestRetryPartitionRefusesInstanceBlockedByFailedInGroupDependency(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, []pkgtask.Partition{{Key: "a"}, {Key: "b", DependsOn: []string{"a"}}})
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusFailed)

	_, reopened, err := f.store.RetryPartition(context.Background(), f.runID, rows[1].ID)
	require.ErrorIs(t, err, ErrPartitionRetryBlocked)
	require.False(t, reopened)

	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", rows[1].ID).Error)
	require.Equal(t, string(TaskStatusFailed), persisted.Status)
	require.False(t, persisted.PartitionRetryPending)

	got, err := f.store.Get(f.runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status)
}

// TestRetryPartitionInGroupDependencyOnRetriedSiblingStaysPending is the
// control for the in-group refusal: a dependency that is itself pending again
// (retried first) is not terminal, so the dependent's retry is accepted and
// simply waits on it.
func TestRetryPartitionInGroupDependencyOnRetriedSiblingStaysPending(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, []pkgtask.Partition{{Key: "a"}, {Key: "b", DependsOn: []string{"a"}}})
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusFailed)

	_, _, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	reset, _, err := f.store.RetryPartition(context.Background(), f.runID, rows[1].ID)
	require.NoError(t, err)
	require.Equal(t, 1, reset.OutstandingPredecessors, "b must wait on the retried a")
}

// TestCompleteRefusesPartitionRetryBlockedOnLiveDependency broadens the fence:
// a retry-reset instance that is still waiting on a non-terminal dependency is
// exactly as stranded by a terminal JobRun as a ready one. The fence must key
// on retry provenance alone, not on outstanding_predecessors = 0.
func TestCompleteRefusesPartitionRetryBlockedOnLiveDependency(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, f.producerRow(t).ID, TaskStatusSucceeded, nil)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", 0).Error)
	rows := f.instances(t)
	setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusFailed, nil)
	setInstanceOutcome(t, f.db, rows[1].ID, TaskStatusSucceeded, nil)

	_, reopened, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	require.False(t, reopened)
	// A dependency the live engine has not resolved yet.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("id = ?", rows[0].ID).
		Update("outstanding_predecessors", 1).Error)

	err = f.store.Complete(f.runID, nil)
	require.ErrorIs(t, err, ErrRunHasPendingWork)

	got, err := f.store.Get(f.runID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)
}

// TestRetryPartitionCountsOnlyLivePredecessorsWhenMixed pins the non-terminal
// branch of the re-seed. outstanding_predecessors is a terminality counter —
// it is decremented exactly once per predecessor group, when that group
// becomes terminal, and the trigger rule is evaluated at that moment. A group
// that is already terminal (even without succeeding) will never decrement
// again, so counting it strands the row; only still-live groups may be
// counted.
func TestRetryPartitionCountsOnlyLivePredecessorsWhenMixed(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.consumer.ID).
		Update("trigger_rule", jobdefschema.TriggerRuleAllDone).Error)
	addTerminalUpstream(t, f, TaskStatusFailed)
	live := addTerminalUpstream(t, f, TaskStatusRunning)
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, f.producerRow(t).ID, TaskStatusSucceeded, nil)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", 0).Error)
	rows := f.instances(t)
	setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusFailed, nil)
	setInstanceOutcome(t, f.db, rows[1].ID, TaskStatusSucceeded, nil)

	reset, _, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, 1, reset.OutstandingPredecessors,
		"only the live predecessor %s may be counted; the terminal-failed one will never decrement again", live.Name)
}
