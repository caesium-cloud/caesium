package incident

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newIncidentForTimer(t *testing.T, store *Store) *models.Incident {
	t.Helper()
	inc, _, err := store.OpenOrAppend(context.Background(), OpenParams{JobID: uuid.New(), TaskName: "t", Class: ClassUnknown})
	require.NoError(t, err)
	return inc
}

func TestTimerSweepFiresDueTimer(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	inc := newIncidentForTimer(t, store)

	_, err := store.ScheduleTimer(ctx, inc.ID, "snooze_retry", time.Now().Add(-time.Minute), nil, nil, nil)
	require.NoError(t, err)

	sup := NewTimerSupervisor(db, nil, time.Second)
	fired := 0
	sup.RegisterHandler("snooze_retry", func(context.Context, models.RemediationTimer) error {
		fired++
		return nil
	})
	require.NoError(t, sup.SweepOnce(ctx))
	require.Equal(t, 1, fired, "due timer should fire exactly once")

	// A second sweep must not re-fire the already-claimed timer.
	require.NoError(t, sup.SweepOnce(ctx))
	require.Equal(t, 1, fired)

	var timer models.RemediationTimer
	require.NoError(t, db.First(&timer, "incident_id = ?", inc.ID).Error)
	require.Equal(t, models.RemediationTimerStatusFired, timer.Status)
}

func TestTimerNotYetDueDoesNotFire(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	inc := newIncidentForTimer(t, store)

	_, err := store.ScheduleTimer(ctx, inc.ID, "snooze_retry", time.Now().Add(time.Hour), nil, nil, nil)
	require.NoError(t, err)

	sup := NewTimerSupervisor(db, nil, time.Second)
	fired := 0
	sup.RegisterHandler("snooze_retry", func(context.Context, models.RemediationTimer) error { fired++; return nil })
	require.NoError(t, sup.SweepOnce(ctx))
	require.Equal(t, 0, fired)
}

func TestTerminalIncidentCancelsTimer(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	inc := newIncidentForTimer(t, store)

	_, err := store.ScheduleTimer(ctx, inc.ID, "snooze_retry", time.Now().Add(-time.Minute), nil, nil, nil)
	require.NoError(t, err)

	// Closing the incident must cancel its pending timers (A6 invariant): a stale
	// timer can never fire a retry against a closed incident.
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusEscalated, "")
	require.NoError(t, err)
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusClosed, "")
	require.NoError(t, err)

	var timer models.RemediationTimer
	require.NoError(t, db.First(&timer, "incident_id = ?", inc.ID).Error)
	require.Equal(t, models.RemediationTimerStatusCancelled, timer.Status)

	sup := NewTimerSupervisor(db, nil, time.Second)
	fired := 0
	sup.RegisterHandler("snooze_retry", func(context.Context, models.RemediationTimer) error { fired++; return nil })
	require.NoError(t, sup.SweepOnce(ctx))
	require.Equal(t, 0, fired, "cancelled timer must never fire")
}

func TestLeaderGateSkipsSweep(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	inc := newIncidentForTimer(t, store)
	_, err := store.ScheduleTimer(ctx, inc.ID, "snooze_retry", time.Now().Add(-time.Minute), nil, nil, nil)
	require.NoError(t, err)

	sup := NewTimerSupervisor(db, func(context.Context) (bool, error) { return false, nil }, time.Second)
	fired := 0
	sup.RegisterHandler("snooze_retry", func(context.Context, models.RemediationTimer) error { fired++; return nil })
	require.NoError(t, sup.SweepOnce(ctx))
	require.Equal(t, 0, fired, "non-leader must not fire timers")
}
