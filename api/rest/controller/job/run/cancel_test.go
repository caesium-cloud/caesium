package run

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestAuthorizeRunCancel(t *testing.T) {
	marketing, finance := "marketing", "finance"
	principal := func(grants policy.Grants) *auth.Principal {
		return &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: grants}
	}
	const (
		allow    = "allow"
		notFound = "not_found"
		denied   = "denied"
	)
	for _, tc := range []struct {
		name      string
		principal *auth.Principal
		namespace string
		want      string
	}{
		{"auth disabled", nil, marketing, allow},
		{"operator at *", principal(policy.Grants{"*": models.RoleOperator}), marketing, allow},
		{"admin at *", principal(policy.Grants{"*": models.RoleAdmin}), marketing, allow},
		{"operator in the run's namespace", principal(policy.Grants{marketing: models.RoleOperator}), marketing, allow},
		{"operator only in another namespace", principal(policy.Grants{finance: models.RoleOperator}), marketing, notFound},
		{"viewer where the run lives", principal(policy.Grants{marketing: models.RoleViewer, finance: models.RoleOperator}), marketing, denied},
		{"viewer at *", principal(policy.Grants{"*": models.RoleViewer}), marketing, denied},
		{"viewer at *, operator where the run lives", principal(policy.Grants{"*": models.RoleViewer, marketing: models.RoleOperator}), marketing, allow},
		{"empty namespace is default", principal(policy.Grants{models.DefaultNamespace: models.RoleOperator}), "", allow},
		{"confined caller, empty namespace", principal(policy.Grants{finance: models.RoleOperator}), "", notFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizeRunCancel(tc.principal, tc.namespace)
			switch tc.want {
			case allow:
				require.NoError(t, err)
			case notFound:
				require.ErrorIs(t, err, errRunCancelNotFound)
			case denied:
				denial, ok := errors.AsType[*authz.Denial](err)
				require.True(t, ok, "want a *authz.Denial, got %v", err)
				require.Equal(t, models.RoleOperator, denial.Required)
			}
		})
	}
}

func TestCancelRunningRunPreservesFinishedTasksAndLeavesOthers(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := runstorage.NewStore(db)
	restoreCancelSeams(t, db, store)

	now := time.Now().UTC()
	job := seedCancelJob(t, db, "cancel-target")
	other := seedCancelJob(t, db, "cancel-other")
	atomID := seedCancelAtom(t, db, now)
	succeededTask := seedCancelTask(t, db, job.ID, atomID, "step-ok", now)
	runningTask := seedCancelTask(t, db, job.ID, atomID, "step-hold", now)
	pendingTask := seedCancelTask(t, db, job.ID, atomID, "step-later", now)
	otherTask := seedCancelTask(t, db, other.ID, atomID, "unrelated", now)

	runID := uuid.New()
	otherRunID := uuid.New()
	seedCancelRun(t, db, runID, job.ID, string(runstorage.StatusRunning), "", now)
	seedCancelRun(t, db, otherRunID, other.ID, string(runstorage.StatusRunning), "", now)
	succeededRow := uuid.New()
	require.NoError(t, db.Create(&models.TaskRun{
		ID: succeededRow, JobRunID: runID, TaskID: succeededTask, AtomID: atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
		Status: string(runstorage.TaskStatusSucceeded), Result: "kept",
		Output: datatypes.JSON(`{"kept":"marker"}`), CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: runningTask, AtomID: atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sleep"]`,
		Status: string(runstorage.TaskStatusRunning), ClaimedBy: "worker-1", CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: pendingTask, AtomID: atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
		Status: string(runstorage.TaskStatusPending), CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: otherRunID, TaskID: otherTask, AtomID: atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sleep"]`,
		Status: string(runstorage.TaskStatusRunning), ClaimedBy: "worker-2", CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.RunLease{
		RunID: runID.String(), OwnerNode: "node-a", AcquiredAt: now,
		LeaseExpiresAt: now.Add(time.Minute), Generation: 1,
	}).Error)

	rec := callCancel(t, job.ID, runID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var body runstorage.JobRun
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, runstorage.StatusCancelled, body.Status)
	require.Equal(t, runstorage.OperatorCancelReason, body.Error)

	var runRow models.JobRun
	require.NoError(t, db.First(&runRow, "id = ?", runID).Error)
	require.Equal(t, string(runstorage.StatusCancelled), runRow.Status)
	require.Equal(t, runstorage.OperatorCancelReason, runRow.Error)
	require.NotNil(t, runRow.CompletedAt)

	var tasks []models.TaskRun
	require.NoError(t, db.Find(&tasks, "job_run_id = ?", runID).Error)
	byTask := map[uuid.UUID]models.TaskRun{}
	for _, row := range tasks {
		byTask[row.TaskID] = row
	}
	require.Equal(t, string(runstorage.TaskStatusSucceeded), byTask[succeededTask].Status)
	require.Equal(t, "kept", byTask[succeededTask].Result)
	require.JSONEq(t, `{"kept":"marker"}`, string(byTask[succeededTask].Output))
	require.Equal(t, string(runstorage.TaskStatusCancelled), byTask[runningTask].Status)
	require.Empty(t, byTask[runningTask].ClaimedBy)
	require.Equal(t, string(runstorage.TaskStatusCancelled), byTask[pendingTask].Status)

	var otherRun models.JobRun
	require.NoError(t, db.First(&otherRun, "id = ?", otherRunID).Error)
	require.Equal(t, string(runstorage.StatusRunning), otherRun.Status)
	var otherTasks []models.TaskRun
	require.NoError(t, db.Find(&otherTasks, "job_run_id = ?", otherRunID).Error)
	require.Equal(t, string(runstorage.TaskStatusRunning), otherTasks[0].Status)
	require.Equal(t, "worker-2", otherTasks[0].ClaimedBy)

	var leases int64
	require.NoError(t, db.Model(&models.RunLease{}).Where("run_id = ?", runID.String()).Count(&leases).Error)
	require.Zero(t, leases)

	again := callCancel(t, job.ID, runID, nil)
	require.Equal(t, http.StatusConflict, again.Code)
	var conflict CancelConflictResponse
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &conflict))
	require.Equal(t, "run_not_running", conflict.Error)
	require.Equal(t, "only a running run can be cancelled", conflict.Message)
	require.Equal(t, string(runstorage.StatusCancelled), conflict.Status)
	require.NoError(t, db.First(&runRow, "id = ?", runID).Error)
	require.Equal(t, runstorage.OperatorCancelReason, runRow.Error, "a second cancel must not rewrite the row")
}

func TestCancelRunningRunConflictAndWrongJob(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := runstorage.NewStore(db)
	restoreCancelSeams(t, db, store)

	now := time.Now().UTC()
	job := seedCancelJob(t, db, "cancel-conflict")
	other := seedCancelJob(t, db, "cancel-foreign")
	finished := uuid.New()
	foreign := uuid.New()
	seedCancelRun(t, db, finished, job.ID, string(runstorage.StatusSucceeded), "finished", now)
	seedCancelRun(t, db, foreign, other.ID, string(runstorage.StatusRunning), "", now)

	rec := callCancel(t, job.ID, finished, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	var conflict CancelConflictResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &conflict))
	require.Equal(t, "run_not_running", conflict.Error)
	require.Equal(t, string(runstorage.StatusSucceeded), conflict.Status)
	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", finished).Error)
	require.Equal(t, string(runstorage.StatusSucceeded), row.Status)
	require.Equal(t, "finished", row.Error)

	err := callCancelErr(t, job.ID, foreign, nil)
	require.ErrorIs(t, err, echo.ErrNotFound)
	row = models.JobRun{}
	require.NoError(t, db.First(&row, "id = ?", foreign).Error)
	require.Equal(t, string(runstorage.StatusRunning), row.Status)

	err = callCancelErr(t, job.ID, uuid.Nil, nil)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)

	viewer := &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: policy.Grants{"marketing": models.RoleViewer}}
	running := uuid.New()
	seedCancelRun(t, db, running, job.ID, string(runstorage.StatusRunning), "", now)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", running).Update("namespace", "marketing").Error)
	err = callCancelErr(t, job.ID, running, viewer)
	denial, ok := errors.AsType[*authz.Denial](err)
	require.True(t, ok, "viewer is a denial, got %v", err)
	require.Equal(t, models.RoleOperator, denial.Required)
	row = models.JobRun{}
	require.NoError(t, db.First(&row, "id = ?", running).Error)
	require.Equal(t, string(runstorage.StatusRunning), row.Status)

	// Concurrency replacement keeps its own reason. Operator cancel must not
	// have changed CancelRun.
	replaced := uuid.New()
	seedCancelRun(t, db, replaced, job.ID, string(runstorage.StatusRunning), "", now)
	require.NoError(t, store.CancelRun(context.Background(), replaced))
	row = models.JobRun{}
	require.NoError(t, db.First(&row, "id = ?", replaced).Error)
	require.Equal(t, string(runstorage.StatusCancelled), row.Status)
	require.Equal(t, "cancelled by concurrency replacement", row.Error)
	require.ErrorIs(t, store.CancelRunningRun(context.Background(), replaced), runstorage.ErrRunNotRunning)
}

func restoreCancelSeams(t *testing.T, db *gorm.DB, store *runstorage.Store) {
	t.Helper()
	oldJob, oldRun, oldCancel := cancelGetJob, cancelGetRun, cancelRunningRun
	t.Cleanup(func() { cancelGetJob, cancelGetRun, cancelRunningRun = oldJob, oldRun, oldCancel })
	cancelGetJob = func(_ context.Context, id uuid.UUID) (*models.Job, error) {
		var job models.Job
		err := db.First(&job, "id = ?", id).Error
		return &job, err
	}
	cancelGetRun = func(_ context.Context, id uuid.UUID) (*runstorage.JobRun, error) {
		return store.Get(id)
	}
	cancelRunningRun = func(ctx context.Context, id uuid.UUID) error {
		return store.CancelRunningRun(ctx, id)
	}
}

func callCancel(t *testing.T, jobID, runID uuid.UUID, principal *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := cancelContext(t, jobID, runID, principal)
	require.NoError(t, Cancel(c))
	return rec
}

func callCancelErr(t *testing.T, jobID, runID uuid.UUID, principal *auth.Principal) error {
	t.Helper()
	c, _ := cancelContext(t, jobID, runID, principal)
	return Cancel(c)
}

func cancelContext(t *testing.T, jobID, runID uuid.UUID, principal *auth.Principal) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/cancel", nil)
	c := e.NewContext(req, rec)
	runValue := runID.String()
	if runID == uuid.Nil {
		runValue = "not-a-uuid"
	}
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runValue}})
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	return c, rec
}

func seedCancelJob(t *testing.T, db *gorm.DB, alias string) *models.Job {
	t.Helper()
	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID: uuid.New(), Alias: alias + "-trigger", Type: models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: alias, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	return job
}

func seedCancelAtom(t *testing.T, db *gorm.DB, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.Atom{
		ID: id, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	return id
}

func seedCancelTask(t *testing.T, db *gorm.DB, jobID, atomID uuid.UUID, name string, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.Task{
		ID: id, JobID: jobID, AtomID: atomID, Name: name, CreatedAt: now, UpdatedAt: now,
	}).Error)
	return id
}

func seedCancelRun(t *testing.T, db *gorm.DB, runID, jobID uuid.UUID, status, errText string, now time.Time) {
	t.Helper()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: status, Error: errText,
		StartedAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}).Error)
}
