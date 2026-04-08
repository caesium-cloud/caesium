package bind

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestAllProtectsRESTButLeavesWebhooksPublic(t *testing.T) {
	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	require.NoError(t, env.Process())

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	svc := auth.NewService(db)
	auditor := auth.NewAuditLogger(db)
	limiter := auth.NewRateLimiter(10, time.Minute)

	originalWebhookHandler := webhookHandler
	t.Cleanup(func() {
		webhookHandler = originalWebhookHandler
	})

	webhookCalls := 0
	webhookHandler = func(c *echo.Context) error {
		webhookCalls++
		return c.String(http.StatusAccepted, "webhook")
	}

	e := echo.New()
	All(e.Group("/v1"), nil, svc, auditor, limiter)

	protectedReq := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	protectedRec := httptest.NewRecorder()
	e.ServeHTTP(protectedRec, protectedReq)
	require.Equal(t, http.StatusUnauthorized, protectedRec.Code)

	webhookReq := httptest.NewRequest(http.MethodPost, "/v1/hooks/github/push", strings.NewReader(`{}`))
	webhookRec := httptest.NewRecorder()
	e.ServeHTTP(webhookRec, webhookReq)
	require.Equal(t, http.StatusAccepted, webhookRec.Code)
	require.Equal(t, 1, webhookCalls)
}
