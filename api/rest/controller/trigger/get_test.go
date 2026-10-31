package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetMalformedUUIDReturnsBadRequestBeforeService(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, getTrigger(t, "not-a-uuid").Code)
}

func getTrigger(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.GET("/v1/triggers/:id", Get)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/triggers/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// useGetTestDB points Get at a test database, read through ctx when it is set.
func useGetTestDB(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := triggerGetService
	triggerGetService = func(requestCtx context.Context) tsvc.Trigger {
		if ctx != nil {
			requestCtx = ctx
		}
		return tsvc.ServiceWithDatabase(requestCtx, db)
	}
	t.Cleanup(func() { triggerGetService = prev })
	return db
}

func TestGetKnownTriggerReturnsIt(t *testing.T) {
	db := useGetTestDB(t, nil)
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Trigger{ID: id, Alias: "known", Type: models.TriggerTypeHTTP, Configuration: `{"path":"/known"}`, CreatedAt: now, UpdatedAt: now}).Error)

	rec := getTrigger(t, id.String())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), id.String())
}

// An unknown id is a 404, not the 500 the service's gorm.ErrRecordNotFound
// used to become.
func TestGetUnknownIDReturnsNotFound(t *testing.T) {
	useGetTestDB(t, nil)

	require.Equal(t, http.StatusNotFound, getTrigger(t, uuid.NewString()).Code)
}

func TestGetOtherServiceErrorStaysInternal(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	useGetTestDB(t, cancelled)

	require.Equal(t, http.StatusInternalServerError, getTrigger(t, uuid.NewString()).Code)
}
