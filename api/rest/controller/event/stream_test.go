package event

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestBuildFilterIncludesQuarantineOnlyForAccessibleRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	runID := seedEventStreamRun(t, db, "alpha")
	scopeJSON, err := json.Marshal(models.KeyScope{Jobs: []string{"alpha"}})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/events?run_id="+runID.String(), nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{Role: models.RoleViewer, Scope: scopeJSON})

	filter, err := New(nil).WithAuthService(iauth.NewService(db)).buildFilter(c)
	require.NoError(t, err)
	require.Equal(t, runID, filter.RunID)
	require.True(t, filter.IncludeQuarantine)
}

func TestBuildFilterAllowsRunScopedQuarantineWhenAuthDisabled(t *testing.T) {
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/events?run_id="+runID.String(), nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	filter, err := New(nil).buildFilter(c)
	require.NoError(t, err)
	require.Equal(t, runID, filter.RunID)
	require.True(t, filter.IncludeQuarantine)
}

func TestBuildFilterRejectsQuarantineForOutOfScopeRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	runID := seedEventStreamRun(t, db, "alpha")
	scopeJSON, err := json.Marshal(models.KeyScope{Jobs: []string{"beta"}})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/events?run_id="+runID.String(), nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{Role: models.RoleViewer, Scope: scopeJSON})

	filter, err := New(nil).WithAuthService(iauth.NewService(db)).buildFilter(c)
	require.Error(t, err)
	require.False(t, filter.IncludeQuarantine)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, httpErr.Code)
}

func seedEventStreamRun(t *testing.T, db *gorm.DB, alias string) uuid.UUID {
	t.Helper()

	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     alias + "-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     alias,
		TriggerID: trigger.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     job.ID,
		Status:    "running",
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return runID
}
