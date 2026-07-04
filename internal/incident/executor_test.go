package incident

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	mtest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeOps records calls to the ActionOps surface so tests can assert dispatch
// without wiring the real run/callback/notification/replay subsystems.
type fakeOps struct {
	retryFromFailure []uuid.UUID
	retryCallbacks   []uuid.UUID
	rerun            []rerunCall
	notify           []notifyCall
	escalate         []escalateCall
	setPaused        []pauseCall
	clearCache       []cacheCall
	suppress         []time.Time
	extendSLA        []extendCall
	replay           []uuid.UUID

	retryErr error
	rerunID  uuid.UUID
}

type rerunCall struct {
	jobID  uuid.UUID
	params map[string]string
}
type notifyCall struct{ channel, message string }
type escalateCall struct {
	incidentID       uuid.UUID
	channel, summary string
}
type pauseCall struct {
	jobID  uuid.UUID
	paused bool
}
type cacheCall struct {
	jobID    uuid.UUID
	taskName string
}
type extendCall struct {
	runID  uuid.UUID
	extend time.Duration
}

func (f *fakeOps) RetryFromFailure(_ context.Context, runID uuid.UUID) error {
	f.retryFromFailure = append(f.retryFromFailure, runID)
	return f.retryErr
}
func (f *fakeOps) RetryCallbacks(_ context.Context, runID uuid.UUID) error {
	f.retryCallbacks = append(f.retryCallbacks, runID)
	return nil
}
func (f *fakeOps) RerunWithParams(_ context.Context, jobID uuid.UUID, params map[string]string) (uuid.UUID, error) {
	f.rerun = append(f.rerun, rerunCall{jobID: jobID, params: params})
	if f.rerunID == uuid.Nil {
		f.rerunID = uuid.New()
	}
	return f.rerunID, nil
}
func (f *fakeOps) QuarantineReplay(_ context.Context, runID uuid.UUID, _ map[string]string) (json.RawMessage, error) {
	f.replay = append(f.replay, runID)
	return json.RawMessage(`{"ok":true}`), nil
}
func (f *fakeOps) Notify(_ context.Context, channel, message string) error {
	f.notify = append(f.notify, notifyCall{channel: channel, message: message})
	return nil
}
func (f *fakeOps) Escalate(_ context.Context, incidentID uuid.UUID, channel, summary string) error {
	f.escalate = append(f.escalate, escalateCall{incidentID: incidentID, channel: channel, summary: summary})
	return nil
}
func (f *fakeOps) SetJobPaused(_ context.Context, jobID uuid.UUID, paused bool) error {
	f.setPaused = append(f.setPaused, pauseCall{jobID: jobID, paused: paused})
	return nil
}
func (f *fakeOps) ClearCacheEntry(_ context.Context, jobID uuid.UUID, taskName string) error {
	f.clearCache = append(f.clearCache, cacheCall{jobID: jobID, taskName: taskName})
	return nil
}
func (f *fakeOps) SuppressDownstreamAlerts(_ context.Context, _ uuid.UUID, until time.Time) error {
	f.suppress = append(f.suppress, until)
	return nil
}
func (f *fakeOps) ExtendSLAOnce(_ context.Context, runID uuid.UUID, extend time.Duration) error {
	f.extendSLA = append(f.extendSLA, extendCall{runID: runID, extend: extend})
	return nil
}

// seedIncident opens an incident with a remediation-target run so run-scoped
// actions can resolve their run id from the incident.
func seedIncident(t *testing.T, store *Store) (*models.Incident, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	inc, outcome, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  uuid.New(),
		RunID:                  &runID,
		TaskName:               "extract",
		Class:                  ClassTransientInfra,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	return inc, runID
}

func newExecutorTest(t *testing.T) (*gorm.DB, *Store, *fakeOps, *Executor) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ops := &fakeOps{}
	return db, store, ops, NewExecutor(store, ops)
}

func TestExecuteTier1AutonomousRetry(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, runID := seedIncident(t, store)

	before := mtest.CounterValue(t, metrics.AgentActionsTotal, ActionTypeRetryFromFailure, "1", string(models.AgentActionActorAgent))

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Actor:      models.AgentActionActorAgent,
		Type:       ActionTypeRetryFromFailure,
		Playbook:   Playbook{},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)
	require.Equal(t, models.AgentActionActorAgent, action.Actor)
	require.Equal(t, TierAutonomous, action.Tier)
	require.Equal(t, []uuid.UUID{runID}, ops.retryFromFailure)

	after := mtest.CounterValue(t, metrics.AgentActionsTotal, ActionTypeRetryFromFailure, "1", string(models.AgentActionActorAgent))
	require.Equal(t, before+1, after)

	// Tier 1 is NOT mirrored into the audit log.
	var audits int64
	require.NoError(t, store.DB().Model(&models.AuditLog{}).Count(&audits).Error)
	require.Zero(t, audits)
}

func TestExecuteTier2RequiresExplicitAllow(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	// Tier-2 pause_job with an empty allow list is denied (recorded rejected).
	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypePauseJob,
		Playbook:   Playbook{},
	})
	require.ErrorIs(t, err, ErrActionNotPermitted)
	require.Equal(t, models.AgentActionStatusRejected, action.Status)
	require.Empty(t, ops.setPaused)

	// Explicitly allowed → executes and mirrors into the audit log (tier 2).
	action2, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypePauseJob,
		Playbook:   Playbook{Allow: map[string]bool{ActionTypePauseJob: true}},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action2.Status)
	require.Len(t, ops.setPaused, 1)
	require.True(t, ops.setPaused[0].paused)
	require.Equal(t, inc.JobID, ops.setPaused[0].jobID)

	var audits int64
	require.NoError(t, store.DB().Model(&models.AuditLog{}).
		Where("action = ?", "agent.action."+ActionTypePauseJob).Count(&audits).Error)
	require.Equal(t, int64(1), audits, "tier-2 execution must mirror into the audit log")
}

func TestExecuteTier3AlwaysProposedNeverExecuted(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	// Even with the action explicitly allowed, tier 3 is never auto-executed.
	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Playbook:   Playbook{Allow: map[string]bool{ActionTypeApplyJobdefPatch: true}},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusProposed, action.Status)
	require.Equal(t, TierApproval, action.Tier)
	require.Empty(t, ops.setPaused)
	require.Empty(t, ops.retryFromFailure)
}

func TestExecuteRequireApprovalRoutesTier1ToProposed(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeRetryFromFailure,
		Playbook:   Playbook{RequireApproval: map[string]bool{ActionTypeRetryFromFailure: true}},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusProposed, action.Status)
	require.Empty(t, ops.retryFromFailure, "an approval-gated action must not execute")
}

func TestExecuteUnknownAction(t *testing.T) {
	_, store, _, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	_, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       "definitely_not_a_real_action",
	})
	require.ErrorIs(t, err, ErrUnknownAction)
}

func TestExecuteRerunWithParamsWhitelist(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	pb := Playbook{
		Allow:          map[string]bool{ActionTypeRerunWithParams: true},
		ParamOverrides: map[string][]string{"badRowPolicy": {"quarantine"}},
	}

	// A non-whitelisted value is rejected (recorded failed).
	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeRerunWithParams,
		Params:     ActionParams{Overrides: map[string]string{"badRowPolicy": "drop"}},
		Playbook:   pb,
	})
	require.Error(t, err)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
	require.Empty(t, ops.rerun)

	// A whitelisted value runs.
	action2, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeRerunWithParams,
		Params:     ActionParams{Overrides: map[string]string{"badRowPolicy": "quarantine"}},
		Playbook:   pb,
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action2.Status)
	require.Len(t, ops.rerun, 1)
	require.Equal(t, "quarantine", ops.rerun[0].params["badRowPolicy"])
}

func TestExecutePolicyDeterministicRuleRecordsActorPolicy(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	// transient_infra → auto_retry_backoff (retry_from_failure) deterministic rule.
	inc, runID := seedIncident(t, store)

	action, matched, err := exec.ApplyDeterministicRule(context.Background(), inc, DefaultRuleSet())
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, models.AgentActionActorPolicy, action.Actor)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)
	require.Equal(t, ActionTypeRetryFromFailure, action.Type)
	require.Equal(t, []uuid.UUID{runID}, ops.retryFromFailure)
}

func TestApplyDeterministicRuleNoMatch(t *testing.T) {
	_, store, _, exec := newExecutorTest(t)
	// auth_failure has no default deterministic rule → agent path.
	runID := uuid.New()
	inc, _, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  uuid.New(),
		RunID:                  &runID,
		TaskName:               "extract",
		Class:                  ClassAuthFailure,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)

	action, matched, err := exec.ApplyDeterministicRule(context.Background(), inc, DefaultRuleSet())
	require.NoError(t, err)
	require.False(t, matched)
	require.Nil(t, action)
}

func TestSnoozeRetrySchedulesDurableTimerAndFires(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	// Use a data_unavailable incident so a snooze is the natural remediation.
	runID := uuid.New()
	inc, _, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  uuid.New(),
		RunID:                  &runID,
		TaskName:               "extract",
		Class:                  ClassDataUnavailable,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSnoozeRetry,
		Params:     ActionParams{DelaySeconds: 2700},
		Playbook:   Playbook{},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)

	// A durable timer must exist, pending, owned by the incident, linked to the
	// action — no retry has fired yet.
	var timer models.RemediationTimer
	require.NoError(t, db.First(&timer, "incident_id = ?", inc.ID).Error)
	require.Equal(t, models.RemediationTimerStatusPending, timer.Status)
	require.Equal(t, TimerKindSnoozeRetry, timer.Kind)
	require.NotNil(t, timer.ActionID)
	require.Equal(t, action.ID, *timer.ActionID)
	require.Empty(t, ops.retryFromFailure)

	// Backdate the timer so it is due, then sweep: the handler must fire the
	// admit-aware retry for the snoozed run.
	require.NoError(t, db.Model(&models.RemediationTimer{}).
		Where("id = ?", timer.ID).
		Update("fire_at", time.Now().UTC().Add(-time.Minute)).Error)

	sup := NewTimerSupervisor(db, nil, time.Second)
	exec.RegisterTimerHandlers(sup)
	require.NoError(t, sup.SweepOnce(context.Background()))

	require.Equal(t, []uuid.UUID{runID}, ops.retryFromFailure)

	var fired models.RemediationTimer
	require.NoError(t, db.First(&fired, "id = ?", timer.ID).Error)
	require.Equal(t, models.RemediationTimerStatusFired, fired.Status)
}

func TestSnoozeTimerCancelledOnTerminalIncidentIsNotFired(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	runID := uuid.New()
	inc, _, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  uuid.New(),
		RunID:                  &runID,
		TaskName:               "extract",
		Class:                  ClassDataUnavailable,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)

	_, err = exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSnoozeRetry,
		Params:     ActionParams{DelaySeconds: 2700},
	})
	require.NoError(t, err)

	// The incident is abandoned (budget exhausted); this terminal transition must
	// cancel the timer so it never fires a retry against a closed incident.
	_, err = store.Transition(context.Background(), inc.ID, models.IncidentStatusAbandoned, "budget exhausted")
	require.NoError(t, err)

	var timer models.RemediationTimer
	require.NoError(t, db.First(&timer, "incident_id = ?", inc.ID).Error)
	require.Equal(t, models.RemediationTimerStatusCancelled, timer.Status)

	// Even backdated, a cancelled timer is not swept.
	require.NoError(t, db.Model(&models.RemediationTimer{}).
		Where("id = ?", timer.ID).
		Update("fire_at", time.Now().UTC().Add(-time.Minute)).Error)
	sup := NewTimerSupervisor(db, nil, time.Second)
	exec.RegisterTimerHandlers(sup)
	require.NoError(t, sup.SweepOnce(context.Background()))
	require.Empty(t, ops.retryFromFailure)
}

func TestSnoozeRetryFireHandlerErrPropagates(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	ops.retryErr = errors.New("boom")
	runID := uuid.New()
	timer := models.RemediationTimer{
		Payload: mustJSON(t, snoozePayload{RunID: runID}),
	}
	err := exec.fireSnoozeRetry(context.Background(), timer)
	require.Error(t, err)
	_ = store
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
