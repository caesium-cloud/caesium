package incident

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
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

// TestSubscriberDedupesRunLevelTwin drives the dual-event path a real failing
// run produces: it publishes BOTH task_failed (task-attributed) and run_failed
// (no TaskID) for one run. Before the fix these routed to two distinct dedupe
// keys (job|task|class and job||class) and opened two incidents. Exactly one
// incident — the task-attributed one — must remain.
func TestSubscriberDedupesRunLevelTwin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")

	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	bus.Publish(event.Event{Type: event.TypeRunFailed, JobID: jobID, RunID: runID, Timestamp: time.Now()})

	// Sentinel: an independent failure published after the run_failed above. A
	// single subscriber goroutine drains the bus FIFO, so once the sentinel's
	// incident exists the run_failed has already been handled — making the
	// "exactly one incident for the run" assertion deterministic rather than a
	// race against an unprocessed event.
	jobID2, runID2, taskID2 := seedFailedTask(t, db, "Error: permission denied reading /other")
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID2, RunID: runID2, TaskID: taskID2, Timestamp: time.Now()})

	require.Eventually(t, func() bool {
		var n int64
		if err := db.Model(&models.Incident{}).Where("job_id = ?", jobID2).Count(&n).Error; err != nil {
			return false
		}
		return n == 1
	}, 3*time.Second, 10*time.Millisecond)

	// Exactly one incident for the first run, and it is the task-attributed one.
	var incidents []models.Incident
	require.NoError(t, db.Where("job_id = ?", jobID).Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, "extract", incidents[0].TaskName)
	require.NotNil(t, incidents[0].TaskID)
	require.Equal(t, taskID, *incidents[0].TaskID)
}

// TestSubscriberOpensRunLevelIncidentWithNoFailedTask guards the preserve case:
// a run_failed with NO failed task_run (an infra/setup failure that never
// produced a failing task) is genuinely run-level and must still open one
// incident.
func TestSubscriberOpensRunLevelIncidentWithNoFailedTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	now := time.Now().UTC()
	jobID := uuid.New()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	bus.Publish(event.Event{Type: event.TypeRunFailed, JobID: jobID, RunID: runID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, models.IncidentStatusOpen, inc.Status)
	require.Equal(t, "", inc.TaskName)
	require.Nil(t, inc.TaskID)
}

// TestSubscriberOpensRunLevelIncidentWhenTaskFailedDropped guards against a
// silently missed incident: a failed task_run is committed, but its task_failed
// event never reaches the subscriber (bus overflow / crash between publish and
// handle), so NO task-attributed incident was opened. The run_failed must still
// open exactly one incident. This is precisely why suppression keys on the
// incident, not on the task_runs table — a committed failed task_run with no
// incident behind it must not swallow the run failure.
func TestSubscriberOpensRunLevelIncidentWhenTaskFailedDropped(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	// The failed task_run exists, but we publish ONLY run_failed (the task_failed
	// event was "dropped").
	jobID, runID, _ := seedFailedTask(t, db, "Error: permission denied reading /secure")

	bus.Publish(event.Event{Type: event.TypeRunFailed, JobID: jobID, RunID: runID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, models.IncidentStatusOpen, inc.Status)
	require.NotNil(t, inc.RunID)
	require.Equal(t, runID, *inc.RunID)
	// The run-level incident (no task attribution) was correctly opened.
	require.Equal(t, "", inc.TaskName)
	require.Nil(t, inc.TaskID)
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

func TestSubscriberPropagatesRuntimeOOMEvidence(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)
	jobID, runID, taskID := seedFailedTask(t, db, "no diagnostic log")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).Updates(map[string]any{"result": "resource_failure", "oom_killed": true, "exit_code": nil}).Error)
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)
	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, string(ClassOOM), inc.Class)
}

// TestSubscriberPublishesIncidentOpened is #419's contract: opening an
// incident must announce TypeIncidentOpened on the shared event stream,
// persisted through the same event.Store path as approval_requested /
// agent_action_executed (SetEventSink, PR #390's seam) so it survives a
// restart and is queryable from /v1/events — not just fanned out in memory.
func TestSubscriberPublishesIncidentOpened(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub := NewSubscriber(bus, db, nil, 0)
	sub.SetEventSink(event.NewStore(db))

	opened, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeIncidentOpened}})
	require.NoError(t, err)

	ready := make(chan struct{})
	go func() { _ = sub.StartWithReady(ctx, ready) }()
	<-ready

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})

	var evt event.Event
	select {
	case evt = <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for incident_opened")
	}

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)

	require.Equal(t, jobID, evt.JobID)
	require.Equal(t, runID, evt.RunID)
	require.Equal(t, taskID, evt.TaskID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(evt.Payload, &payload))
	require.Equal(t, inc.ID.String(), payload["incident_id"])
	require.Equal(t, jobID.String(), payload["job_id"])
	require.Equal(t, runID.String(), payload["run_id"])
	require.Equal(t, taskID.String(), payload["task_id"])
	require.Equal(t, "extract", payload["task_name"])
	require.Equal(t, string(ClassAuthFailure), payload["class"])
	require.Equal(t, string(models.IncidentStatusOpen), payload["status"])
	require.Equal(t, inc.DedupeKey, payload["dedupe_key"])

	// The event is PERSISTED, not merely fanned out in memory — the /v1/events
	// backlog replay (api/rest/controller/event/stream.go) only ever sees rows
	// in the event store.
	var persisted int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeIncidentOpened)).Count(&persisted).Error)
	require.EqualValues(t, 1, persisted)
}

// TestSubscriberDoesNotRepublishIncidentOpenedOnAppend pins that a second
// failure folding into the SAME incident (an appended occurrence, not a fresh
// open) does not re-announce incident_opened — that would misrepresent one
// incident as opening twice on every consumer of the event stream.
func TestSubscriberDoesNotRepublishIncidentOpenedOnAppend(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub := NewSubscriber(bus, db, nil, 0)
	sub.SetEventSink(event.NewStore(db))

	opened, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeIncidentOpened}})
	require.NoError(t, err)

	ready := make(chan struct{})
	go func() { _ = sub.StartWithReady(ctx, ready) }()
	<-ready

	jobID, runID, taskID := seedFailedTask(t, db, "quota exceeded: too many requests")
	evt := event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()}
	bus.Publish(evt)

	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first incident_opened")
	}

	bus.Publish(evt)
	require.Eventually(t, func() bool {
		var inc models.Incident
		if err := db.First(&inc).Error; err != nil {
			return false
		}
		return inc.OccurrenceCount == 2
	}, 3*time.Second, 10*time.Millisecond, "the second failure must fold into the same incident")

	select {
	case second := <-opened:
		t.Fatalf("incident_opened must not republish on an appended occurrence: %+v", second)
	case <-time.After(500 * time.Millisecond):
		// Expected: nothing else arrives.
	}

	var persisted int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeIncidentOpened)).Count(&persisted).Error)
	require.EqualValues(t, 1, persisted)
}

// failNextInsertInto arms a one-shot failure on the next INSERT this DB issues
// against table. It is a real transaction abort — gorm sees the error before
// the statement runs, so an enclosing transaction rolls back exactly as a
// genuine durable-write failure would (mirrors
// internal/run/owner_commit_atomicity_test.go's failNextUpdate, scoped to
// Create instead of Update).
func failNextInsertInto(t *testing.T, db *gorm.DB, table string) *atomic.Bool {
	t.Helper()
	var armed atomic.Bool
	callbackName := "test:fail_next_insert_" + table
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != table {
			return
		}
		if armed.CompareAndSwap(true, false) {
			_ = tx.AddError(errors.New("injected durable write failure"))
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName)
	})
	return &armed
}

// TestSubscriberIncidentOpenedSurvivesEventInsertFailureThenRedelivery is the
// #459 review's exact reproduction, driven through the real Subscriber wiring
// with a REAL event.Store over the execution_events table (not just Store's
// generic OnOpen hook, which store_test.go covers directly):
//
//	one-shot execution_events insert failure, restore writes, redeliver the
//	failure → before the fix: one incident with occurrence_count=2 and ZERO
//	incident_opened rows, because the incident commit and the event insert
//	were two separate transactions.
//
// After the fix, the failing first delivery rolls back the incident row
// along with the failed event insert (OnOpen runs inside OpenOrAppend's
// transaction), so nothing is left to "append" to; the redelivered failure
// opens a genuinely fresh incident with its incident_opened event persisted
// and published on the live bus.
func TestSubscriberIncidentOpenedSurvivesEventInsertFailureThenRedelivery(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub := NewSubscriber(bus, db, nil, 0)
	sub.SetEventSink(event.NewStore(db))

	opened, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeIncidentOpened}})
	require.NoError(t, err)

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")
	evt := event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()}

	armed := failNextInsertInto(t, db, "execution_events")
	armed.Store(true)

	// First delivery: the companion event insert fails mid-transaction.
	// handleFailure is called directly (not through the async bus) so the
	// negative assertions below are deterministic, not eventually-true.
	sub.handleFailure(ctx, evt)
	require.False(t, armed.Load(), "the injected failure must actually have fired")

	testutil.AssertCount(t, db, &models.Incident{}, 0)
	var incidentOpenedRows int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeIncidentOpened)).Count(&incidentOpenedRows).Error)
	require.EqualValues(t, 0, incidentOpenedRows,
		"a rolled-back open must leave no incident_opened row behind")

	// Redelivery: the same underlying failure is processed again. The
	// injected failure was one-shot and already fired, so the write path is
	// effectively "restored".
	sub.handleFailure(ctx, evt)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, 1, inc.OccurrenceCount,
		"redelivery after a fully rolled-back attempt must open fresh, not append to a ghost incident")

	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeIncidentOpened)).Count(&incidentOpenedRows).Error)
	require.EqualValues(t, 1, incidentOpenedRows,
		"the companion event must be persisted once redelivery succeeds — the #459 gap")

	select {
	case got := <-opened:
		var payload map[string]any
		require.NoError(t, json.Unmarshal(got.Payload, &payload))
		require.Equal(t, inc.ID.String(), payload["incident_id"],
			"the redelivered open must still publish incident_opened on the live bus")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for incident_opened on the redelivered attempt")
	}
}
