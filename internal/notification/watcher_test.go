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
	"gorm.io/gorm"
)

func TestWatcherSkipsQuarantinedRunningRunsBeforeEmitting(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	bus := event.New()
	store := event.NewStore(db)
	watcher := NewWatcher(db, bus, store, time.Second)

	now := time.Now().UTC()
	normalRunID := seedWatcherRun(t, db, "watcher-normal", now, false)
	quarantinedRunID := seedWatcherRun(t, db, "watcher-quarantine", now, true)

	watcher.scanRunningRuns(context.Background(), now)

	var normalEvents int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("run_id = ? AND type = ?", normalRunID, string(event.TypeRunTimedOut)).
		Count(&normalEvents).Error)
	require.EqualValues(t, 1, normalEvents, "control run should prove the watcher producer is active")

	var quarantinedEvents int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("run_id = ? AND type = ?", quarantinedRunID, string(event.TypeRunTimedOut)).
		Count(&quarantinedEvents).Error)
	require.Zero(t, quarantinedEvents, "watcher must not emit timeout/SLA events for quarantined replay runs")
}

func seedWatcherRun(t *testing.T, db *gorm.DB, alias string, now time.Time, quarantined bool) uuid.UUID {
	t.Helper()
	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     alias + "-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:         uuid.New(),
		Alias:      alias,
		TriggerID:  trigger.ID,
		RunTimeout: time.Nanosecond,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, db.Create(job).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:           runID,
		JobID:        job.ID,
		TriggerID:    trigger.ID,
		TriggerType:  string(trigger.Type),
		TriggerAlias: trigger.Alias,
		Status:       "running",
		Quarantine:   quarantined,
		StartedAt:    now.Add(-time.Second),
		CreatedAt:    now.Add(-time.Second),
		UpdatedAt:    now.Add(-time.Second),
	}).Error)
	return runID
}
