package incident

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedApprovedAction reproduces the crash window exactly: a tier-3 proposal was
// made, a human approved it (decision committed), and the process died before
// the synchronous dispatch ran. What survives on disk is an `approved` action
// with no execution record — the thing the sweeper must finish.
func seedApprovedAction(t *testing.T, db *gorm.DB, store *Store, exec *Executor, age time.Duration) (*models.Incident, uuid.UUID, uuid.UUID) {
	t.Helper()
	inc, runID := seedIncident(t, store)

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Actor:      models.AgentActionActorAgent,
		Type:       ActionTypeOverrideSchemaGate,
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusProposed, action.Status)

	approveAction(t, db, action.ID, "carol@example.com", "ship it")

	// Age the row past the sweeper's grace period.
	require.NoError(t, db.Model(&models.AgentAction{}).
		Where("id = ?", action.ID).
		Update("updated_at", time.Now().UTC().Add(-age)).Error)

	return inc, action.ID, runID
}

func TestRedriveExecutesStrandedApprovedActionExactlyOnce(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	_, actionID, runID := seedApprovedAction(t, db, store, exec, 10*time.Minute)

	redriver := NewApprovalRedriver(db, exec, nil, time.Second, time.Minute)
	require.NoError(t, redriver.SweepOnce(context.Background()))

	require.Len(t, ops.overrideGate, 1, "the stranded approved action must be dispatched")
	require.Equal(t, runID, ops.overrideGate[0])

	var got models.AgentAction
	require.NoError(t, db.First(&got, "id = ?", actionID).Error)
	require.Equal(t, models.AgentActionStatusExecuted, got.Status)

	var result map[string]any
	require.NoError(t, json.Unmarshal(got.Result, &result))
	require.Equal(t, "carol@example.com", result["approved_by"],
		"the redriven execution must still credit the recorded human decision")

	// A second sweep must find nothing: the row is no longer `approved`.
	require.NoError(t, redriver.SweepOnce(context.Background()))
	require.Len(t, ops.overrideGate, 1, "a redriven action must never execute twice")
}

func TestRedriveSkipsActionsInsideTheGracePeriod(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	// Approved just now — the synchronous dispatch may still be in flight.
	seedApprovedAction(t, db, store, exec, 0)

	redriver := NewApprovalRedriver(db, exec, nil, time.Second, time.Hour)
	require.NoError(t, redriver.SweepOnce(context.Background()))

	require.Empty(t, ops.overrideGate,
		"the sweep must not race the dispatch that normally follows a decision")
}

func TestRedriveLeaderGateSkipsSweep(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	seedApprovedAction(t, db, store, exec, 10*time.Minute)

	redriver := NewApprovalRedriver(db, exec, func(context.Context) (bool, error) {
		return false, nil
	}, time.Second, time.Minute)
	require.NoError(t, redriver.SweepOnce(context.Background()))

	require.Empty(t, ops.overrideGate, "a non-leader must not redrive approved actions")
}

// countingExecutor wraps the real executor so a concurrent second attempt can be
// observed: the claim, not the caller, is what must make dispatch once-only.
type countingExecutor struct {
	inner *Executor
	calls atomic.Int32
}

func (c *countingExecutor) ExecuteApproved(ctx context.Context, actionID uuid.UUID) (*models.AgentAction, error) {
	c.calls.Add(1)
	return c.inner.ExecuteApproved(ctx, actionID)
}

func TestConcurrentExecuteApprovedDispatchesOnce(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	_, actionID, _ := seedApprovedAction(t, db, store, exec, 10*time.Minute)

	counter := &countingExecutor{inner: exec}

	// Two attempts on the same action — the sweeper racing the synchronous
	// post-decision execute. Serial rather than parallel because the test DB is
	// single-connection; the guard under test is the conditional UPDATE, which
	// serialising does not weaken.
	_, first := counter.ExecuteApproved(context.Background(), actionID)
	require.NoError(t, first)
	_, second := counter.ExecuteApproved(context.Background(), actionID)
	require.ErrorIs(t, second, ErrActionNotApproved,
		"the second attempt must lose the claim, not re-run the mutation")

	require.EqualValues(t, 2, counter.calls.Load())
	require.Len(t, ops.overrideGate, 1, "an approved tier-3 action must dispatch exactly once")
}

func TestExecuteApprovedRefusesUnclaimableStatuses(t *testing.T) {
	db, store, _, exec := newExecutorTest(t)
	_, actionID, _ := seedApprovedAction(t, db, store, exec, 0)

	// An action already claimed by another executor (or stranded `executing` by a
	// crash) is never re-dispatched: re-running a half-applied tier-3 mutation
	// unattended is worse than leaving a visible stuck row.
	require.NoError(t, db.Model(&models.AgentAction{}).
		Where("id = ?", actionID).
		Update("status", models.AgentActionStatusExecuting).Error)

	_, err := exec.ExecuteApproved(context.Background(), actionID)
	require.ErrorIs(t, err, ErrActionNotApproved)
}
