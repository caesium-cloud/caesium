package incident

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenOrAppendOpensThenAppends(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()

	p := OpenParams{JobID: jobID, TaskName: "extract", Class: ClassDataUnavailable, LastError: "boom"}

	inc1, outcome, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	require.Equal(t, 1, inc1.OccurrenceCount)

	// A second independent same-key failure must append an occurrence, not open
	// a twin (the A4 acceptance invariant).
	inc2, outcome, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	require.Equal(t, OutcomeAppended, outcome)
	require.Equal(t, inc1.ID, inc2.ID)
	require.Equal(t, 2, inc2.OccurrenceCount)

	testutil.AssertCount(t, db, &models.Incident{}, 1)
}

func TestOpenOrAppendDistinctClassOpensSeparate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()

	_, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "t", Class: ClassAuthFailure})
	require.NoError(t, err)
	_, outcome, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "t", Class: ClassOOM})
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	testutil.AssertCount(t, db, &models.Incident{}, 2)
}

func TestTerminalTransitionFreesDedupeKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()
	p := OpenParams{JobID: jobID, TaskName: "t", Class: ClassUnknown}

	inc, _, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)

	// Drive it to a terminal state; the active dedupe key must clear so a fresh
	// same-key failure opens a NEW incident.
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusTriaging, "")
	require.NoError(t, err)
	closed, err := store.Remediate(ctx, inc.ID, "fixed")
	require.NoError(t, err)
	require.Equal(t, models.IncidentStatusClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)

	inc2, outcome, err := store.OpenOrAppend(ctx, p) // no cooldown
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	require.NotEqual(t, inc.ID, inc2.ID)
	testutil.AssertCount(t, db, &models.Incident{}, 2)
}

func TestOpenForJobTaskMatchesTaskNameExactly(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()

	// One per-task incident and one run-level incident (empty task name) for the
	// same job.
	_, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassDataUnavailable})
	require.NoError(t, err)
	_, _, err = store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "", Class: ClassSLARisk})
	require.NoError(t, err)

	// A per-task success (task_succeeded → task name) must match ONLY that task's
	// incident.
	perTask, err := store.OpenForJobTask(ctx, jobID, "extract")
	require.NoError(t, err)
	require.Len(t, perTask, 1)
	require.Equal(t, "extract", perTask[0].TaskName)

	// A run-level success (run_completed → empty task name) must match ONLY the
	// run-level incident, never wildcard-close the per-task one.
	runLevel, err := store.OpenForJobTask(ctx, jobID, "")
	require.NoError(t, err)
	require.Len(t, runLevel, 1)
	require.Equal(t, "", runLevel[0].TaskName)
	require.Equal(t, string(ClassSLARisk), runLevel[0].Class)
}

func TestInvalidTransitionRejected(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: uuid.New(), TaskName: "t", Class: ClassUnknown})
	require.NoError(t, err)

	// open → closed is not a valid direct transition.
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusClosed, "")
	require.ErrorIs(t, err, ErrInvalidTransition)
}

func TestCooldownSuppressesReopen(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()
	p := OpenParams{JobID: jobID, TaskName: "t", Class: ClassUnknown, Cooldown: time.Hour}

	inc, _, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusEscalated, "")
	require.NoError(t, err)
	_, err = store.Transition(ctx, inc.ID, models.IncidentStatusClosed, "")
	require.NoError(t, err)

	// Within the cooldown window a fresh same-key failure is suppressed.
	_, outcome, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	require.Equal(t, OutcomeSuppressed, outcome)
	testutil.AssertCount(t, db, &models.Incident{}, 1)
}

// TestOpenOrAppendOnOpenCommitsAtomicallyWithIncident is the happy-path half
// of the #459 review's atomicity contract: OnOpen's write (a durable
// companion event, in production) must land in the SAME transaction as the
// incident insert, not a separate one after it.
func TestOpenOrAppendOnOpenCommitsAtomicallyWithIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()

	p := OpenParams{JobID: jobID, TaskName: "extract", Class: ClassDataUnavailable, LastError: "boom"}
	p.OnOpen = func(tx *gorm.DB, inc *models.Incident) error {
		return tx.Create(&models.ExecutionEvent{
			Type:      "incident_opened",
			JobID:     &inc.JobID,
			CreatedAt: time.Now().UTC(),
		}).Error
	}

	inc, outcome, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	testutil.AssertCount(t, db, &models.Incident{}, 1)

	var events int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("job_id = ? AND type = ?", inc.JobID, "incident_opened").
		Count(&events).Error)
	require.EqualValues(t, 1, events,
		"OnOpen's write must be visible once OpenOrAppend returns, committed alongside the incident")
}

// TestOpenOrAppendOnOpenFailureRollsBackIncident is the #459 review's
// reproduction, fixed: "one-shot execution_events insert failure, restore
// writes, redeliver the failure" used to leave one incident with
// occurrence_count=2 and zero incident_opened rows, because the incident
// commit and the event insert were two separate transactions. Now OnOpen runs
// INSIDE the incident-create transaction, so a failing companion write rolls
// the incident insert back too — nothing survives the first attempt — and a
// redelivery of the same failure opens a genuinely fresh incident (not an
// appended occurrence of a ghost row) once the companion write succeeds.
func TestOpenOrAppendOnOpenFailureRollsBackIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()
	jobID := uuid.New()

	injected := errors.New("injected event insert failure")
	p := OpenParams{JobID: jobID, TaskName: "extract", Class: ClassDataUnavailable, LastError: "boom"}
	p.OnOpen = func(tx *gorm.DB, inc *models.Incident) error {
		return injected
	}

	_, _, err := store.OpenOrAppend(ctx, p)
	require.ErrorIs(t, err, injected)

	// Nothing survives the failed attempt: the incident insert rolled back
	// with the failed companion write, not just the companion write on its
	// own — this is the crux of the fix.
	testutil.AssertCount(t, db, &models.Incident{}, 0)
	var events int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Count(&events).Error)
	require.EqualValues(t, 0, events)

	// Redelivery: the same underlying failure is processed again. The
	// transient condition has cleared, so OnOpen succeeds this time.
	var openCalls int
	p.OnOpen = func(tx *gorm.DB, inc *models.Incident) error {
		openCalls++
		return tx.Create(&models.ExecutionEvent{
			Type:      "incident_opened",
			JobID:     &inc.JobID,
			CreatedAt: time.Now().UTC(),
		}).Error
	}
	inc, outcome, err := store.OpenOrAppend(ctx, p)
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome,
		"redelivery after a fully rolled-back attempt must open fresh, not append to a ghost incident")
	require.Equal(t, 1, inc.OccurrenceCount)
	require.Equal(t, 1, openCalls)

	testutil.AssertCount(t, db, &models.Incident{}, 1)
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Count(&events).Error)
	require.EqualValues(t, 1, events,
		"the companion event must exist after redelivery succeeds — the #459 gap")
}
