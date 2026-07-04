package agent

import (
	"context"
	"testing"
	"time"

	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedJob(t *testing.T, db *gorm.DB, alias string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	trigger := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: trigger, Type: models.TriggerTypeCron, Configuration: `{"cron":"0 * * * *"}`, CreatedAt: now, UpdatedAt: now}).Error)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: alias, TriggerID: trigger, CreatedAt: now, UpdatedAt: now}).Error)
	return jobID
}

func seedIncident(t *testing.T, db *gorm.DB, jobID uuid.UUID) *models.Incident {
	t.Helper()
	inc, _, err := iincident.NewStore(db).OpenOrAppend(context.Background(), iincident.OpenParams{
		JobID: jobID, TaskName: "extract", Class: iincident.ClassUnknown,
	})
	require.NoError(t, err)
	return inc
}

func TestProposeActionRecordsProposedWhenNoExecutor(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Cleanup(func() { SetActionExecutor(nil) })
	SetActionExecutor(nil)

	jobID := seedJob(t, db, "vendor-x")
	inc := seedIncident(t, db, jobID)

	svc := &Service{ctx: context.Background(), db: db}
	res, err := svc.ProposeAction(inc, ActionRequest{Type: "retry_from_failure", Params: []byte(`{"run_id":"x"}`)})
	require.NoError(t, err)
	require.Equal(t, "proposed", res.Disposition)
	require.Equal(t, models.AgentActionStatusProposed, res.Action.Status)
	require.Equal(t, models.AgentActionActorAgent, res.Action.Actor)

	var count int64
	require.NoError(t, db.Model(&models.AgentAction{}).Where("incident_id = ?", inc.ID).Count(&count).Error)
	require.Equal(t, int64(1), count)
}

func TestProposeActionRequiresType(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Cleanup(func() { SetActionExecutor(nil) })

	jobID := seedJob(t, db, "vendor-x")
	inc := seedIncident(t, db, jobID)

	svc := &Service{ctx: context.Background(), db: db}
	_, err := svc.ProposeAction(inc, ActionRequest{Type: "  "})
	require.ErrorIs(t, err, ErrUnknownActionType)
}

// fakeExecutor stands in for Stream B's executor and records delegation.
type fakeExecutor struct{ called bool }

func (f *fakeExecutor) ExecuteAgentAction(_ context.Context, req ActionRequest) (*ActionResult, error) {
	f.called = true
	return &ActionResult{Action: &models.AgentAction{IncidentID: req.IncidentID, Type: req.Type, Status: models.AgentActionStatusExecuted}, Disposition: "executed"}, nil
}

func TestProposeActionDelegatesToRegisteredExecutor(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Cleanup(func() { SetActionExecutor(nil) })

	jobID := seedJob(t, db, "vendor-x")
	inc := seedIncident(t, db, jobID)

	exec := &fakeExecutor{}
	SetActionExecutor(exec)

	svc := &Service{ctx: context.Background(), db: db}
	res, err := svc.ProposeAction(inc, ActionRequest{Type: "retry_from_failure"})
	require.NoError(t, err)
	require.True(t, exec.called)
	require.Equal(t, "executed", res.Disposition)
}

func TestHistoryEnforcesAllowlist(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := seedJob(t, db, "vendor-x")
	seedJob(t, db, "other-job")
	inc := seedIncident(t, db, jobID)

	svc := &Service{ctx: context.Background(), db: db}

	// A cross-job read for a job NOT in the frozen allowlist is refused.
	_, err := svc.History(inc, "other-job", []string{"vendor-x"})
	require.ErrorIs(t, err, ErrForbiddenJob)

	// The incident's own job (default) is always readable.
	_, err = svc.History(inc, "", []string{"vendor-x"})
	require.NoError(t, err)

	// A job within the allowlist is readable.
	_, err = svc.History(inc, "vendor-x", []string{"vendor-x"})
	require.NoError(t, err)
}
