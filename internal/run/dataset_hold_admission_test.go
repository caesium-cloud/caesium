package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// seedConsumerJob builds a two-step job that DECLARES it consumes `dataset`.
// The second step is fanned, so the skipped-run shape can be asserted against a
// fan-out group as well as a plain step.
func seedConsumerJob(t *testing.T, db *gorm.DB, alias, dataset, onUpstreamHold string) uuid.UUID {
	t.Helper()

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron}).Error)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID: jobID, Alias: alias, TriggerID: triggerID, OnUpstreamHold: onUpstreamHold,
	}).Error)

	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{
		ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
	}).Error)

	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "read", CreatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "fanned",
		FanOutConfig: datatypes.JSON([]byte(`{"from":"read.items"}`)),
		CreatedAt:    now.Add(time.Millisecond),
	}).Error)

	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, JobAlias: alias, StepName: "read",
		Name: dataset, Direction: models.DatasetDirectionConsumes,
	}).Error)

	return jobID
}

// seedActiveHold opens a hold directly, standing in for a producer run that
// already breached its contract.
func seedActiveHold(t *testing.T, db *gorm.DB, dataset string) *models.DatasetHold {
	t.Helper()
	now := time.Now().UTC()
	key := models.DatasetHoldKey("", dataset)
	hold := &models.DatasetHold{
		ID:              uuid.New(),
		Name:            dataset,
		Status:          models.DatasetHoldStatusActive,
		ActiveKey:       &key,
		Reason:          AssertionMin,
		HeldByJobID:     uuid.New(),
		HeldByJobAlias:  "producer",
		OccurrenceCount: 1,
		OpenedAt:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, db.Create(hold).Error)
	return hold
}

// TestAdmitSkipsRunWhenAConsumedDatasetIsHeld is the C2 headline: a run
// triggered while an upstream dataset is held becomes a ROW — terminal
// `skipped`, with a reason and a full set of skipped task rows — not an
// absence.
func TestAdmitSkipsRunWhenAConsumedDatasetIsHeld(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	bus := event.New()
	store.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := bus.Subscribe(ctx, event.Filter{
		Types: []event.Type{event.TypeRunHeldUpstream, event.TypeTaskSkipped, event.TypeRunStarted},
	})
	require.NoError(t, err)

	dataset := "warehouse/orders"
	jobID := seedConsumerJob(t, db, "consumer", dataset, "")
	hold := seedActiveHold(t, db, dataset)

	beforeHeld := metricstestutil.CounterValue(t, metrics.RunsHeldUpstreamTotal, "consumer", dataset)
	beforeSkipped := metricstestutil.CounterValue(t, metrics.RunSkippedTotal, "consumer", "dataset_hold")

	created, err := store.Start(jobID, nil)
	require.ErrorIs(t, err, ErrRunHeldUpstream)
	require.ErrorIs(t, err, ErrRunSkipped,
		"every existing skip consumer must keep treating this as 'nothing to execute'")
	require.Nil(t, created, "the caller gets no run to execute — but the row exists")

	var runs []models.JobRun
	require.NoError(t, db.Where("job_id = ?", jobID).Find(&runs).Error)
	require.Len(t, runs, 1, "a hold-skipped run is a row, not an absence")
	assert.Equal(t, string(StatusSkipped), runs[0].Status)
	assert.Equal(t, "dataset_hold:/"+dataset, runs[0].SkipReason)
	require.NotNil(t, runs[0].CompletedAt, "the run is terminal on arrival")

	var taskRuns []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ?", runs[0].ID).Order("partition_index ASC").Find(&taskRuns).Error)
	require.Len(t, taskRuns, 2, "one row per catalog task — including the fanned step")
	for _, row := range taskRuns {
		assert.Equal(t, string(TaskStatusSkipped), row.Status)
		assert.Contains(t, row.Error, "dataset_hold:/"+dataset)
		assert.Contains(t, row.Error, "hold="+hold.ID.String())
		assert.Equal(t, 0, row.PartitionIndex)
		assert.Equal(t, 0, row.PartitionCount,
			"a fanned step collapses to ONE unexpanded template row: its producer never ran")
		assert.NotZero(t, row.TerminalSequence, "every skipped row gets its own terminal sequence")
	}

	assert.Equal(t, beforeHeld+1, metricstestutil.CounterValue(t, metrics.RunsHeldUpstreamTotal, "consumer", dataset))
	assert.Equal(t, beforeSkipped+1, metricstestutil.CounterValue(t, metrics.RunSkippedTotal, "consumer", "dataset_hold"))

	// The events: run_held_upstream once, task_skipped per row, run_started
	// never (nothing started).
	seen := map[event.Type]int{}
	var heldPayload RunHeldUpstreamEvent
	deadline := time.After(2 * time.Second)
	for len(seen) == 0 || seen[event.TypeTaskSkipped] < 2 {
		select {
		case evt := <-events:
			seen[evt.Type]++
			if evt.Type == event.TypeRunHeldUpstream {
				require.NoError(t, json.Unmarshal(evt.Payload, &heldPayload))
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the gate's events; saw %v", seen)
		}
	}
	assert.Equal(t, 1, seen[event.TypeRunHeldUpstream])
	assert.Equal(t, 2, seen[event.TypeTaskSkipped])
	assert.Zero(t, seen[event.TypeRunStarted], "nothing started, so nothing may claim it did")

	assert.Equal(t, hold.ID, heldPayload.HoldID)
	assert.Equal(t, dataset, heldPayload.Dataset)
	assert.Equal(t, "consumer", heldPayload.JobAlias)
	assert.Equal(t, "dataset_hold:/"+dataset, heldPayload.SkipReason)
	assert.Equal(t, 2, heldPayload.SkippedTasks)

	// Persisted, not merely published.
	var stored int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeRunHeldUpstream)).Count(&stored).Error)
	assert.Equal(t, int64(1), stored)
}

// TestAdmitRunsWhenOnUpstreamHoldIsRun proves the per-job opt-out is honoured
// off the PERSISTED column, with no jobdef re-parse on the admission path.
func TestAdmitRunsWhenOnUpstreamHoldIsRun(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	dataset := "warehouse/orders"
	jobID := seedConsumerJob(t, db, "opted-out", dataset, jobdef.OnUpstreamHoldRun)
	seedActiveHold(t, db, dataset)

	created, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, StatusRunning, created.Status)
	assert.Empty(t, created.SkipReason)
}

// TestAdmitIgnoresHoldsOnDatasetsTheJobDoesNotConsume keeps the gate honest:
// it fires on the DECLARED consumes edge, not on "some dataset somewhere is
// held".
func TestAdmitIgnoresHoldsOnDatasetsTheJobDoesNotConsume(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID := seedConsumerJob(t, db, "unrelated", "warehouse/orders", "")
	seedActiveHold(t, db, "warehouse/somethingelse")

	created, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, StatusRunning, created.Status)
}

// TestAdmitIgnoresReleasedHolds proves a released hold reopens the gate — the
// same predicate the clean-run and human-ack paths rely on.
func TestAdmitIgnoresReleasedHolds(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	dataset := "warehouse/orders"
	jobID := seedConsumerJob(t, db, "released-consumer", dataset, "")
	hold := seedActiveHold(t, db, dataset)

	_, err := store.Start(jobID, nil)
	require.ErrorIs(t, err, ErrRunHeldUpstream)

	_, err = store.ReleaseHold(context.Background(), hold.ID, ReleaseHoldParams{ReleasedBy: "user:ada"})
	require.NoError(t, err)

	created, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, StatusRunning, created.Status)
}

// TestAdmitSkipsGateWhenTheFeatureIsOff pins arc convention 1 on the hot path:
// with the master flag off the gate issues no query and no run is refused, even
// with an active hold row present.
func TestAdmitSkipsGateWhenTheFeatureIsOff(t *testing.T) {
	setDataAssertions(t, false)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	dataset := "warehouse/orders"
	jobID := seedConsumerJob(t, db, "flag-off", dataset, "")
	seedActiveHold(t, db, dataset)

	created, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, StatusRunning, created.Status)
}
