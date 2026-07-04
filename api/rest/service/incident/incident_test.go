package incident

import (
	"context"
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

func TestRejectRecordsReason(t *testing.T) {
	svc, db := newTestService(t)
	inc, action, approval := seedAwaitingApproval(t, db, uuid.New(), "auth_failure")

	res, err := svc.Reject(inc.ID, approval.ID, "operator@example.com", "unsafe patch")
	require.NoError(t, err)
	require.Equal(t, models.ApprovalDecisionRejected, res.Approval.Decision)
	require.Equal(t, "unsafe patch", res.Approval.Reason)

	var reloaded models.AgentAction
	require.NoError(t, db.First(&reloaded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusRejected, reloaded.Status)
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
