package atom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetMalformedUUIDReturnsBadRequestBeforeService(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, getAtom(t, "not-a-uuid").Code)
}

func getAtom(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.GET("/v1/atoms/:id", Get)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/atoms/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// useTestDB points Get at a test database, read through ctx when it is set.
func useTestDB(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := atomGetService
	atomGetService = func(requestCtx context.Context) svc.Atom {
		if ctx != nil {
			requestCtx = ctx
		}
		return svc.ServiceWithDB(requestCtx, db)
	}
	t.Cleanup(func() { atomGetService = prev })
	return db
}

func TestGetKnownAtomReturnsIt(t *testing.T) {
	db := useTestDB(t, nil)
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Atom{ID: id, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", CreatedAt: now, UpdatedAt: now}).Error)

	rec := getAtom(t, id.String())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), id.String())
}

// An unknown id is a 404, not the 500 the service's gorm.ErrRecordNotFound
// used to become.
func TestGetUnknownIDReturnsNotFound(t *testing.T) {
	useTestDB(t, nil)

	require.Equal(t, http.StatusNotFound, getAtom(t, uuid.NewString()).Code)
}

func TestGetOtherServiceErrorStaysInternal(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	useTestDB(t, cancelled)

	require.Equal(t, http.StatusInternalServerError, getAtom(t, uuid.NewString()).Code)
}
