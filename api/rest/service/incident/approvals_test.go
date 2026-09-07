package incident

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// approvals_test.go covers the far end of the tier-3 pipeline
// (trust-the-substrate C7): an approved action must actually RUN, and when no
// executor is registered the service must degrade visibly to "approved, not
// executed" rather than pretend the remediation happened.

// fakeApprovedExecutor records the post-approval dispatch.
type fakeApprovedExecutor struct {
	calls []uuid.UUID
	err   error
	db    *gorm.DB
}

func (f *fakeApprovedExecutor) ExecuteApproved(_ context.Context, actionID uuid.UUID) (*models.AgentAction, error) {
	f.calls = append(f.calls, actionID)
	if f.err != nil {
		return nil, f.err
	}
	// Mirror what the real executor does on success, so the test can assert the
	// service ran it AFTER the decision committed.
	if f.db != nil {
		_ = f.db.Model(&models.AgentAction{}).
			Where("id = ?", actionID).
			Update("status", models.AgentActionStatusExecuted).Error
	}
	return &models.AgentAction{ID: actionID, Status: models.AgentActionStatusExecuted}, nil
}

func TestApproveExecutesTheApprovedAction(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })

	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")
	exec := &fakeApprovedExecutor{db: db}
	SetApprovedActionExecutor(exec)

	res, err := svc.Approve(inc.ID, approval.ID, "operator@example.com", "looks correct")
	require.NoError(t, err)
	require.True(t, res.Executed)
	require.Empty(t, res.ExecutionError)

	require.Equal(t, []uuid.UUID{action.ID}, exec.calls,
		"the approved action must be handed to the executor exactly once")

	// The dispatch ran AFTER the decision committed: the row the executor saw is
	// the one the transaction wrote.
	var reloaded models.AgentAction
	require.NoError(t, db.First(&reloaded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusExecuted, reloaded.Status)
}

func TestApproveDegradesWhenNoExecutorRegistered(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })
	SetApprovedActionExecutor(nil)

	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")

	res, err := svc.Approve(inc.ID, approval.ID, "operator@example.com", "")
	require.NoError(t, err, "the decision must still be recorded when nothing can execute it")
	require.False(t, res.Executed)
	require.Equal(t, models.ApprovalDecisionApproved, res.Approval.Decision)

	// approved, NOT executed — the row stays at `approved` so an operator can see
	// the remediation is outstanding.
	var reloaded models.AgentAction
	require.NoError(t, db.First(&reloaded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusApproved, reloaded.Status)
}

func TestApproveReportsExecutionFailureWithoutUndoingTheDecision(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })

	inc, _, approval := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")
	SetApprovedActionExecutor(&fakeApprovedExecutor{err: errors.New("job is git-synced")})

	res, err := svc.Approve(inc.ID, approval.ID, "operator@example.com", "")
	require.NoError(t, err, "a dispatch failure must not roll back the human decision")
	require.False(t, res.Executed)
	require.Contains(t, res.ExecutionError, "git-synced")

	var reloadedApproval models.ApprovalRequest
	require.NoError(t, db.First(&reloadedApproval, "id = ?", approval.ID).Error)
	require.Equal(t, models.ApprovalDecisionApproved, reloadedApproval.Decision,
		"the approval stays approved: it committed before execution was attempted")
}

func TestRejectNeverExecutes(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })

	inc, _, approval := seedAwaitingApproval(t, db, uuid.New(), "auth_failure")
	exec := &fakeApprovedExecutor{db: db}
	SetApprovedActionExecutor(exec)

	res, err := svc.Reject(inc.ID, approval.ID, "operator@example.com", "unsafe patch")
	require.NoError(t, err)
	require.False(t, res.Executed)
	require.Empty(t, exec.calls, "a rejected action must never reach the executor")
}
