package incident

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fanout_remediation_test.go covers the second half of the fan-out incident
// defect: attribution reads the FAILED instance (fanout_attribution_test.go),
// but remediation still closed the incident on any task_succeeded for the same
// catalog task.

// TestSubscriber_PartiallyFailedGroupDoesNotRemediate pins the remediation
// guard. Under fanOut.failurePolicy: continue a group can be terminal with some
// partitions failed and some succeeded. A success event naming that catalog
// task must NOT be read as "this task later ran green" — the group as a whole
// failed, the incident describes a live failure, and auto-closing it hides a
// broken partition from every alerting path that watches open incidents.
func TestSubscriber_PartiallyFailedGroupDoesNotRemediate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID, _ := seedFanOutFailure(t, db)
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.NotNil(t, inc.ActiveDedupeKey, "the incident starts open")

	// The last succeeding sibling of the SAME group lands. Two partitions of the
	// group are still failed.
	bus.Publish(event.Event{Type: event.TypeTaskSucceeded, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})

	requireIncidentStaysOpen(t, db, inc.ID)
}

// TestSubscriber_FullyGreenGroupRemediates is the control: once every partition
// of the group is a terminal success, the success event must still close the
// incident, exactly as it does for an unfanned task.
func TestSubscriber_FullyGreenGroupRemediates(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID, _ := seedFanOutFailure(t, db)
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)

	// A later attempt turned every partition green.
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]any{"status": "succeeded", "result": "success", "error": ""}).Error)

	bus.Publish(event.Event{Type: event.TypeTaskSucceeded, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})

	requireIncidentRemediated(t, db, inc.ID)
}

// TestSubscriber_UnfannedSuccessStillRemediates keeps the unfanned path
// byte-identical: one row, succeeded, closes its incident.
func TestSubscriber_UnfannedSuccessStillRemediates(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)

	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]any{"status": "succeeded", "result": "success", "error": ""}).Error)

	bus.Publish(event.Event{Type: event.TypeTaskSucceeded, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})

	requireIncidentRemediated(t, db, inc.ID)
}

func requireIncidentRemediated(t *testing.T, db *gorm.DB, id uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var inc models.Incident
		require.NoError(t, db.First(&inc, "id = ?", id).Error)
		if inc.Status == models.IncidentStatusClosed && inc.ClosedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected the incident to be remediated by the group's success")
}

func requireIncidentStaysOpen(t *testing.T, db *gorm.DB, id uuid.UUID) {
	t.Helper()
	// Give the subscriber a real window to (wrongly) remediate before asserting.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var inc models.Incident
		require.NoError(t, db.First(&inc, "id = ?", id).Error)
		require.NotEqual(t, models.IncidentStatusClosed, inc.Status,
			"a group with failed partitions has not run green; the incident must stay open")
		require.NotNil(t, inc.ActiveDedupeKey,
			"the incident must remain the job/task's ACTIVE one")
		time.Sleep(10 * time.Millisecond)
	}
}
