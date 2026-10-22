package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// scopedKey mints a key of role whose scope is the given exact aliases.
func (f *nsFixture) scopedKey(t *testing.T, role models.Role, aliases ...string) cred {
	t.Helper()
	created, err := f.svc.CreateKey(&auth.CreateKeyRequest{Role: role, Scope: &models.KeyScope{Jobs: aliases}, CreatedBy: "test"})
	require.NoError(t, err)
	return cred{bearer: created.Plaintext}
}

// TestScopedKeyEventIngestAndFire: a job-scoped runner key may ingest (the
// handler filters delivery to its aliases) and may fire a trigger only when
// every job the trigger serves is one of its exact aliases (A6). A trigger
// with no job has no owner and is refused.
func TestScopedKeyEventIngestAndFire(t *testing.T) {
	f := newNSFixture(t)
	mkt := f.scopedKey(t, models.RoleRunner, "ns-mkt")
	both := f.scopedKey(t, models.RoleRunner, "ns-mkt-shared", "ns-fin")
	// A job stored as "ns-pad " (trailing space) is not the key's "ns-pad".
	padTrigger := uuid.New()
	require.NoError(t, f.db.Create(&models.Trigger{ID: padTrigger, Type: models.TriggerTypeHTTP, Configuration: `{}`}).Error)
	require.NoError(t, f.db.Create(&models.Job{ID: uuid.New(), Alias: "ns-pad ", Namespace: "marketing", TriggerID: padTrigger}).Error)
	padded := f.scopedKey(t, models.RoleRunner, "ns-pad")

	require.Equal(t, http.StatusOK, f.do(t, mkt, post("/v1/events", nil, `{}`)).status)

	fire := func(trigger uuid.UUID) nsCall {
		return post("/v1/triggers/:id/fire", pv("id", trigger.String()), "")
	}
	require.Equal(t, http.StatusOK, f.do(t, mkt, fire(f.mktTrigger)).status)
	require.Equal(t, http.StatusForbidden, f.do(t, mkt, fire(f.sharedTrigger)).status)
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"reason": "insufficient_scope"})
	require.Equal(t, http.StatusOK, f.do(t, both, fire(f.sharedTrigger)).status)
	require.Equal(t, http.StatusForbidden, f.do(t, padded, fire(padTrigger)).status, "aliases match exactly, never trimmed")
	require.Equal(t, http.StatusForbidden, f.do(t, both, fire(f.idleTrigger)).status, "a jobless trigger has no owner")
	require.Equal(t, http.StatusNotFound, f.do(t, both, fire(uuid.New())).status)
}

func legacyContext(t *testing.T, header string) *echo.Context {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/events", nil)
	if header != "" {
		req.Header.Set(auth.LegacySecretHeader, header)
	}
	return echo.New().NewContext(req, httptest.NewRecorder())
}

func lastLegacyRow(t *testing.T, f *nsFixture) models.AuditLog {
	t.Helper()
	var entry models.AuditLog
	require.NoError(t, f.db.Where("action = ?", auth.ActionLegacySecretUsed).Order("timestamp DESC").First(&entry).Error)
	return entry
}

// TestLegacySecretAuditOutcome: a presented secret is audited once whatever
// the handler's outcome, decided from its returned error (written to the
// client only after the deferred Finish runs) and otherwise from the
// committed status (A6, auth off).
func TestLegacySecretAuditOutcome(t *testing.T) {
	f := newNSFixture(t)
	t.Setenv("CAESIUM_AUTH_MODE", "none")
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "s3cret")
	require.NoError(t, env.Process())

	for _, tc := range []struct {
		name    string
		finish  func(c *echo.Context) error
		outcome string
	}{
		{"committed 202", func(c *echo.Context) error { return c.JSON(http.StatusAccepted, nil) }, auth.OutcomeSuccess},
		{"observer failure after routing", func(*echo.Context) error {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error")
		}, auth.OutcomeError},
		{"handler denial", func(*echo.Context) error {
			return &authz.Denial{Reason: "insufficient_role", Required: models.RoleRunner, Namespaces: []string{"finance"}}
		}, auth.OutcomeDenied},
		{"plain error", func(*echo.Context) error { return errors.New("boom") }, auth.OutcomeError},
		{"committed 500", func(c *echo.Context) error { return c.NoContent(http.StatusInternalServerError) }, auth.OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := legacyContext(t, "s3cret")
			audit, err := authmw.BeginLegacySecret(c, f.auditor, auth.LegacyEventIngest, env.Variables().EventIngestAPIKey)
			require.NoError(t, err)
			require.NotNil(t, audit)
			audit.SetNamespaces([]string{"marketing"})
			audit.Set("delivered", 1)
			audit.Finish(tc.finish(c))
			row := lastLegacyRow(t, f)
			require.Equal(t, auth.ActorLegacyEventIngest, row.Actor)
			require.Equal(t, tc.outcome, row.Outcome)
			require.Equal(t, "marketing", row.Namespace)
			require.NotContains(t, string(row.Metadata), "s3cret")
		})
	}

	// A wrong secret is refused with no row; a missing one too.
	var before int64
	require.NoError(t, f.db.Model(&models.AuditLog{}).Where("action = ?", auth.ActionLegacySecretUsed).Count(&before).Error)
	for _, header := range []string{"nope", ""} {
		audit, err := authmw.BeginLegacySecret(legacyContext(t, header), f.auditor, auth.LegacyEventIngest, "s3cret")
		require.Nil(t, audit)
		require.Equal(t, http.StatusUnauthorized, echo.StatusCode(err))
	}
	var after int64
	require.NoError(t, f.db.Model(&models.AuditLog{}).Where("action = ?", auth.ActionLegacySecretUsed).Count(&after).Error)
	require.Equal(t, before, after)

	// Unset with auth off: disabled, as before.
	_, err := authmw.BeginLegacySecret(legacyContext(t, "s3cret"), f.auditor, auth.LegacyEventIngest, "")
	require.Equal(t, http.StatusServiceUnavailable, echo.StatusCode(err))
}
