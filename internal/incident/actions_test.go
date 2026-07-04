package incident

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestActionTierCatalog(t *testing.T) {
	cases := map[string]int{
		ActionTypeRetryFromFailure:         TierAutonomous,
		ActionTypeSnoozeRetry:              TierAutonomous,
		ActionTypeNotify:                   TierAutonomous,
		ActionTypeEscalate:                 TierAutonomous,
		ActionTypeQuarantineReplay:         TierAutonomous,
		ActionTypeRerunWithParams:          TierGated,
		ActionTypePauseJob:                 TierGated,
		ActionTypeUnpauseJob:               TierGated,
		ActionTypeClearCacheEntry:          TierGated,
		ActionTypeSuppressDownstreamAlerts: TierGated,
		ActionTypeExtendSLAOnce:            TierGated,
		ActionTypeSkipTask:                 TierApproval,
		ActionTypeOverrideSchemaGate:       TierApproval,
		ActionTypeApplyJobdefPatch:         TierApproval,
	}
	for actionType, wantTier := range cases {
		tier, ok := ActionTier(actionType)
		require.Truef(t, ok, "%s must be in the catalog", actionType)
		require.Equalf(t, wantTier, tier, "%s tier", actionType)
	}
	_, ok := ActionTier("nope")
	require.False(t, ok)
}

func TestValidateParamOverrides(t *testing.T) {
	whitelist := map[string][]string{
		"badRowPolicy": {"quarantine", "skip"},
		"anyValue":     {}, // empty allowed-set means any value is fine
	}
	require.NoError(t, validateParamOverrides(map[string]string{"badRowPolicy": "quarantine"}, whitelist))
	require.NoError(t, validateParamOverrides(map[string]string{"anyValue": "whatever"}, whitelist))
	require.Error(t, validateParamOverrides(map[string]string{"badRowPolicy": "drop"}, whitelist))
	require.Error(t, validateParamOverrides(map[string]string{"unlisted": "x"}, whitelist))
}

func TestNotifyRequiresChannel(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeNotify,
		Params:     ActionParams{Message: "hi"},
		Playbook:   Playbook{},
	})
	require.Error(t, err)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)
	require.Empty(t, ops.notify)

	action2, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeNotify,
		Params:     ActionParams{Channel: "data-oncall", Message: "late file"},
		Playbook:   Playbook{},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action2.Status)
	require.Len(t, ops.notify, 1)
	require.Equal(t, "data-oncall", ops.notify[0].channel)
}

func TestClearCacheEntryDefaultsToIncidentTask(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store) // task_name "extract"

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeClearCacheEntry,
		Playbook:   Playbook{Allow: map[string]bool{ActionTypeClearCacheEntry: true}},
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action.Status)
	require.Len(t, ops.clearCache, 1)
	require.Equal(t, "extract", ops.clearCache[0].taskName)
	require.Equal(t, inc.JobID, ops.clearCache[0].jobID)
}

func TestExtendSLAOnceRequiresPositiveWindow(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, runID := seedIncident(t, store)
	pb := Playbook{Allow: map[string]bool{ActionTypeExtendSLAOnce: true}}

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeExtendSLAOnce,
		Playbook:   pb,
	})
	require.Error(t, err)
	require.Equal(t, models.AgentActionStatusFailed, action.Status)

	action2, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeExtendSLAOnce,
		Params:     ActionParams{ExtendSeconds: 3600},
		Playbook:   pb,
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, action2.Status)
	require.Len(t, ops.extendSLA, 1)
	require.Equal(t, runID, ops.extendSLA[0].runID)
}

func TestEscalateDispatches(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)

	_, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeEscalate,
		Params:     ActionParams{Channel: "pagerduty", Summary: "vendor file 2h late"},
		Playbook:   Playbook{},
	})
	require.NoError(t, err)
	require.Len(t, ops.escalate, 1)
	require.Equal(t, inc.ID, ops.escalate[0].incidentID)
	require.Equal(t, "pagerduty", ops.escalate[0].channel)
}

func TestRerunWithParamsExplicitJobID(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)
	inc, _ := seedIncident(t, store)
	otherJob := uuid.New()

	_, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeRerunWithParams,
		Params: ActionParams{
			JobID:     &otherJob,
			Overrides: map[string]string{"k": "v"},
		},
		Playbook: Playbook{
			Allow:          map[string]bool{ActionTypeRerunWithParams: true},
			ParamOverrides: map[string][]string{"k": {"v"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, ops.rerun, 1)
	require.Equal(t, otherJob, ops.rerun[0].jobID)
}
