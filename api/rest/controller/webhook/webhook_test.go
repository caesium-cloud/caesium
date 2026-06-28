package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
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

func stubWebhookRouter(t *testing.T, fn func(context.Context, *models.IngestedEvent) (*triggerevent.RouteResult, error)) {
	t.Helper()
	original := routeWebhookEvent
	routeWebhookEvent = fn
	t.Cleanup(func() { routeWebhookEvent = original })
}

func TestReceiveWithServicesFiresWebhookTrigger(t *testing.T) {
	require.NoError(t, env.Process())
	stubWebhookRouter(t, func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		require.Equal(t, "webhook", evt.Type)
		require.Equal(t, "github/push", evt.Source)
		require.JSONEq(t, `{"ref":"refs/heads/main"}`, string(evt.Data))
		return &triggerevent.RouteResult{EventID: uuid.New(), EventType: evt.Type, Source: evt.Source}, nil
	})

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
		nil,
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

func TestReceiveWithServicesContinuesWhenWebhookBridgeFails(t *testing.T) {
	require.NoError(t, env.Process())
	metrics.Register()

	stubWebhookRouter(t, func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		require.Equal(t, "webhook", evt.Type)
		return nil, errors.New("route failed")
	})

	runs := make(chan struct{}, 1)
	before := metrictestutil.CounterValue(t, metrics.EventBridgeFailuresTotal, "webhook")

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/issues", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("Authorization", "Bearer top-secret")
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/issues"}})

	err := ReceiveWithServices(
		c,
		stubTriggerLister{
			triggers: models.Triggers{
				&models.Trigger{
					ID:   uuid.New(),
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"github/issues",
						"secret":"top-secret",
						"signatureScheme":"bearer"
					}`,
				},
			},
		},
		stubJobLister{jobs: models.Jobs{&models.Job{ID: uuid.New(), Alias: "deploy"}}},
		nil,
		func(context.Context, *models.Job, map[string]string) error {
			runs <- struct{}{}
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)

	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook-triggered job")
	}

	after := metrictestutil.CounterValue(t, metrics.EventBridgeFailuresTotal, "webhook")
	require.Greater(t, after, before)
}

func TestReceiveWithServicesRoutesWebhookBeforeHTTPJobs(t *testing.T) {
	require.NoError(t, env.Process())

	runs := make(chan struct{}, 1)
	stubWebhookRouter(t, func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		require.Equal(t, "webhook", evt.Type)
		require.Equal(t, "github", evt.Source)
		require.JSONEq(t, `{"action":"opened"}`, string(evt.Data))
		select {
		case <-runs:
			t.Fatal("HTTP job launched before webhook event route completed")
		default:
		}
		return &triggerevent.RouteResult{EventID: uuid.New(), EventType: evt.Type, Source: evt.Source}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/issues", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("Authorization", "Bearer top-secret")
	req.Header.Set("X-Caesium-Event-Source", "github")
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "github/issues"}})

	err := ReceiveWithServices(
		c,
		stubTriggerLister{
			triggers: models.Triggers{
				&models.Trigger{
					ID:   uuid.New(),
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"github/issues",
						"secret":"top-secret",
						"signatureScheme":"bearer"
					}`,
				},
			},
		},
		stubJobLister{jobs: models.Jobs{&models.Job{ID: uuid.New(), Alias: "deploy"}}},
		nil,
		func(context.Context, *models.Job, map[string]string) error {
			runs <- struct{}{}
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)

	select {
	case <-runs:
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
		nil,
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
		nil,
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
	stubWebhookRouter(t, func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		return &triggerevent.RouteResult{EventID: uuid.New(), EventType: evt.Type, Source: evt.Source}, nil
	})
	webhookRateLimiters = authmw.NewIPRateLimiters(15*time.Minute, webhookRateLimitConfig)

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
	err := ReceiveWithServices(first, triggers, jobs, nil, runner)
	require.NoError(t, err)

	second := newContext()
	err = ReceiveWithServices(second, triggers, jobs, nil, runner)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, httpErr.Code)
}

func TestReceiveWithServicesRecordsMetricOnInvalidSignature(t *testing.T) {
	require.NoError(t, env.Process())
	metrics.Register()

	before := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "github/push", "invalid_signature")

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{"ref":"main"}`))
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
		stubJobLister{},
		nil,
		func(context.Context, *models.Job, map[string]string) error { return nil },
	)
	require.Error(t, err)

	after := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "github/push", "invalid_signature")
	require.Greater(t, after, before)
}

func TestReceiveWithServicesRecordsMetricOnReplayedRequest(t *testing.T) {
	require.NoError(t, env.Process())
	metrics.Register()

	before := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "github/push", "replayed_request")

	body := `{"ref":"main"}`
	ts := fmt.Sprintf("%d", 1713000000)
	mac := hmac.New(sha256.New, []byte("top-secret"))
	_, _ = mac.Write([]byte(ts + "."))
	_, _ = mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Webhook-Timestamp", ts)
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
						"signatureScheme":"hmac-sha256",
						"timestampHeader":"X-Webhook-Timestamp"
					}`,
				},
			},
		},
		stubJobLister{},
		nil,
		func(context.Context, *models.Job, map[string]string) error { return nil },
		triggerhttp.WithNow(func() time.Time { return time.Unix(1713000600, 0) }),
	)
	require.Error(t, err)

	after := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "github/push", "replayed_request")
	require.Greater(t, after, before)
}

func TestReceiveWithServicesNoMetricWhenOneTriggerAccepts(t *testing.T) {
	require.NoError(t, env.Process())
	metrics.Register()
	stubWebhookRouter(t, func(_ context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
		return &triggerevent.RouteResult{EventID: uuid.New(), EventType: evt.Type, Source: evt.Source}, nil
	})

	before := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "multi/path", "invalid_signature")

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/multi/path", strings.NewReader(`{"ref":"main"}`))
	req.Header.Set("Authorization", "Bearer correct-secret")
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: "multi/path"}})

	err := ReceiveWithServices(
		c,
		stubTriggerLister{
			triggers: models.Triggers{
				&models.Trigger{
					ID:   uuid.New(),
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"multi/path",
						"secret":"wrong-secret",
						"signatureScheme":"bearer"
					}`,
				},
				&models.Trigger{
					ID:   uuid.New(),
					Type: models.TriggerTypeHTTP,
					Configuration: `{
						"path":"multi/path",
						"secret":"correct-secret",
						"signatureScheme":"bearer"
					}`,
				},
			},
		},
		stubJobLister{jobs: models.Jobs{&models.Job{ID: uuid.New(), Alias: "deploy"}}},
		nil,
		func(context.Context, *models.Job, map[string]string) error { return nil },
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)

	after := metrictestutil.CounterValue(t, metrics.WebhookAuthFailuresTotal, "multi/path", "invalid_signature")
	require.Equal(t, before, after, "should not record failure when at least one trigger accepts")
}
