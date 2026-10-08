package backfill

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/robfig/cron"
	"github.com/stretchr/testify/require"
)

func TestBackfillOwnershipAndCancellation(t *testing.T) {
	oldJob, oldTrigger, oldCreate, oldExecute := backfillGetJob, backfillGetTrigger, backfillCreate, backfillExecute
	t.Cleanup(func() {
		backfillGetJob, backfillGetTrigger, backfillCreate, backfillExecute = oldJob, oldTrigger, oldCreate, oldExecute
	})
	jobID := uuid.New()
	backfillGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &models.Job{ID: jobID}, nil }
	backfillGetTrigger = func(context.Context, uuid.UUID) (*models.Trigger, error) {
		return &models.Trigger{Type: models.TriggerTypeCron, Configuration: `{"expression":"* * * * *"}`}, nil
	}
	owner := runlife.New(context.Background())
	request, cancelRequest := context.WithCancel(runlife.WithSupervisor(context.Background(), owner))
	defer cancelRequest()
	post := func(ctx context.Context) (*httptest.ResponseRecorder, error) {
		e := echo.New()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(`{"start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c := e.NewContext(req, rec)
		c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}})
		return rec, Post(c)
	}
	var backfillID uuid.UUID
	backfillCreate = func(b *models.Backfill) error { backfillID = b.ID; return nil }
	started := make(chan context.Context, 1)
	finish := make(chan struct{})
	backfillExecute = func(ctx context.Context, _ *models.Backfill, _ *models.Job, _ cron.Schedule, _ *time.Location) {
		started <- ctx
		<-finish
	}
	rec, err := post(request)
	require.NoError(t, err)
	require.Equal(t, 202, rec.Code)
	running := <-started
	cancelRequest()
	require.NoError(t, running.Err())
	cancelFuncsMu.Lock()
	cancel := cancelFuncs[backfillID]
	cancelFuncsMu.Unlock()
	require.NotNil(t, cancel)
	cancel()
	require.ErrorIs(t, running.Err(), context.Canceled)
	owner.CloseAndCancel()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded)
	close(finish)
	require.NoError(t, owner.Wait(context.Background()))
	cancelFuncsMu.Lock()
	_, stillRegistered := cancelFuncs[backfillID]
	cancelFuncsMu.Unlock()
	require.False(t, stillRegistered)
	mutated := false
	backfillCreate = func(*models.Backfill) error { mutated = true; return nil }
	_, err = post(runlife.WithSupervisor(context.Background(), owner))
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	require.Equal(t, 503, he.Code)
	require.False(t, mutated)
	// Failed durable creation must release its reservation without a launch.
	other := runlife.New(context.Background())
	backfillCreate = func(*models.Backfill) error { return errors.New("insert failed") }
	_, err = post(runlife.WithSupervisor(context.Background(), other))
	require.ErrorAs(t, err, &he)
	require.Equal(t, 500, he.Code)
	other.CloseAndCancel()
	require.NoError(t, other.Wait(context.Background()))
}

func TestBackfillRejectsUnknownRawReprocessBeforeAdmission(t *testing.T) {
	oldJob, oldTrigger, oldCreate := backfillGetJob, backfillGetTrigger, backfillCreate
	t.Cleanup(func() { backfillGetJob, backfillGetTrigger, backfillCreate = oldJob, oldTrigger, oldCreate })
	jobID := uuid.New()
	backfillGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &models.Job{ID: jobID}, nil }
	backfillGetTrigger = func(context.Context, uuid.UUID) (*models.Trigger, error) {
		return &models.Trigger{Type: models.TriggerTypeCron, Configuration: `{"expression":"* * * * *"}`}, nil
	}
	backfillCreate = func(*models.Backfill) error { t.Fatal("invalid policy reached durable create"); return nil }
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{"start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","reprocess":"invalid"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}})
	err := Post(c)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	require.Equal(t, http.StatusBadRequest, he.Code)
}
