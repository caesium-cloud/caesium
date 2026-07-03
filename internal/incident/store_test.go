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
