package incident

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedFailedTask inserts a JobRun, Task, and failed TaskRun the subscriber can
// resolve, returning the ids needed to publish an event.
func seedFailedTask(t *testing.T, db *gorm.DB, logText string) (jobID, runID, taskID uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	jobID = uuid.New()
	runID = uuid.New()
	taskID = uuid.New()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "extract", CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "sh",
		Status: "failed", Result: "failure", LogText: logText,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	return jobID, runID, taskID
}

func startSubscriber(t *testing.T, bus event.Bus, db *gorm.DB, cooldown time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := NewSubscriber(bus, db, nil, cooldown)
	ready := make(chan struct{})
	go func() { _ = sub.StartWithReady(ctx, ready) }()
	<-ready
	return ctx
}

func waitForIncidents(t *testing.T, db *gorm.DB, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		require.NoError(t, db.Model(&models.Incident{}).Count(&n).Error)
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var n int64
	db.Model(&models.Incident{}).Count(&n)
	t.Fatalf("expected %d incidents, got %d", want, n)
}

func TestSubscriberOpensClassifiedIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")

	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, string(ClassAuthFailure), inc.Class)
	require.Equal(t, models.IncidentStatusOpen, inc.Status)
	require.Equal(t, 1, inc.OccurrenceCount)
}

func TestSubscriberAppendsOccurrenceNoTwin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "quota exceeded: too many requests")
	evt := event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()}

	bus.Publish(evt)
	waitForIncidents(t, db, 1)
	bus.Publish(evt)

	// Give the subscriber a moment; count must stay at 1 with occurrence 2.
	require.Eventually(t, func() bool {
		var inc models.Incident
		if err := db.First(&inc).Error; err != nil {
			return false
		}
		return inc.OccurrenceCount == 2
	}, 3*time.Second, 10*time.Millisecond)
	testutil.AssertCount(t, db, &models.Incident{}, 1)
}

func TestSubscriberRemediatesOnSuccess(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "could not connect: connection refused")
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	// A later success for the same job/task closes the incident as remediated.
	bus.Publish(event.Event{Type: event.TypeTaskSucceeded, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	require.Eventually(t, func() bool {
		var inc models.Incident
		if err := db.First(&inc).Error; err != nil {
			return false
		}
		return inc.Status == models.IncidentStatusClosed && inc.ClosedAt != nil
	}, 3*time.Second, 10*time.Millisecond)
}
