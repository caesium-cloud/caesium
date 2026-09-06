package incident

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// enableAuthMode makes the process look like a deployment with an active auth
// mode, which apply_jobdef_patch requires (arc convention 4). t.Setenv restores
// the previous value, and env.Process is re-run at cleanup so a later test in
// this package does not inherit the mutation.
func enableAuthMode(t *testing.T) {
	t.Helper()
	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
}

// seedJobIncident opens an incident that has a real Job row (and optionally a
// named Task row) behind it, which the tier-3 dispatches need: skip_task
// resolves a task name within the incident's job, apply_jobdef_patch reads the
// job's provenance fields.
func seedJobIncident(t *testing.T, db *gorm.DB, store *Store, taskName string, provenance string) (*models.Incident, uuid.UUID, uuid.UUID) {
	t.Helper()

	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:                 jobID,
		Alias:              "approval-" + jobID.String()[:8],
		TriggerID:          triggerID,
		ProvenanceSourceID: provenance,
		CreatedAt:          now,
		UpdatedAt:          now,
	}).Error)

	taskID := uuid.New()
	if taskName != "" {
		require.NoError(t, db.Create(&models.Task{
			ID:        taskID,
			JobID:     jobID,
			Name:      taskName,
			AtomID:    uuid.New(),
			CreatedAt: now,
			UpdatedAt: now,
		}).Error)
	}

	runID := uuid.New()
	inc, outcome, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  jobID,
		RunID:                  &runID,
		TaskName:               taskName,
		Class:                  ClassUnknown,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)
	return inc, runID, taskID
}

// recordingBus captures published events so the tests can assert on the
// approval/execution lifecycle without a live subscriber.
type recordingBus struct{ events []event.Event }

func (b *recordingBus) Publish(e event.Event) { b.events = append(b.events, e) }
func (b *recordingBus) Subscribe(context.Context, event.Filter) (<-chan event.Event, error) {
	return nil, nil
}

func (b *recordingBus) typed(t event.Type) []event.Event {
	var out []event.Event
	for _, e := range b.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// TestTier3ProposalCreatesApprovalRequest is the C4 contract: a tier-3 proposal
// must produce an ApprovalRequest, park the incident in awaiting_approval, and
// announce it — and must NOT execute.
func TestTier3ProposalCreatesApprovalRequest(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	bus := &recordingBus{}
	exec.SetEventSink(bus, event.NewStore(db))
	inc, runID, taskID := seedJobIncident(t, db, store, "extract", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Actor:      models.AgentActionActorAgent,
		Type:       ActionTypeSkipTask,
		Params:     ActionParams{TaskName: "extract", Reason: "vendor file is empty today"},
	})
	require.NoError(t, err)
	require.NotNil(t, action)
	require.Equal(t, models.AgentActionStatusProposed, action.Status)
	require.Equal(t, TierApproval, action.Tier)

	// Nothing executed: the whole point of tier 3.
	require.Empty(t, ops.skipTask, "a tier-3 proposal must not dispatch")

	var approval models.ApprovalRequest
	require.NoError(t, db.Where("action_id = ?", action.ID).First(&approval).Error)
	require.Equal(t, models.ApprovalDecisionPending, approval.Decision)
	require.Equal(t, inc.ID, approval.IncidentID)
	require.NotNil(t, approval.ExpiresAt)

	reloaded, err := store.Get(context.Background(), inc.ID)
	require.NoError(t, err)
	require.Equal(t, models.IncidentStatusAwaitingApproval, reloaded.Status,
		"a pending tier-3 approval must park the incident awaiting_approval")

	requested := bus.typed(event.TypeApprovalRequested)
	require.Len(t, requested, 1, "approval_requested must be published")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(requested[0].Payload, &payload))
	require.Equal(t, approval.ID.String(), payload["approval_id"])
	require.Equal(t, ActionTypeSkipTask, payload["action_type"])

	// The event is PERSISTED, not merely fanned out in memory.
	var persisted int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeApprovalRequested)).Count(&persisted).Error)
	require.EqualValues(t, 1, persisted)

	_ = runID
	_ = taskID
}

// TestExecuteApprovedRunsSkipTask is the C7 contract: an approved action
// dispatches exactly once, records the decider, and refuses a second run.
func TestExecuteApprovedRunsSkipTask(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	bus := &recordingBus{}
	exec.SetEventSink(bus, event.NewStore(db))
	inc, runID, taskID := seedJobIncident(t, db, store, "extract", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSkipTask,
		Params:     ActionParams{TaskName: "extract", Reason: "vendor file is empty today"},
	})
	require.NoError(t, err)

	approveAction(t, db, action.ID, "alice@example.com", "agreed, skip it")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)

	require.Len(t, ops.skipTask, 1)
	require.Equal(t, runID, ops.skipTask[0].runID)
	require.Equal(t, taskID, ops.skipTask[0].taskID)
	require.Equal(t, "vendor file is empty today", ops.skipTask[0].reason)

	var result map[string]any
	require.NoError(t, json.Unmarshal(executed.Result, &result))
	require.Equal(t, "alice@example.com", result["approved_by"])
	require.NotEmpty(t, result["executed_at"])
	require.Contains(t, result, "caveat", "the skip's downstream caveat must be recorded on the row")

	// The audit spine names the human who approved it, not just the proposer.
	var audit models.AuditLog
	require.NoError(t, db.Where("action = ?", "agent.action."+ActionTypeSkipTask).
		Where("outcome = ?", string(models.AgentActionStatusExecuted)).
		First(&audit).Error)
	require.Equal(t, "human:alice@example.com", audit.Actor)

	// The explainability event fired.
	executedEvents := bus.typed(event.TypeAgentActionExecuted)
	require.Len(t, executedEvents, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(executedEvents[0].Payload, &payload))
	require.Equal(t, "alice@example.com", payload["approved_by"])
	require.Equal(t, true, payload["succeeded"])

	// Once-only: the row is no longer `approved`, so a replayed decision is a
	// refusal rather than a second skip.
	_, err = exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrActionNotApproved)
	require.Len(t, ops.skipTask, 1)
}

// TestExecuteApprovedRefusesUnapprovedAction pins the guard that makes the
// approval gate load-bearing: a merely-proposed (or rejected) action can never
// be dispatched, even by a direct call.
func TestExecuteApprovedRefusesUnapprovedAction(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "extract", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSkipTask,
		Params:     ActionParams{TaskName: "extract"},
	})
	require.NoError(t, err)

	_, err = exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrActionNotApproved)
	require.Empty(t, ops.skipTask)

	// A rejected action is equally undispatchable.
	require.NoError(t, db.Model(&models.AgentAction{}).Where("id = ?", action.ID).
		Update("status", models.AgentActionStatusRejected).Error)
	_, err = exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrActionNotApproved)
	require.Empty(t, ops.skipTask)
}

// TestExecuteApprovedOverridesSchemaGate covers the second tier-3 dispatch.
func TestExecuteApprovedOverridesSchemaGate(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	inc, runID, _ := seedJobIncident(t, db, store, "transform", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeOverrideSchemaGate,
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "bob@example.com", "")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	require.Equal(t, []uuid.UUID{runID}, ops.overrideGate)

	var result map[string]any
	require.NoError(t, json.Unmarshal(executed.Result, &result))
	require.Equal(t, "one_run", result["scope"])
}

// TestExecuteApprovedAppliesJobdefPatchDirectly covers the direct half of the
// provenance router: a job with no git provenance is patched in place.
func TestExecuteApprovedAppliesJobdefPatchDirectly(t *testing.T) {
	enableAuthMode(t)
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "transform", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: json.RawMessage(`{"apiVersion":"v1"}`)},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "carol@example.com", "")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)

	require.Len(t, ops.applyPatch, 1)
	require.False(t, ops.applyPatch[0].dryRun, "the direct route applies, it does not merely render")
	require.Empty(t, ops.escalate, "a non-git job must not escalate")

	var result map[string]any
	require.NoError(t, json.Unmarshal(executed.Result, &result))
	require.Equal(t, "direct", result["route"])
	require.Equal(t, true, result["applied"])
}

// TestExecuteApprovedJobdefPatchEscalatesForGitSyncedJob pins the security-
// relevant half of the router: a git-synced job is NEVER patched in the
// database, because the next sync would revert it and leave Git and the DB
// disagreeing. It degrades to escalate WITH the rendered diff.
func TestExecuteApprovedJobdefPatchEscalatesForGitSyncedJob(t *testing.T) {
	enableAuthMode(t)
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "transform", "git-source-1")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: json.RawMessage(`{"apiVersion":"v1"}`)},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "dave@example.com", "")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)

	require.Len(t, ops.applyPatch, 1)
	require.True(t, ops.applyPatch[0].dryRun, "a git-synced job may only RENDER the patch, never apply it")
	require.Len(t, ops.escalate, 1, "the approved patch must escalate with the diff")
	require.Contains(t, ops.escalate[0].summary, "alias")

	var result map[string]any
	require.NoError(t, json.Unmarshal(executed.Result, &result))
	require.Equal(t, "escalate", result["route"])
	require.Equal(t, false, result["applied"])
}

// TestApplyJobdefPatchRefusedWithoutAuthMode is arc convention 4: with no auth
// mode the approve route is an unauthenticated POST the agent container itself
// could call, so the one action that rewrites a job definition is refused —
// with a typed reason recorded on the row, not a panic and not a silent no-op.
func TestApplyJobdefPatchRefusedWithoutAuthMode(t *testing.T) {
	t.Setenv("CAESIUM_AUTH_MODE", "none")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })

	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "transform", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: json.RawMessage(`{"apiVersion":"v1"}`)},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "eve@example.com", "")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrAuthModeNone)
	require.Equal(t, models.AgentActionStatusFailed, executed.Status)
	require.Empty(t, ops.applyPatch, "nothing may be rendered or applied without an auth mode")

	var result map[string]any
	require.NoError(t, json.Unmarshal(executed.Result, &result))
	require.Contains(t, result["error"], "auth mode")
}

// TestSkipTaskRefusesTaskOutsideIncidentJob pins the incident boundary for task
// targets: an approval for incident X must not reach a task of another job.
func TestSkipTaskRefusesTaskOutsideIncidentJob(t *testing.T) {
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "extract", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeSkipTask,
		Params:     ActionParams{TaskName: "a-task-of-some-other-job"},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "mallory@example.com", "")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrCrossBoundaryTarget)
	require.Equal(t, models.AgentActionStatusFailed, executed.Status)
	require.Empty(t, ops.skipTask)
}

// TestDecodePlaybook pins the fallback direction: an absent or malformed
// playbook must never decode to something more permissive than the default.
func TestDecodePlaybook(t *testing.T) {
	require.Equal(t, Playbook{}, DecodePlaybook(nil))
	require.Equal(t, Playbook{}, DecodePlaybook([]byte(`not json`)))

	pb := DecodePlaybook([]byte(`{"autonomy":{"allow":["pause_job"],"requireApproval":["retry_from_failure"],"paramOverrides":{"badRowPolicy":["quarantine"]}}}`))
	require.True(t, pb.Allow["pause_job"])
	require.False(t, pb.Allow["clear_cache_entry"])
	require.True(t, pb.RequireApproval["retry_from_failure"])
	require.Equal(t, []string{"quarantine"}, pb.ParamOverrides["badRowPolicy"])

	// A playbook that forces approval on a tier-1 action still routes it to a
	// human, which is the only reason RequireApproval exists.
	require.Equal(t, decisionApprove, pb.decide(ActionTypeRetryFromFailure, TierAutonomous))
}

// approveAction records the human decision the way the approvals service does:
// the approval row flips to approved with a decider, and the action row is
// mirrored to `approved`. ExecuteApproved reads both.
func approveAction(t *testing.T, db *gorm.DB, actionID uuid.UUID, decider, reason string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Model(&models.ApprovalRequest{}).
		Where("action_id = ?", actionID).
		Updates(map[string]any{
			"decision":   models.ApprovalDecisionApproved,
			"decider":    decider,
			"reason":     reason,
			"decided_at": now,
			"updated_at": now,
		}).Error)
	require.NoError(t, db.Model(&models.AgentAction{}).
		Where("id = ?", actionID).
		Updates(map[string]any{"status": models.AgentActionStatusApproved, "updated_at": now}).Error)
}
