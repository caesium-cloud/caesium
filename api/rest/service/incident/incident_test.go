package incident

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newTestService builds a Service over a migrated in-memory DB (white-box: the
// production New() would open the dqlite router).
func newTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return &Service{ctx: context.Background(), db: db}, db
}

// seedAwaitingApproval creates an incident parked in awaiting_approval with a
// tier-3 action and a pending approval, as Stream B would produce.
func seedAwaitingApproval(t *testing.T, db *gorm.DB, jobID uuid.UUID, class string) (models.Incident, models.AgentAction, models.ApprovalRequest) {
	t.Helper()
	now := time.Now().UTC()
	key := jobID.String() + "|task|" + class

	inc := models.Incident{
		ID:              uuid.New(),
		JobID:           jobID,
		TaskName:        "task",
		Class:           class,
		Status:          models.IncidentStatusAwaitingApproval,
		DedupeKey:       key,
		ActiveDedupeKey: &key,
		OccurrenceCount: 1,
		OpenedAt:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, db.Create(&inc).Error)

	action := models.AgentAction{
		ID:         uuid.New(),
		IncidentID: inc.ID,
		Type:       "apply_jobdef_patch",
		Tier:       3,
		Status:     models.AgentActionStatusProposed,
		Actor:      models.AgentActionActorAgent,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, db.Create(&action).Error)

	approval := models.ApprovalRequest{
		ID:         uuid.New(),
		IncidentID: inc.ID,
		ActionID:   action.ID,
		Decision:   models.ApprovalDecisionPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, db.Create(&approval).Error)

	return inc, action, approval
}

func TestListFiltersAndPaginates(t *testing.T) {
	svc, db := newTestService(t)
	jobA, jobB := uuid.New(), uuid.New()
	seedAwaitingApproval(t, db, jobA, "schema_violation")
	seedAwaitingApproval(t, db, jobB, "data_unavailable")

	all, err := svc.List(ListParams{})
	require.NoError(t, err)
	require.Equal(t, int64(2), all.Total)
	require.Len(t, all.Incidents, 2)

	byClass, err := svc.List(ListParams{Class: "schema_violation"})
	require.NoError(t, err)
	require.Equal(t, int64(1), byClass.Total)

	byJob, err := svc.List(ListParams{JobID: &jobB})
	require.NoError(t, err)
	require.Equal(t, int64(1), byJob.Total)
	require.Equal(t, jobB, byJob.Incidents[0].JobID)

	needs, err := svc.List(ListParams{NeedsApproval: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), needs.Total)

	none, err := svc.List(ListParams{Status: string(models.IncidentStatusOpen)})
	require.NoError(t, err)
	require.Equal(t, int64(0), none.Total)
}

func TestGetReturnsTimeline(t *testing.T) {
	svc, db := newTestService(t)
	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "unknown")

	detail, err := svc.Get(inc.ID)
	require.NoError(t, err)
	require.Equal(t, inc.ID, detail.Incident.ID)
	require.Len(t, detail.Actions, 1)
	require.Equal(t, action.ID, detail.Actions[0].ID)
	require.Len(t, detail.Approvals, 1)
	require.Equal(t, approval.ID, detail.Approvals[0].ID)

	_, err = svc.Get(uuid.New())
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestApproveResolvesAndResumesIncident(t *testing.T) {
	svc, db := newTestService(t)
	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")

	res, err := svc.Approve(inc.ID, approval.ID, "operator@example.com", "looks correct")
	require.NoError(t, err)
	require.Equal(t, models.ApprovalDecisionApproved, res.Approval.Decision)
	require.Equal(t, "operator@example.com", res.Approval.Decider)
	require.True(t, res.StatusChanged)
	require.Equal(t, models.IncidentStatusTriaging, res.Incident.Status)

	// The audit-spine action mirrors the decision.
	var reloaded models.AgentAction
	require.NoError(t, db.First(&reloaded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusApproved, reloaded.Status)

	// Deciding again is refused — decisions are once-only.
	_, err = svc.Approve(inc.ID, approval.ID, "operator@example.com", "")
	require.ErrorIs(t, err, ErrApprovalNotPending)
}

func TestRejectEscalatesAndDoesNotResumeTriage(t *testing.T) {
	svc, db := newTestService(t)
	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "auth_failure")

	res, err := svc.Reject(inc.ID, approval.ID, "operator@example.com", "unsafe patch")
	require.NoError(t, err)
	require.Equal(t, models.ApprovalDecisionRejected, res.Approval.Decision)
	require.Equal(t, "unsafe patch", res.Approval.Reason)
	require.True(t, res.StatusChanged)

	// A rejection must NOT resume triaging (which would let the agent re-propose
	// the same action) — it escalates so a human owns the incident.
	require.Equal(t, models.IncidentStatusEscalated, res.Incident.Status)
	var reloadedInc models.Incident
	require.NoError(t, db.First(&reloadedInc, "id = ?", inc.ID).Error)
	require.Equal(t, models.IncidentStatusEscalated, reloadedInc.Status)

	// The rejected action is stamped rejected (excluded from any re-proposal).
	var reloaded models.AgentAction
	require.NoError(t, db.First(&reloaded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusRejected, reloaded.Status)
}

func TestConcurrentDecideRaceHasSingleWinner(t *testing.T) {
	svc, db := newTestService(t)
	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")

	// Two operators race to decide the same approval: exactly one must win; the
	// others must be refused (ErrApprovalNotPending) so the audit is never
	// overwritten by a later commit.
	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		refused int
		winner  models.ApprovalDecision
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		decision := models.ApprovalDecisionApproved
		if i%2 == 1 {
			decision = models.ApprovalDecisionRejected
		}
		wg.Add(1)
		go func(d models.ApprovalDecision) {
			defer wg.Done()
			<-start
			var derr error
			if d == models.ApprovalDecisionApproved {
				_, derr = svc.Approve(inc.ID, approval.ID, "op", "")
			} else {
				_, derr = svc.Reject(inc.ID, approval.ID, "op", "")
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case derr == nil:
				wins++
				winner = d
			case errors.Is(derr, ErrApprovalNotPending):
				refused++
			default:
				t.Errorf("unexpected error: %v", derr)
			}
		}(decision)
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, wins, "exactly one decision may succeed")
	require.Equal(t, racers-1, refused, "all other deciders must be refused")

	// The persisted decision matches the sole winner and is no longer pending.
	var finalApproval models.ApprovalRequest
	require.NoError(t, db.First(&finalApproval, "id = ?", approval.ID).Error)
	require.Equal(t, winner, finalApproval.Decision)

	// The audit-spine action mirror matches the winner too (no drift).
	var finalAction models.AgentAction
	require.NoError(t, db.First(&finalAction, "id = ?", action.ID).Error)
	expected := models.AgentActionStatusApproved
	if winner == models.ApprovalDecisionRejected {
		expected = models.AgentActionStatusRejected
	}
	require.Equal(t, expected, finalAction.Status)
}

func TestDecideRejectsIncidentMismatch(t *testing.T) {
	svc, db := newTestService(t)
	_, _, approval := seedAwaitingApproval(t, db, uuid.New(), "quota")

	// A different incident id in the route must not resolve this approval.
	_, err := svc.Approve(uuid.New(), approval.ID, "operator@example.com", "")
	require.ErrorIs(t, err, ErrApprovalIncidentMismatch)

	_, err = svc.Approve(uuid.New(), uuid.New(), "operator@example.com", "")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
