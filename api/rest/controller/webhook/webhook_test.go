package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

type stubTriggerLister struct {
	triggers models.Triggers
	err      error
}

func (s stubTriggerLister) ListByPath(string) (models.Triggers, error) {
	return s.triggers, s.err
}

type stubJobLister struct {
	jobs models.Jobs
	err  error
}

func (s stubJobLister) List(*jsvc.ListRequest) (models.Jobs, error) {
	return s.jobs, s.err
}

func TestReceiveWithServicesFiresWebhookTrigger(t *testing.T) {
	require.NoError(t, env.Process())

	triggerID := uuid.New()
	jobID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("Authorization", "Bearer top-secret")
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/push"}})

	runs := make(chan map[string]string, 1)
	runner := func(ctx context.Context, j *models.Job, params map[string]string) error {
		copied := make(map[string]string, len(params))
		for k, v := range params {
			copied[k] = v
		}
		runs <- copied
		return nil
	}

	err := ReceiveWithServices(
		c,
		stubTriggerLister{
			triggers: models.Triggers{
				&models.Trigger{
					ID:   triggerID,
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"github/push",
						"secret":"top-secret",
						"signatureScheme":"bearer",
						"defaultParams":{"environment":"staging"},
						"paramMapping":{"branch":"$.ref"}
					}`,
				},
			},
		},
		stubJobLister{
			jobs: models.Jobs{
				&models.Job{ID: jobID, Alias: "deploy"},
			},
		},
		runner,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)

	select {
	case params := <-runs:
		require.Equal(t, map[string]string{
			"environment": "staging",
			"branch":      "refs/heads/main",
		}, params)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook-triggered job")
	}
}

func TestReceiveWithServicesRejectsInvalidSignature(t *testing.T) {
	require.NoError(t, env.Process())

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/push"}})

	err := ReceiveWithServices(
		c,
		stubTriggerLister{
			triggers: models.Triggers{
				&models.Trigger{
					ID:   uuid.New(),
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"github/push",
						"secret":"top-secret",
						"signatureScheme":"bearer"
					}`,
				},
			},
		},
		stubJobLister{
			jobs: models.Jobs{
				&models.Job{ID: uuid.New(), Alias: "deploy"},
			},
		},
		func(context.Context, *models.Job, map[string]string) error { return nil },
	)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
}

func TestReceiveWithServicesRejectsOversizedBody(t *testing.T) {
	t.Setenv("CAESIUM_WEBHOOK_MAX_BODY_SIZE", "8B")
	require.NoError(t, env.Process())

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{"ref":"refs/heads/main"}`))
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/push"}})

	err := ReceiveWithServices(
		c,
		stubTriggerLister{},
		stubJobLister{},
		func(context.Context, *models.Job, map[string]string) error { return nil },
	)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusRequestEntityTooLarge, httpErr.Code)
}

func TestReceiveWithServicesRateLimitsByIP(t *testing.T) {
	t.Setenv("CAESIUM_WEBHOOK_RATE_LIMIT_PER_MINUTE", "1")
	t.Setenv("CAESIUM_WEBHOOK_RATE_LIMIT_BURST", "1")
	require.NoError(t, env.Process())
	webhookRateLimiters = &ipRateLimiters{
		clients:  map[string]*clientLimiter{},
		staleAge: 15 * time.Minute,
	}

	newContext := func() *echo.Context {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{"ref":"refs/heads/main"}`))
		req.Header.Set("Authorization", "Bearer top-secret")
		req.RemoteAddr = "203.0.113.8:1234"
		rec := httptest.NewRecorder()
		e := echo.New()
		c := e.NewContext(req, rec)
		c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/push"}})
		return c
	}

	triggers := stubTriggerLister{
		triggers: models.Triggers{
			&models.Trigger{
				ID:   uuid.New(),
				Type: models.TriggerTypeHTTP,
				Configuration: `{
					"path":"github/push",
					"secret":"top-secret",
					"signatureScheme":"bearer"
				}`,
			},
		},
	}
	jobs := stubJobLister{jobs: models.Jobs{&models.Job{ID: uuid.New(), Alias: "deploy"}}}
	runner := func(context.Context, *models.Job, map[string]string) error { return nil }

	first := newContext()
	err := ReceiveWithServices(first, triggers, jobs, runner)
	require.NoError(t, err)

	second := newContext()
	err = ReceiveWithServices(second, triggers, jobs, runner)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, httpErr.Code)
}
