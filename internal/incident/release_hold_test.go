package incident

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const releaseDataset = "orders.daily"

// releaseWorld is a real hold with its real incident, and an executor over a
// fake ActionOps whose release runs the boundary guard, as the transaction would.
type releaseWorld struct {
	*holdWorld
	store  *Store
	ops    *fakeOps
	exec   *Executor
	holdID uuid.UUID
	inc    models.Incident
}

func newReleaseWorld(t *testing.T, namespace string) *releaseWorld {
	t.Helper()
	w := &releaseWorld{holdWorld: newHoldWorld(t, namespace, time.Hour)}
	w.store = NewStore(w.db)
	w.ops = &fakeOps{releaseDB: w.db}
	w.exec = NewExecutor(w.store, w.ops)
	w.declare(w.jobID, releaseDataset)
	w.holdID = w.heldHold(releaseDataset)
	w.sub.handle(context.Background(), w.heldEvent(w.holdID, releaseDataset))
	w.inc = w.only()
	return w
}

// declare records job as a producer of dataset, with the NULL dataset namespace
// the registry writes.
func (w *releaseWorld) declare(jobID uuid.UUID, dataset string) {
	w.t.Helper()
	require.NoError(w.t, w.db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, Name: dataset, Direction: models.DatasetDirectionProduces, CreatedAt: time.Now().UTC(),
	}).Error)
}

// heldHold opens a hold whose row carries the opening run and a min verdict.
func (w *releaseWorld) heldHold(dataset string) uuid.UUID {
	w.t.Helper()
	id := w.hold(dataset, nil)
	require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).Updates(map[string]any{
		"held_by_run_id": w.runID,
		"violations":     datatypes.JSON(`[{"metric":"rowCount","assertion":"min","observed":12,"bound":1000}]`),
	}).Error)
	return id
}

func (w *releaseWorld) foreignJob(alias, namespace string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	require.NoError(w.t, w.db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace}).Error)
	return id
}

func (w *releaseWorld) holdStatus(id uuid.UUID) string {
	w.t.Helper()
	var hold models.DatasetHold
	require.NoError(w.t, w.db.Select("id", "status").First(&hold, "id = ?", id).Error)
	return hold.Status
}

func releaseAllowed() Playbook {
	return Playbook{Allow: map[string]bool{ActionTypeReleaseHold: true}}
}

func (w *releaseWorld) act(params ActionParams) (*models.AgentAction, error) {
	return w.exec.Execute(context.Background(), ActionRequest{
		IncidentID: w.inc.ID,
		Actor:      models.AgentActionActorAgent,
		Type:       ActionTypeReleaseHold,
		Params:     params,
		Playbook:   releaseAllowed(),
	})
}

func actionResult(t *testing.T, action *models.AgentAction) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal(action.Result, &result))
	return result
}

// approve is the approvals service's decision: the row, the action and the
// incident move together, then the action runs.
func (w *releaseWorld) approve(actionID uuid.UUID, decider string) (*models.AgentAction, error) {
	w.t.Helper()
	now := time.Now().UTC()
	require.NoError(w.t, w.db.Model(&models.ApprovalRequest{}).Where("action_id = ?", actionID).
		Updates(map[string]any{"decision": models.ApprovalDecisionApproved, "decider": decider, "decided_at": now}).Error)
	require.NoError(w.t, w.db.Model(&models.AgentAction{}).Where("id = ?", actionID).
		Update("status", models.AgentActionStatusApproved).Error)
	require.NoError(w.t, w.db.Model(&models.Incident{}).Where("id = ? AND status = ?", w.inc.ID, models.IncidentStatusAwaitingApproval).
		Update("status", models.IncidentStatusTriaging).Error)
	return w.exec.ExecuteApproved(context.Background(), actionID)
}

func TestReleaseHoldTierDependsOnTolerance(t *testing.T) {
	tier, ok := ActionTier(ActionTypeReleaseHold)
	require.True(t, ok)
	require.Equal(t, TierGated, tier)

	tier, ok = requestTier(ActionTypeReleaseHold, ActionParams{})
	require.True(t, ok)
	require.Equal(t, TierGated, tier)

	tier, ok = requestTier(ActionTypeReleaseHold, ActionParams{Tolerate: map[string]string{"min": "24h"}})
	require.True(t, ok)
	require.Equal(t, TierApproval, tier)

	tier, ok = requestTier(ActionTypePauseJob, ActionParams{Tolerate: map[string]string{"min": "24h"}})
	require.True(t, ok)
	require.Equal(t, TierGated, tier, "only release_hold reads tolerance windows")
}

func TestReleaseHoldNeedsAnExplicitAllow(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.exec.Execute(context.Background(), ActionRequest{
		IncidentID: w.inc.ID, Type: ActionTypeReleaseHold, Params: ActionParams{Reason: "backfilled"}, Playbook: Playbook{},
	})
	require.ErrorIs(t, err, ErrActionNotPermitted)
	require.Equal(t, models.AgentActionStatusRejected, action.Status)
	require.Empty(t, w.ops.releaseHold)
}

// A run-less incident has nothing else to target, so the implicit hold is what
// makes remediation possible there at all.
func TestReleaseHoldDefaultsToTheIncidentsOwnHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runless bool
	}{{name: "with a run"}, {name: "run-less", runless: true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newReleaseWorld(t, "finance")
			if tc.runless {
				require.NoError(t, w.db.Model(&models.Incident{}).Where("id = ?", w.inc.ID).Updates(map[string]any{
					"run_id": nil, "remediation_target_run_id": nil,
				}).Error)
			}
			action, err := w.act(ActionParams{Reason: " backfilled "})
			require.NoError(t, err)
			require.Equal(t, models.AgentActionStatusExecuted, action.Status)
			require.Equal(t, TierGated, action.Tier)
			require.Len(t, w.ops.releaseHold, 1)
			got := w.ops.releaseHold[0]
			require.Equal(t, w.holdID, got.HoldID)
			require.Equal(t, "backfilled", got.Reason)
			require.Equal(t, "finance", got.Namespace)
			require.Equal(t, "agent:incident:"+w.inc.ID.String(), got.Principal)
			require.Equal(t, releaseDataset, actionResult(t, action)["dataset"])
		})
	}
}

func TestReleaseHoldNamesTheProposingSession(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	session := uuid.New()
	_, err := w.exec.Execute(context.Background(), ActionRequest{
		IncidentID: w.inc.ID, SessionID: &session, Type: ActionTypeReleaseHold,
		Params: ActionParams{Reason: "backfilled"}, Playbook: releaseAllowed(),
	})
	require.NoError(t, err)
	require.Equal(t, "agent:session:"+session.String(), w.ops.releaseHold[0].Principal)
}

func TestReleaseHoldIncidentOfAnotherClassMustNameTheHold(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	inc, _, err := w.store.OpenOrAppend(context.Background(), OpenParams{
		Namespace: "finance", JobID: w.jobID, TaskName: "other", Class: ClassTransientInfra,
	})
	require.NoError(t, err)

	req := ActionRequest{IncidentID: inc.ID, Type: ActionTypeReleaseHold, Params: ActionParams{Reason: "x"}, Playbook: releaseAllowed()}
	action, err := w.exec.Execute(context.Background(), req)
	require.ErrorContains(t, err, "requires a hold id")
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
	require.Empty(t, w.ops.releaseHold)

	req.Params.HoldID = &w.holdID
	_, err = w.exec.Execute(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, w.ops.releaseHold, 1)
}

// The plan's boundary is the job's datasets, so a sibling hold is in reach. Its
// release closes its own incident, not this one.
func TestReleaseHoldOfAnotherDatasetOfTheSameJob(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	sibling := w.heldHold("orders.hourly")
	w.declare(w.jobID, "orders.hourly")

	action, err := w.act(ActionParams{HoldID: &sibling, Reason: "backfilled"})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)
	require.Equal(t, sibling, w.ops.releaseHold[0].HoldID)
}

// TestReleaseHoldBoundary is the security boundary. Each case refuses on both
// roads: dispatch (a playbook allowed it) and the proposal (a tolerance sent it
// to a human, who must never be asked).
func TestReleaseHoldBoundary(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *releaseWorld) uuid.UUID
		want  error
	}{
		{
			name:  "unknown hold",
			setup: func(*releaseWorld) uuid.UUID { return uuid.New() },
			want:  ErrCrossBoundaryTarget,
		},
		{
			name: "hold of a foreign job",
			setup: func(w *releaseWorld) uuid.UUID {
				foreign := w.foreignJob("foreign", "marketing")
				id := w.heldHold("foreign.ds")
				w.declare(foreign, "foreign.ds")
				require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).Update("held_by_job_id", foreign).Error)
				return id
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "hold another job opened on a dataset this job now produces",
			setup: func(w *releaseWorld) uuid.UUID {
				id := w.heldHold("inherited.ds")
				w.declare(w.jobID, "inherited.ds")
				require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).
					Update("held_by_job_id", w.foreignJob("old-owner", "finance")).Error)
				return id
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "dataset the job no longer declares",
			setup: func(w *releaseWorld) uuid.UUID {
				require.NoError(w.t, w.db.Where("job_id = ?", w.jobID).Delete(&models.DatasetDeclaration{}).Error)
				return w.holdID
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "dataset whose producer changed",
			setup: func(w *releaseWorld) uuid.UUID {
				require.NoError(w.t, w.db.Where("job_id = ?", w.jobID).Delete(&models.DatasetDeclaration{}).Error)
				w.declare(w.foreignJob("new-owner", "finance"), releaseDataset)
				return w.holdID
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "co-produced from another namespace",
			setup: func(w *releaseWorld) uuid.UUID {
				w.declare(w.foreignJob("co-producer", "marketing"), releaseDataset)
				return w.holdID
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "job soft-deleted",
			setup: func(w *releaseWorld) uuid.UUID {
				require.NoError(w.t, w.db.Where("id = ?", w.jobID).Delete(&models.Job{}).Error)
				return w.holdID
			},
			want: ErrCrossBoundaryTarget,
		},
		{
			name: "job moved out of the incident's namespace",
			setup: func(w *releaseWorld) uuid.UUID {
				require.NoError(w.t, w.db.Model(&models.Job{}).Where("id = ?", w.jobID).Update("namespace", "marketing").Error)
				return w.holdID
			},
			want: ErrNamespaceMoved,
		},
		{
			name: "hold opened by a run in another namespace",
			setup: func(w *releaseWorld) uuid.UUID {
				id := w.heldHold("older.ds")
				w.declare(w.jobID, "older.ds")
				other, _ := w.greenRun("marketing")
				require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).Update("held_by_run_id", other).Error)
				return id
			},
			want: ErrNamespaceMoved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/dispatch", func(t *testing.T) {
			w := newReleaseWorld(t, "finance")
			target := tc.setup(w)
			action, err := w.act(ActionParams{HoldID: &target, Reason: "x"})
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, models.AgentActionStatusFailed, action.Status)
			require.Empty(t, w.ops.releaseHold)
		})
		t.Run(tc.name+"/proposal", func(t *testing.T) {
			w := newReleaseWorld(t, "finance")
			target := tc.setup(w)
			action, err := w.act(ActionParams{HoldID: &target, Reason: "x", Tolerate: map[string]string{"min": "24h"}})
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, models.AgentActionStatusFailed, action.Status)
			var approvals int64
			require.NoError(t, w.db.Model(&models.ApprovalRequest{}).Count(&approvals).Error)
			require.Zero(t, approvals, "a refused proposal must not reach a human")
		})
	}

	t.Run("implicit target is checked like a named one", func(t *testing.T) {
		w := newReleaseWorld(t, "finance")
		require.NoError(t, w.db.Where("job_id = ?", w.jobID).Delete(&models.DatasetDeclaration{}).Error)
		action, err := w.act(ActionParams{Reason: "x"})
		require.ErrorIs(t, err, ErrCrossBoundaryTarget)
		require.Equal(t, models.AgentActionStatusFailed, action.Status)
		require.Empty(t, w.ops.releaseHold)
	})
}

func TestReleaseHoldRefusalDoesNotRevealWhetherAHoldExists(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	foreign := w.foreignJob("foreign", "marketing")
	held := w.heldHold("foreign.ds")
	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", held).Update("held_by_job_id", foreign).Error)
	missing := uuid.New()

	_, heldErr := w.act(ActionParams{HoldID: &held, Reason: "x"})
	_, missingErr := w.act(ActionParams{HoldID: &missing, Reason: "x"})
	require.Equal(t,
		strings.ReplaceAll(heldErr.Error(), held.String(), "<id>"),
		strings.ReplaceAll(missingErr.Error(), missing.String(), "<id>"))
	require.NotContains(t, heldErr.Error(), foreign.String(), "a refusal must not name the foreign owner")
}

// The boundary is checked again inside the release transaction: a producer that
// changes after dispatch read it must stop the release.
func TestReleaseHoldGuardCatchesAProducerChangeAfterTheCheck(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	w.ops.releaseDB = nil
	_, err := w.act(ActionParams{Reason: "x"})
	require.NoError(t, err)
	guard := w.ops.releaseHold[0].Guard
	require.NoError(t, guard(w.db))

	require.NoError(t, w.db.Where("job_id = ?", w.jobID).Delete(&models.DatasetDeclaration{}).Error)
	require.ErrorIs(t, guard(w.db), ErrCrossBoundaryTarget)

	w.declare(w.jobID, releaseDataset)
	require.NoError(t, guard(w.db))
	require.NoError(t, w.db.Model(&models.Job{}).Where("id = ?", w.jobID).Update("namespace", "marketing").Error)
	require.ErrorIs(t, guard(w.db), ErrNamespaceMoved)
}

func TestReleaseHoldRequiresAReason(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.act(ActionParams{Reason: "  "})
	require.ErrorIs(t, err, ErrReleaseReasonRequired)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
	require.Empty(t, w.ops.releaseHold)
}

func TestReleaseHoldToleranceWindows(t *testing.T) {
	for name, tolerate := range map[string]map[string]string{
		"not a duration":             {"min": "1 day"},
		"zero":                       {"min": "0s"},
		"negative":                   {"min": "-1h"},
		"over the bound":             {"min": "721h"},
		"assertion it did not break": {"maxLag": "1h"},
		"not an assertion":           {"rowCount": "1h"},
		"blank key":                  {" ": "1h"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newReleaseWorld(t, "finance")
			action, err := w.act(ActionParams{Reason: "x", Tolerate: tolerate})
			require.ErrorIs(t, err, ErrInvalidTolerance)
			require.Equal(t, models.AgentActionStatusFailed, action.Status)
			var approvals int64
			require.NoError(t, w.db.Model(&models.ApprovalRequest{}).Count(&approvals).Error)
			require.Zero(t, approvals)
		})
	}

	t.Run("a hold with no readable verdicts accepts none", func(t *testing.T) {
		w := newReleaseWorld(t, "finance")
		require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", w.holdID).Update("violations", nil).Error)
		_, err := w.act(ActionParams{Reason: "x", Tolerate: map[string]string{"min": "1h"}})
		require.ErrorIs(t, err, ErrInvalidTolerance)
	})
}

// A tolerance ends at a human even for an agent the playbook lets release alone,
// the approved action runs once, and the decider is the one recorded as releasing.
func TestReleaseHoldWithToleranceTerminatesAtAHuman(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.act(ActionParams{Reason: "month end", Tolerate: map[string]string{" min ": " 720h "}})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusProposed, action.Status)
	require.Equal(t, TierApproval, action.Tier)
	require.Empty(t, w.ops.releaseHold, "nothing runs before a decision")

	var approval models.ApprovalRequest
	require.NoError(t, w.db.First(&approval, "action_id = ?", action.ID).Error)
	require.Equal(t, models.ApprovalDecisionPending, approval.Decision)
	require.Equal(t, models.IncidentStatusAwaitingApproval, w.only().Status)

	executed, err := w.approve(action.ID, "user:ada")
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	require.Len(t, w.ops.releaseHold, 1)
	got := w.ops.releaseHold[0]
	require.Equal(t, "user:ada", got.Principal)
	require.Equal(t, map[string]string{"min": "720h"}, got.Tolerate)
	result := actionResult(t, executed)
	require.Equal(t, "user:ada", result["approved_by"])
	require.Contains(t, result["caveat"], "not enforced", "the record must say a window suppresses nothing yet")

	_, err = w.exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrActionNotApproved)
	require.Len(t, w.ops.releaseHold, 1, "a replay must not release again")
}

// The row keeps the latest breach's verdicts, so a later breach of another
// assertion must not turn a decision a human already made into a failure.
func TestReleaseHoldApprovedToleranceSurvivesALaterBreach(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.act(ActionParams{Reason: "month end", Tolerate: map[string]string{"min": "24h"}})
	require.NoError(t, err)
	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", w.holdID).
		Update("violations", datatypes.JSON(`[{"metric":"rowCount","assertion":"max"}]`)).Error)

	executed, err := w.approve(action.ID, "user:ada")
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	require.Equal(t, map[string]string{"min": "24h"}, w.ops.releaseHold[0].Tolerate)
}

func TestReleaseHoldOfAnEndedHoldIsDone(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", w.holdID).Updates(map[string]any{
		"status": models.DatasetHoldStatusReleased, "active_key": nil,
		"released_by": "user:bob", "release_reason": models.DatasetHoldReleaseManualAck,
	}).Error)

	t.Run("at dispatch", func(t *testing.T) {
		action, err := w.act(ActionParams{Reason: "x"})
		require.NoError(t, err)
		require.Equal(t, models.AgentActionStatusExecuted, action.Status)
		result := actionResult(t, action)
		require.Equal(t, true, result["already_released"])
		require.Equal(t, "user:bob", result["released_by"])
	})
	t.Run("at the proposal, so no human is asked", func(t *testing.T) {
		action, err := w.act(ActionParams{Tolerate: map[string]string{"min": "1h"}})
		require.NoError(t, err)
		require.Equal(t, models.AgentActionStatusExecuted, action.Status)
		require.Equal(t, true, actionResult(t, action)["already_released"])
		var approvals int64
		require.NoError(t, w.db.Model(&models.ApprovalRequest{}).Count(&approvals).Error)
		require.Zero(t, approvals)
	})
	require.Empty(t, w.ops.releaseHold)
}

// A release that loses to another between the check and the write.
func TestReleaseHoldLosingARaceSucceedsWithoutReleasing(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.act(ActionParams{Tolerate: map[string]string{"min": "1h"}, Reason: "x"})
	require.NoError(t, err)

	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", w.holdID).Updates(map[string]any{
		"released_by": "user:bob", "release_reason": models.DatasetHoldReleaseManualAck,
	}).Error)
	w.ops.releaseHoldErr = ErrHoldNotActive

	executed, err := w.approve(action.ID, "user:ada")
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	result := actionResult(t, executed)
	require.Equal(t, true, result["already_released"])
	require.Equal(t, "user:bob", result["released_by"], "the release that won is the one reported")
}

func TestReleaseHoldApprovedAfterAConcurrentReleaseIsDone(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	action, err := w.act(ActionParams{Tolerate: map[string]string{"min": "1h"}, Reason: "x"})
	require.NoError(t, err)

	w.release(context.Background(), w.holdID)
	require.Equal(t, models.IncidentStatusClosed, w.only().Status, "the release closes the incident under the pending approval")

	executed, err := w.approve(action.ID, "user:ada")
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	require.Equal(t, true, actionResult(t, executed)["already_released"])
	require.Empty(t, w.ops.releaseHold)
	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

// A release that fails leaves the action failed, the incident where it was and the
// hold active.
func TestReleaseHoldFailureLeavesTheIncidentOpen(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	w.ops.releaseHoldErr = errors.New("boom")
	action, err := w.act(ActionParams{Reason: "x"})
	require.Error(t, err)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
	require.Equal(t, models.IncidentStatusOpen, w.only().Status)
	require.Equal(t, models.DatasetHoldStatusActive, w.holdStatus(w.holdID))
}

func TestReleaseHoldWithoutOpsFails(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	exec := NewExecutor(w.store, nil)
	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: w.inc.ID, Type: ActionTypeReleaseHold, Params: ActionParams{Reason: "x"}, Playbook: releaseAllowed(),
	})
	require.ErrorIs(t, err, errNoOps)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
}

// markReleased is what the release transaction leaves behind.
func (w *releaseWorld) markReleased(by string) {
	w.t.Helper()
	require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", w.holdID).Updates(map[string]any{
		"status": models.DatasetHoldStatusReleased, "active_key": nil,
		"released_by": by, "release_reason": models.DatasetHoldReleaseManualAck,
	}).Error)
}

// TestReleaseHoldIncidentOutcome pins which outcome wins. The executor writes no
// incident status, so an executed release leaves the incident to the release's two
// consumers. Each close passes through the status machine under the row lock, and
// from any live status the incident ends closed with the release's summary.
func TestReleaseHoldIncidentOutcome(t *testing.T) {
	statuses := []models.IncidentStatus{
		models.IncidentStatusOpen,
		models.IncidentStatusTriaging,
		models.IncidentStatusAwaitingApproval,
		models.IncidentStatusEscalated,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			w := newReleaseWorld(t, "finance")
			require.NoError(t, w.db.Model(&models.Incident{}).Where("id = ?", w.inc.ID).Update("status", status).Error)

			_, err := w.act(ActionParams{Reason: "x"})
			require.NoError(t, err)
			require.Equal(t, status, w.only().Status, "the executor must not move the incident")

			w.markReleased("user:ada")
			payload, err := json.Marshal(map[string]any{
				"hold_id": w.holdID, "dataset": releaseDataset, "status": "released",
				"release_reason": models.DatasetHoldReleaseManualAck, "released_by": "user:ada",
			})
			require.NoError(t, err)
			released := event.Event{Type: event.TypeDatasetReleased, JobID: w.jobID, Payload: payload}

			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				assert.NoError(t, w.sub.ReconcileHolds(context.Background(), time.Hour))
			}()
			for range 2 {
				go func() {
					defer wg.Done()
					w.sub.handle(context.Background(), released)
				}()
			}
			wg.Wait()

			got := w.only()
			require.Equal(t, models.IncidentStatusClosed, got.Status)
			require.Nil(t, got.ActiveDedupeKey)
			require.Equal(t, holdReleaseSummary(models.DatasetHoldReleaseManualAck, "user:ada"), got.ResolutionSummary)
		})
	}
}

// The sweep alone reaches the same end, and an action finishing after the close
// changes nothing about it.
func TestReleaseHoldSweepClosesWhenTheEventIsLost(t *testing.T) {
	w := newReleaseWorld(t, "finance")
	w.markReleased("user:ada")
	require.NoError(t, w.sub.ReconcileHolds(context.Background(), time.Hour))
	require.Equal(t, models.IncidentStatusClosed, w.only().Status)

	action, err := w.act(ActionParams{Reason: "x"})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)
	require.Len(t, w.incidents(), 1, "the sweep must not open a second incident for the hold")
}
