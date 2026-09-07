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
	// escalateRouted is what the fake reports for "a notification policy carried
	// this"; false by default so a test must opt in to claiming delivery.
	escalateRouted bool
	setPaused      []pauseCall
	clearCache     []cacheCall
	suppress       []time.Time
	extendSLA      []extendCall
	replay         []uuid.UUID
	skipTask       []skipTaskCall
	overrideGate   []uuid.UUID
	applyPatch     []applyPatchCall

	retryErr error
	rerunID  uuid.UUID
	// applyPatchErr forces the jobdef apply to fail so the approved-action
	// failure path is exercised.
	applyPatchErr error
}

type skipTaskCall struct {
	runID  uuid.UUID
	taskID uuid.UUID
	reason string
}

type applyPatchCall struct {
	jobID      uuid.UUID
	definition json.RawMessage
	dryRun     bool
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
func (f *fakeOps) Escalate(_ context.Context, incidentID uuid.UUID, channel, summary string) (bool, error) {
	f.escalate = append(f.escalate, escalateCall{incidentID: incidentID, channel: channel, summary: summary})
	return f.escalateRouted, nil
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
func (f *fakeOps) SkipTask(_ context.Context, runID, taskID uuid.UUID, reason string) error {
	f.skipTask = append(f.skipTask, skipTaskCall{runID: runID, taskID: taskID, reason: reason})
	return nil
}
func (f *fakeOps) OverrideSchemaGateOnce(_ context.Context, runID uuid.UUID) error {
	f.overrideGate = append(f.overrideGate, runID)
	return nil
}
func (f *fakeOps) ApplyJobdefPatch(_ context.Context, jobID uuid.UUID, definition json.RawMessage, dryRun bool) (json.RawMessage, error) {
	f.applyPatch = append(f.applyPatch, applyPatchCall{jobID: jobID, definition: definition, dryRun: dryRun})
	if f.applyPatchErr != nil {
		return nil, f.applyPatchErr
	}
	return json.RawMessage(`{"alias":"demo","empty":false}`), nil
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

func pendingTimers(t *testing.T, db *gorm.DB, incidentID uuid.UUID) []models.RemediationTimer {
	t.Helper()
	var timers []models.RemediationTimer
	require.NoError(t, db.Where("incident_id = ? AND status = ?", incidentID, models.RemediationTimerStatusPending).Find(&timers).Error)
	return timers
}

func decodeSnoozePayload(t *testing.T, raw []byte) snoozePayload {
	t.Helper()
	var p snoozePayload
	require.NoError(t, json.Unmarshal(raw, &p))
	return p
}

// A snooze_retry whose job is paused (retryable refusal) must NOT consume the
// timer and drop the retry: it re-arms a fresh pending timer, and once the job
// is unpaused the re-armed timer's fire retries successfully.
func TestSnoozeRetryRearmsOnDeferredThenSucceeds(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	inc, runID := seedIncident(t, store)

	// Simulate a paused job: the retry is deferred, not permanently failed.
	ops.retryErr = ErrRetryDeferred

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSnoozeRetry,
		Params:     ActionParams{DelaySeconds: 2700},
		Playbook:   Playbook{},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)

	pend := pendingTimers(t, db, inc.ID)
	require.Len(t, pend, 1)
	original := pend[0]

	// Backdate + sweep: the deferred retry must re-arm a NEW pending timer, not
	// vanish.
	require.NoError(t, db.Model(&models.RemediationTimer{}).Where("id = ?", original.ID).
		Update("fire_at", time.Now().UTC().Add(-time.Minute)).Error)
	sup := NewTimerSupervisor(db, nil, time.Second)
	exec.RegisterTimerHandlers(sup)
	require.NoError(t, sup.SweepOnce(context.Background()))

	// The original attempt was made (and deferred) — the run is NOT yet retried.
	require.Equal(t, []uuid.UUID{runID}, ops.retryFromFailure)

	// The original timer is consumed (fired) but a fresh pending timer re-armed
	// for the same run, rearm=1.
	var originalAfter models.RemediationTimer
	require.NoError(t, db.First(&originalAfter, "id = ?", original.ID).Error)
	require.Equal(t, models.RemediationTimerStatusFired, originalAfter.Status)

	rearmed := pendingTimers(t, db, inc.ID)
	require.Len(t, rearmed, 1, "a retryable refusal must leave exactly one pending re-armed timer")
	require.NotEqual(t, original.ID, rearmed[0].ID)
	rp := decodeSnoozePayload(t, rearmed[0].Payload)
	require.Equal(t, runID, rp.RunID)
	require.Equal(t, 1, rp.Rearm)

	// The re-arm is observable on the audit spine as a policy snooze_retry row
	// (distinct from the original agent-scheduled snooze action).
	var rearmActions int64
	require.NoError(t, db.Model(&models.AgentAction{}).
		Where("incident_id = ? AND type = ? AND actor = ? AND status = ?",
			inc.ID, ActionTypeSnoozeRetry, models.AgentActionActorPolicy, models.AgentActionStatusExecuted).
		Count(&rearmActions).Error)
	require.Equal(t, int64(1), rearmActions, "the re-arm must be recorded on the timeline")

	// Job unpaused: the re-armed timer's fire now retries successfully.
	ops.retryErr = nil
	require.NoError(t, db.Model(&models.RemediationTimer{}).Where("id = ?", rearmed[0].ID).
		Update("fire_at", time.Now().UTC().Add(-time.Minute)).Error)
	require.NoError(t, sup.SweepOnce(context.Background()))

	require.Equal(t, []uuid.UUID{runID, runID}, ops.retryFromFailure, "the re-armed timer retried the run")
	require.Empty(t, pendingTimers(t, db, inc.ID), "no timers left pending after a successful retry")
}

// Beyond the re-arm ceiling a permanently-deferred snooze gives up: no new timer,
// a failed audit row, and an error surfaced to the sweeper.
func TestSnoozeRetryRearmGivesUpAtCeiling(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	inc, runID := seedIncident(t, store)
	ops.retryErr = ErrRetryDeferred

	timer := models.RemediationTimer{
		IncidentID: inc.ID,
		Namespace:  inc.Namespace,
		Payload:    mustJSON(t, snoozePayload{RunID: runID, Rearm: maxSnoozeRearm}),
	}
	err := exec.fireSnoozeRetry(context.Background(), timer)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRetryDeferred)

	require.Empty(t, pendingTimers(t, db, inc.ID), "at the ceiling no further timer is armed")

	var gaveUp int64
	require.NoError(t, db.Model(&models.AgentAction{}).
		Where("incident_id = ? AND type = ? AND status = ?", inc.ID, ActionTypeSnoozeRetry, models.AgentActionStatusFailed).
		Count(&gaveUp).Error)
	require.Equal(t, int64(1), gaveUp)
}

// --- Playbook resolution semantics (review follow-up, PR #390) --------------

// TestPlaybookNilVsEmptyAllow pins the distinction the whole policy model rests
// on. Conflating them is how the shipped "zero risk" triage-only profile granted
// every tier-1 action, and how combining two playbooks silently widened tier 2.
func TestPlaybookNilVsEmptyAllow(t *testing.T) {
	t.Run("nil allow is unconfigured: tier defaults apply", func(t *testing.T) {
		pb := Playbook{}
		require.Nil(t, pb.Allow)
		require.Equal(t, decisionExecute, pb.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"tier 1 defaults autonomous when no allowlist is configured")
		require.Equal(t, decisionDeny, pb.decide(ActionTypePauseJob, TierGated),
			"tier 2 still needs an explicit allow")
		require.Equal(t, decisionApprove, pb.decide(ActionTypeSkipTask, TierApproval))
	})

	t.Run("empty allow is configured: it grants nothing", func(t *testing.T) {
		pb := Playbook{Allow: map[string]bool{}}
		require.Equal(t, decisionDeny, pb.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"`allow: []` says allow nothing — including tier 1")
		require.Equal(t, decisionDeny, pb.decide(ActionTypePauseJob, TierGated))
	})

	t.Run("decoder preserves the distinction", func(t *testing.T) {
		require.Nil(t, DecodePlaybook([]byte(`{"autonomy":{}}`)).Allow,
			"an absent allow key is unconfigured")
		configured := DecodePlaybook([]byte(`{"autonomy":{"allow":[]}}`))
		require.NotNil(t, configured.Allow, "a present `allow: []` is configured")
		require.Empty(t, configured.Allow)
		require.Equal(t, decisionDeny, configured.decide(ActionTypeEscalate, TierAutonomous))
	})

	t.Run("a configured allowlist governs at every tier below approval", func(t *testing.T) {
		pb := DecodePlaybook([]byte(`{"autonomy":{"allow":["pause_job"]}}`))
		require.Equal(t, decisionExecute, pb.decide(ActionTypePauseJob, TierGated),
			"a tier-2 action is autonomous when explicitly allowed")
		require.Equal(t, decisionDeny, pb.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"a configured allowlist that omits a tier-1 action denies it")
	})
}

// TestPlaybookOverride covers resolving a job's authored autonomy block over its
// profile's defaults. The job block may GRANT as well as narrow — that is the
// design's `metadata.remediation` overriding profile defaults — and is safe only
// because an agent cannot edit it (ErrPatchAltersRemediation).
func TestPlaybookOverride(t *testing.T) {
	t.Run("a nil job allow inherits the profile", func(t *testing.T) {
		profile := Playbook{Allow: map[string]bool{ActionTypeRetryFromFailure: true}}
		got := profile.Override(Playbook{})
		require.Equal(t, profile.Allow, got.Allow)
	})

	t.Run("a configured job allow replaces the profile's", func(t *testing.T) {
		profile := Playbook{Allow: map[string]bool{ActionTypeRetryFromFailure: true}}
		got := profile.Override(Playbook{Allow: map[string]bool{ActionTypePauseJob: true}})
		require.Equal(t, decisionExecute, got.decide(ActionTypePauseJob, TierGated))
		require.Equal(t, decisionDeny, got.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"an authored job policy replaces, so what it omits is not allowed")
	})

	t.Run("an unconfigured profile plus a job allow grants at tier 2", func(t *testing.T) {
		// The case the old Narrow got wrong: an empty base read as "unconstrained"
		// and was replaced by the job list, which WIDENED tier 2 relative to the
		// base's own verdict. Under Override this is intended and explicit.
		base := Playbook{}
		require.Equal(t, decisionDeny, base.decide(ActionTypePauseJob, TierGated))

		got := base.Override(Playbook{Allow: map[string]bool{ActionTypePauseJob: true}})
		require.Equal(t, decisionExecute, got.decide(ActionTypePauseJob, TierGated),
			"a job that authors pause_job into its allowlist grants it deliberately")
	})

	t.Run("a job allow of [] revokes everything the profile granted", func(t *testing.T) {
		profile := Playbook{Allow: map[string]bool{ActionTypeRetryFromFailure: true}}
		got := profile.Override(Playbook{Allow: map[string]bool{}})
		require.Equal(t, decisionDeny, got.decide(ActionTypeRetryFromFailure, TierAutonomous))
	})

	t.Run("require-approval is a union: the stricter side wins", func(t *testing.T) {
		profile := Playbook{RequireApproval: map[string]bool{ActionTypeRetryFromFailure: true}}
		got := profile.Override(Playbook{RequireApproval: map[string]bool{ActionTypePauseJob: true}})
		require.Equal(t, decisionApprove, got.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"a job block may not drop a gate its profile imposes")
		require.Equal(t, decisionApprove, got.decide(ActionTypePauseJob, TierGated))
	})

	t.Run("param overrides replace or inherit", func(t *testing.T) {
		profile := Playbook{ParamOverrides: map[string][]string{"region": {"us-east-1"}}}
		require.Equal(t, profile.ParamOverrides, profile.Override(Playbook{}).ParamOverrides)

		got := profile.Override(Playbook{ParamOverrides: map[string][]string{"tier": {"gold"}}})
		require.NoError(t, validateParamOverrides(map[string]string{"tier": "gold"}, got.ParamOverrides))
		require.Error(t, validateParamOverrides(map[string]string{"region": "us-east-1"}, got.ParamOverrides),
			"a replaced whitelist no longer carries the profile's keys")
	})
}

// TestDenyAllPlaybookPermitsNothing: the fail-closed policy used when a job's
// DECLARED remediation policy cannot be resolved. It must be distinguishable
// from the zero Playbook, which still lets tier 0/1 run autonomously.
func TestDenyAllPlaybookPermitsNothing(t *testing.T) {
	deny := DenyAllPlaybook()
	require.NotNil(t, deny.Allow, "denial is a CONFIGURED empty allowlist, not an unconfigured one")
	require.Equal(t, decisionDeny, deny.decide(ActionTypeRetryFromFailure, TierAutonomous))
	require.Equal(t, decisionDeny, deny.decide(ActionTypePauseJob, TierGated))
	require.Equal(t, decisionApprove, deny.decide(ActionTypeSkipTask, TierApproval),
		"tier 3 still terminates at a human rather than being denied outright")

	require.Equal(t, decisionExecute, Playbook{}.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"the zero Playbook means unconfigured, not denied")
}

// TestPlaybookDocumentRoundTripsForTheBundle: the triage bundle shows the agent
// the resolved policy, so "configured but empty" must not render as "absent".
func TestPlaybookDocumentRoundTripsForTheBundle(t *testing.T) {
	require.JSONEq(t, `{"autonomy":{}}`, string(Playbook{}.Document()),
		"an unconfigured playbook advertises no allowlist")
	require.JSONEq(t, `{"autonomy":{"allow":[]}}`, string(DenyAllPlaybook().Document()),
		"a deny-all playbook must be visibly deny-all, not visibly unconfigured")

	pb := Playbook{
		Allow:           map[string]bool{ActionTypePauseJob: true, ActionTypeEscalate: true},
		RequireApproval: map[string]bool{ActionTypeRetryFromFailure: true},
	}
	require.JSONEq(t,
		`{"autonomy":{"allow":["escalate","pause_job"],"requireApproval":["retry_from_failure"]}}`,
		string(pb.Document()), "keys are sorted so a bundle served twice is byte-identical")

	// The document a resolved playbook renders must decode back to the same policy.
	require.Equal(t, pb, DecodePlaybook(pb.Document()))
}
