package incident

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
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

// TestListWithholdsUnownedBaselineLastError: a finance-confined list loses a
// pre-upgrade baseline quoted in LastError, using the verdicts of the task
// the incident names. A namespace-owned verdict, a sibling task's verdict,
// a payload that does not decode, and a missing task row leave the text
// alone. An unrestricted list and the stored column still carry the median.
func TestListWithholdsUnownedBaselineLastError(t *testing.T) {
	svc, db := newTestService(t)
	now := time.Now().UTC()
	jobID, runID := uuid.New(), uuid.New()
	taskID, otherTask := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Namespace: "finance", Status: "failed",
		TriggerType: "cron", TriggerAlias: "nightly", StartedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "process"}).Error)
	require.NoError(t, db.Create(&models.Task{ID: otherTask, JobID: jobID, AtomID: uuid.New(), Name: "other"}).Error)

	legacyMessage := "observed 10 deviates from the baseline median 31337 (5 sample(s))"
	ownedMessage := "own baseline median 42"
	foreignMessage := "foreign baseline median 88888"
	legacyJSON := `[{"dataset":"d","metric":"rowCount","assertion":"deltaFromBaseline","observed":10,"bound":15668.5,"baseline_median":31337,"baseline_samples":5,"message":"` + legacyMessage + `"}]`
	ownedJSON := `[{"dataset":"d","metric":"rowCount","assertion":"deltaFromBaseline","baseline_scope":"namespace","baseline_median":42,"baseline_samples":6,"message":"` + ownedMessage + `"}]`
	foreignJSON := `[{"dataset":"d","metric":"rowCount","assertion":"deltaFromBaseline","baseline_median":88888,"baseline_samples":3,"message":"` + foreignMessage + `"}]`

	seedRow := func(task uuid.UUID, violations string) {
		t.Helper()
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: task, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: `["echo"]`,
			Status: "failed", Attempt: 1, DataViolations: datatypes.JSON(violations),
			CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	seedRow(taskID, legacyJSON)
	seedRow(otherTask, foreignJSON)
	ownedTask := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: ownedTask, JobID: jobID, AtomID: uuid.New(), Name: "owned"}).Error)
	seedRow(ownedTask, ownedJSON)
	badTask := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: badTask, JobID: jobID, AtomID: uuid.New(), Name: "bad"}).Error)
	seedRow(badTask, "not-json")

	allowlist := datatypes.JSON(`["other-job"]`)
	seedIncident := func(task *uuid.UUID, lastError string, opened time.Time) models.Incident {
		t.Helper()
		inc := models.Incident{
			ID: uuid.New(), Namespace: "finance", JobID: jobID, RunID: &runID, TaskID: task,
			Class: "b4_legacy_baseline", Status: models.IncidentStatusClosed,
			DedupeKey: "b4/" + uuid.NewString(), AllowedJobs: allowlist, LastError: lastError,
			OpenedAt: opened, ClosedAt: &opened, CreatedAt: opened, UpdatedAt: opened,
		}
		require.NoError(t, db.Create(&inc).Error)
		return inc
	}
	legacy := seedIncident(&taskID, "task violates its declared data assertions: "+legacyMessage, now.Add(4*time.Second))
	owned := seedIncident(&ownedTask, ownedMessage, now.Add(3*time.Second))
	sibling := seedIncident(&taskID, foreignMessage, now.Add(2*time.Second))
	bad := seedIncident(&badTask, "baseline median 77777", now.Add(time.Second))
	missingTask := uuid.New()
	missing := seedIncident(&missingTask, "baseline median 66666", now)
	var noTask *uuid.UUID
	unscoped := seedIncident(noTask, "task violates its declared data assertions: "+legacyMessage, now.Add(5*time.Second))

	restricted := &Service{ctx: authz.WithAllowedNamespaces(context.Background(), []string{"finance"}), db: db}
	listed, err := restricted.List(ListParams{JobID: &jobID})
	require.NoError(t, err)
	byID := map[uuid.UUID]models.Incident{}
	for _, inc := range listed.Incidents {
		byID[inc.ID] = inc
		require.Nil(t, inc.AllowedJobs)
	}
	require.Contains(t, byID[legacy.ID].LastError, run.BaselineEvidenceWithheld)
	require.NotContains(t, byID[legacy.ID].LastError, "31337")
	require.NotContains(t, byID[legacy.ID].LastError, "15668")
	require.Equal(t, ownedMessage, byID[owned.ID].LastError)
	require.Equal(t, foreignMessage, byID[sibling.ID].LastError, "a sibling task's verdict does not rewrite this task's error")
	require.Equal(t, "baseline median 77777", byID[bad.ID].LastError)
	require.Equal(t, "baseline median 66666", byID[missing.ID].LastError)
	require.Contains(t, byID[unscoped.ID].LastError, run.BaselineEvidenceWithheld)
	require.NotContains(t, byID[unscoped.ID].LastError, "31337")

	open, err := svc.List(ListParams{JobID: &jobID})
	require.NoError(t, err)
	openByID := map[uuid.UUID]models.Incident{}
	for _, inc := range open.Incidents {
		openByID[inc.ID] = inc
	}
	require.Contains(t, openByID[legacy.ID].LastError, "31337")
	require.JSONEq(t, string(allowlist), string(openByID[legacy.ID].AllowedJobs))

	var stored models.Incident
	require.NoError(t, db.First(&stored, "id = ?", legacy.ID).Error)
	require.Contains(t, stored.LastError, "31337", "the persisted last_error is unchanged")
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
	for i := range racers {
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

// seedPendingApproval adds a SECOND (third, …) tier-3 action + pending approval
// to an incident that is already parked in awaiting_approval — the shape
// parkAwaitingApprovalTx produces when a later proposal lands on an incident it
// finds already parked (the park is a no-op, the approval row is not).
func seedPendingApproval(t *testing.T, db *gorm.DB, inc models.Incident, actionType string) (models.AgentAction, models.ApprovalRequest) {
	t.Helper()
	now := time.Now().UTC()

	action := models.AgentAction{
		ID:         uuid.New(),
		IncidentID: inc.ID,
		Type:       actionType,
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

	return action, approval
}

// TestDecideKeepsIncidentParkedWhileAnotherApprovalIsPending is the state-machine
// half of #417: an incident that still owes a human a decision must not read as
// triaging/escalated. Deciding the first of two pending approvals used to advance
// the incident unconditionally, which both mis-stated the incident's state and
// (through the status-based feed) hid the second approval from every operator
// surface.
func TestDecideKeepsIncidentParkedWhileAnotherApprovalIsPending(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })

	inc, _, first := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")
	_, second := seedPendingApproval(t, db, inc, "skip_task")

	res, err := svc.Approve(inc.ID, first.ID, "operator@example.com", "the patch is right")
	require.NoError(t, err)
	require.Equal(t, models.ApprovalDecisionApproved, res.Approval.Decision)
	require.False(t, res.StatusChanged,
		"an incident that still owes a decision on another approval must not advance")
	require.Equal(t, models.IncidentStatusAwaitingApproval, res.Incident.Status)

	var parked models.Incident
	require.NoError(t, db.First(&parked, "id = ?", inc.ID).Error)
	require.Equal(t, models.IncidentStatusAwaitingApproval, parked.Status)

	// …so the operator feed still surfaces it, and the second approval is still
	// reachable and still decidable.
	feed, err := svc.List(ListParams{NeedsApproval: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), feed.Total)
	require.Equal(t, inc.ID, feed.Incidents[0].ID)

	final, err := svc.Reject(inc.ID, second.ID, "operator@example.com", "one skip is enough")
	require.NoError(t, err)
	require.True(t, final.StatusChanged, "the LAST decision advances the incident")
	require.Equal(t, models.IncidentStatusEscalated, final.Incident.Status)

	empty, err := svc.List(ListParams{NeedsApproval: true})
	require.NoError(t, err)
	require.Equal(t, int64(0), empty.Total, "a fully decided incident leaves the approval feed")
}

// TestListNeedsApprovalTracksPendingApprovalRows is the feed half of #417: the
// filter answers "is a human still owed a decision here", which is a property of
// the ApprovalRequest rows, not of the incident's status column.
func TestListNeedsApprovalTracksPendingApprovalRows(t *testing.T) {
	svc, db := newTestService(t)

	// Parked with a pending approval — listed.
	parked, _, _ := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")

	// Parked but every approval decided — NOT listed: there is nothing to decide.
	decided, _, decidedApproval := seedAwaitingApproval(t, db, uuid.New(), "quota")
	require.NoError(t, db.Model(&models.ApprovalRequest{}).
		Where("id = ?", decidedApproval.ID).
		Update("decision", models.ApprovalDecisionApproved).Error)

	// Advanced out of awaiting_approval by a human take-over while an approval is
	// still pending — listed, because the decision is still owed.
	takenOver, _, _ := seedAwaitingApproval(t, db, uuid.New(), "auth_failure")
	require.NoError(t, db.Model(&models.Incident{}).
		Where("id = ?", takenOver.ID).
		Update("status", models.IncidentStatusEscalated).Error)

	feed, err := svc.List(ListParams{NeedsApproval: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), feed.Total)

	listed := map[uuid.UUID]bool{}
	for _, inc := range feed.Incidents {
		listed[inc.ID] = true
	}
	require.True(t, listed[parked.ID], "an incident with a pending approval must be listed")
	require.True(t, listed[takenOver.ID], "a pending approval is owed regardless of incident status")
	require.False(t, listed[decided.ID], "an incident with nothing left to decide must not be listed")

	// The filter composes with the other filters rather than replacing them.
	scoped, err := svc.List(ListParams{NeedsApproval: true, Class: "auth_failure"})
	require.NoError(t, err)
	require.Equal(t, int64(1), scoped.Total)
	require.Equal(t, takenOver.ID, scoped.Incidents[0].ID)
}

// TestConcurrentDecideOnSiblingApprovalsAdvancesOnce proves two operators
// deciding two DIFFERENT approvals on one incident cannot both believe they are
// last: the pending re-check runs inside the decision transaction, behind the
// same incident row lock the transition takes, so exactly one decision advances
// the incident and none leaves it parked with nothing left to decide.
func TestConcurrentDecideOnSiblingApprovalsAdvancesOnce(t *testing.T) {
	svc, db := newTestService(t)
	t.Cleanup(func() { SetApprovedActionExecutor(nil) })

	inc, _, first := seedAwaitingApproval(t, db, uuid.New(), "schema_violation")
	_, second := seedPendingApproval(t, db, inc, "skip_task")
	_, third := seedPendingApproval(t, db, inc, "override_schema_gate")

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		advanced int
	)
	start := make(chan struct{})
	for _, id := range []uuid.UUID{first.ID, second.ID, third.ID} {
		wg.Add(1)
		go func(approvalID uuid.UUID) {
			defer wg.Done()
			<-start
			res, err := svc.Approve(inc.ID, approvalID, "op", "")
			if err != nil {
				t.Errorf("approve %s: %v", approvalID, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if res.StatusChanged {
				advanced++
			}
		}(id)
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, advanced, "exactly one decision may advance the incident")

	var pending int64
	require.NoError(t, db.Model(&models.ApprovalRequest{}).
		Where("incident_id = ? AND decision = ?", inc.ID, models.ApprovalDecisionPending).
		Count(&pending).Error)
	require.Zero(t, pending)

	var final models.Incident
	require.NoError(t, db.First(&final, "id = ?", inc.ID).Error)
	require.Equal(t, models.IncidentStatusTriaging, final.Status,
		"the incident must not be left parked once every approval is decided")
}
