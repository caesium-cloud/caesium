package incident

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedTransientInfraFailure inserts a JobRun/Task/TaskRun whose engine result is
// a startup failure, which the classifier buckets as transient_infra.
func seedTransientInfraFailure(t *testing.T, db *gorm.DB) (jobID, runID, taskID uuid.UUID) {
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
		Status: "failed", Result: string(atom.StartupFailure),
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	return jobID, runID, taskID
}

func waitForAction(t *testing.T, db *gorm.DB) models.AgentAction {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var a models.AgentAction
		if err := db.First(&a).Error; err == nil {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected a deterministic AgentAction row, got none")
	return models.AgentAction{}
}

// A transient_infra incident open must run the deterministic auto_retry_backoff
// rule as an actor=policy retry_from_failure — the live Phase-0 autonomous path
// (design "Playbook match"). Without SetRemediator no action is taken.
func TestSubscriberDeterministicRuleOnOpen(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := NewSubscriber(bus, db, nil, 0)
	ops := &fakeOps{}
	sub.SetRemediator(NewExecutor(NewStore(db), ops), DefaultRuleSet())
	ready := make(chan struct{})
	go func() { _ = sub.StartWithReady(ctx, ready) }()
	<-ready

	jobID, runID, taskID := seedTransientInfraFailure(t, db)
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})

	action := waitForAction(t, db)
	require.Equal(t, models.AgentActionActorPolicy, action.Actor)
	require.Equal(t, ActionTypeRetryFromFailure, action.Type)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)

	// The recorded result names the incident's own run — proving the policy rule
	// retried the failing run.
	var result struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(action.Result, &result))
	require.Equal(t, runID.String(), result.RunID)
}

// Without a remediator wired, the subscriber classifies and opens the incident
// but takes no autonomous action (default-off deployments).
func TestSubscriberNoRemediatorTakesNoAction(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := NewSubscriber(bus, db, nil, 0)
	ready := make(chan struct{})
	go func() { _ = sub.StartWithReady(ctx, ready) }()
	<-ready

	jobID, runID, taskID := seedTransientInfraFailure(t, db)
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	// Give any (erroneous) action a chance to land, then assert none did.
	time.Sleep(50 * time.Millisecond)
	var actions int64
	require.NoError(t, db.Model(&models.AgentAction{}).Count(&actions).Error)
	require.Zero(t, actions)
}
