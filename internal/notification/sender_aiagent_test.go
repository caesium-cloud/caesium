package notification

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAIAgentSenderOpensIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sender := NewAIAgentSender(db, nil, 0) // nil leader check = always act
	jobID := uuid.New()

	err := sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		Error:     "authentication failed: 401 unauthorized",
	})
	require.NoError(t, err)

	var incidents []models.Incident
	require.NoError(t, db.Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, jobID, incidents[0].JobID)
	// The classifier maps the 401 auth log pattern to auth_failure.
	require.Equal(t, "auth_failure", incidents[0].Class)

	// A second matched event for the same key folds in as an occurrence (the
	// atomic conditional insert prevents a twin), not a new incident.
	require.NoError(t, sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		Error:     "authentication failed: 401 unauthorized",
	}))
	require.NoError(t, db.Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, 2, incidents[0].OccurrenceCount)
}

func TestAIAgentSenderLeaderGated(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	// A non-leader node must NOT open incidents — the leader gate avoids N-node
	// duplicate work (the store's atomic insert is the correctness backstop).
	sender := NewAIAgentSender(db, func(context.Context) (bool, error) { return false, nil }, 0)

	err := sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     uuid.New(),
		Error:     "authentication failed: 401",
	})
	require.NoError(t, err)

	var count int64
	require.NoError(t, db.Model(&models.Incident{}).Count(&count).Error)
	require.Equal(t, int64(0), count)
}

// TestAIAgentSenderSkipsSuccessEvents proves the sender never manufactures an
// incident for a healthy run: a policy could fan a success event to an ai_agent
// channel, and those must be ignored (only failure-class events open incidents).
func TestAIAgentSenderSkipsSuccessEvents(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sender := NewAIAgentSender(db, nil, 0)

	for _, et := range []event.Type{event.TypeRunCompleted, event.TypeTaskSucceeded} {
		require.NoError(t, sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
			EventType: et,
			JobID:     uuid.New(),
		}))
	}

	var count int64
	require.NoError(t, db.Model(&models.Incident{}).Count(&count).Error)
	require.Equal(t, int64(0), count, "success events must not open incidents")
}

// TestAIAgentSenderReadsFannedGroupAndPrefersFailedInstance is the F9 coverage
// for this sender's re-keyed read: a fanned task has N TaskRun rows sharing
// (job_run_id, task_id), and the classifier signal must be built from the
// LOWEST-partition_index FAILED instance — proving both that it prefers a
// failure over a succeeded sibling with no error to classify, and that among
// two failures it stops at the lower partition_index rather than whichever
// one it happens to see.
//
// TWO instances fail here, at DIFFERENT partition_index values (1 and 3) and
// with DIFFERENT errors mapping to DIFFERENT failure classes, and the
// higher-index failure is inserted FIRST: with only one failure (as this test
// used to seed), FailedOrLastTaskRunForTask finds it wherever it sits and a
// dropped ORDER BY is invisible; with two, only stopping at the lower index
// produces "auth_failure" rather than "data_unavailable".
//
// Caveat, confirmed by probing this exact fixture: on the sqlite backend
// testutil.OpenTestDB uses, models.TaskRun's own unique index — partition_index
// tagged `uniqueIndex:idx_taskrun_jobrun_task` alongside job_run_id/task_id —
// makes SQLite's query planner satisfy this (job_run_id, task_id) equality
// WHERE via an index scan that already returns partition_index order,
// regardless of whether the Go code adds Order("partition_index ASC"). So
// dropping that clause alone will not fail this test on sqlite; the ORDER BY
// remains correct and necessary (SQL row order is unspecified without one),
// but this fixture's real, falsifiable contribution is pinning
// FailedOrLastTaskRunForTask's own "first failed row wins" contract against a
// genuinely ambiguous case (two failures, not one).
func TestAIAgentSenderReadsFannedGroupAndPrefersFailedInstance(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	now := time.Now().UTC()

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "fanned-notify-job", CreatedAt: now, UpdatedAt: now}).Error)

	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, CreatedAt: now, UpdatedAt: now}).Error)

	taskID := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Name: "process", CreatedAt: now, UpdatedAt: now}).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)

	// Four fan-out instances: two fail, at partition_index 1 and 3, with
	// errors that classify DIFFERENTLY (auth_failure vs. data_unavailable) so
	// the assertion can tell which one won. The higher-index failure (d,
	// index 3) is inserted FIRST.
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 0, PartitionValue: "a", Status: "succeeded", Result: "success", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 1, PartitionValue: "b", Status: "failed", Result: "failure", Error: "authentication failed: 401 unauthorized", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 2, PartitionValue: "c", Status: "succeeded", Result: "success", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 3, PartitionValue: "d", Status: "failed", Result: "failure", Error: "connection refused", CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Create(&rows[3]).Error)
	require.NoError(t, db.Create(&rows[2]).Error)
	require.NoError(t, db.Create(&rows[0]).Error)
	require.NoError(t, db.Create(&rows[1]).Error)

	sender := NewAIAgentSender(db, nil, 0)
	err := sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		RunID:     runID,
		TaskID:    taskID,
	})
	require.NoError(t, err)

	var incidents []models.Incident
	require.NoError(t, db.Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, "auth_failure", incidents[0].Class,
		"the incident must classify from the LOWER-partition_index failed instance (b, auth_failure), "+
			"not the higher one (d, data_unavailable) or a succeeded sibling's absent error")
	require.Equal(t, "process", incidents[0].TaskName)
}
