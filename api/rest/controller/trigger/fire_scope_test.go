package trigger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

type fireCall struct {
	called bool
	opts   triggerhttp.FireOptions
	listed bool
}

func stubFire(t *testing.T, trig *models.Trigger, jobs models.Jobs) *fireCall {
	t.Helper()
	call := &fireCall{}
	origSvc, origFire, origList := triggerServiceFactory, fireHTTPTrigger, listFireJobs
	t.Cleanup(func() { triggerServiceFactory, fireHTTPTrigger, listFireJobs = origSvc, origFire, origList })
	triggerServiceFactory = func(context.Context) triggersvc.Trigger { return &stubTriggerService{trigger: trig} }
	listFireJobs = func(context.Context, uuid.UUID) (models.Jobs, error) {
		call.listed = true
		return jobs, nil
	}
	fireHTTPTrigger = func(_ context.Context, _ *models.Trigger, _ map[string]string, _ string, opts ...triggerhttp.FireOption) error {
		call.called = true
		for _, opt := range opts {
			opt(&call.opts)
		}
		return nil
	}
	return call
}

func fireAs(t *testing.T, handler func(*echo.Context) error, trig *models.Trigger, principal *iauth.Principal) error {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	c := echo.New().NewContext(req, httptest.NewRecorder())
	c.SetPathValues(echo.PathValues{{Name: "id", Value: trig.ID.String()}})
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	return handler(c)
}

// TestFireConfinedCallerCarriesScopeIntoAdmission (A6): a confined caller
// fires only the trigger's jobs inside its WriteConfinement scope, and the
// scope rides into each run's admission; a refusal there is audited.
func TestFireConfinedCallerCarriesScopeIntoAdmission(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Setenv("CAESIUM_MANUAL_TRIGGER_API_KEY", "")
	require.NoError(t, env.Process())

	trig := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeHTTP}
	mkt := &models.Job{ID: uuid.New(), Alias: "mkt", Namespace: "marketing", TriggerID: trig.ID}
	fin := &models.Job{ID: uuid.New(), Alias: "fin", Namespace: "finance", TriggerID: trig.ID}
	principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_mkt", Grants: policy.Grants{"marketing": models.RoleRunner}}

	call := stubFire(t, trig, models.Jobs{mkt, fin})
	require.NoError(t, fireAs(t, FireWith(iauth.NewAuditLogger(db)), trig, principal))
	require.True(t, call.called, "a partly refused trigger still fires its in-scope jobs")
	require.NotNil(t, call.opts.Allows)
	require.True(t, call.opts.Allows("mkt", "marketing"))
	require.False(t, call.opts.Allows("fin", "finance"))
	require.NotNil(t, call.opts.StartGuard)
	require.NoError(t, call.opts.StartGuard(mkt, "marketing"))

	err := call.opts.StartGuard(mkt, "finance")
	denial, ok := err.(*authz.Denial)
	require.True(t, ok, "a job moved out after preflight is refused at admission: %v", err)
	require.Equal(t, []string{"finance"}, denial.Namespaces)
	var written int64
	require.NoError(t, db.Model(&models.AuditLog{}).Count(&written).Error)
	require.Zero(t, written, "the guard runs inside the admission transaction and must not write")
	require.NotNil(t, call.opts.OnGuardedRunError)
	call.opts.OnGuardedRunError(mkt, fmt.Errorf("start: %w", err))
	call.opts.OnGuardedRunError(mkt, errors.New("an operational failure is not a refusal"))
	var row models.AuditLog
	require.NoError(t, db.Where("action = ?", iauth.ActionAuthDenied).First(&row).Error)
	require.Equal(t, "csk_mkt", row.Actor)
	require.Equal(t, "finance", row.Namespace)
	require.Equal(t, iauth.OutcomeDenied, row.Outcome)
	require.Equal(t, mkt.ID.String(), row.ResourceID)
	require.NoError(t, db.Model(&models.AuditLog{}).Count(&written).Error)
	require.EqualValues(t, 1, written, "only the refusal is audited")

	// Every job left the caller's scope since the middleware authorised it.
	call = stubFire(t, trig, models.Jobs{fin})
	err = fireAs(t, FireWith(nil), trig, principal)
	_, ok = err.(*authz.Denial)
	require.True(t, ok, "want an audited 403, got %v", err)
	require.False(t, call.called)
}

// TestFireScopeFromWriteConfinementOnly: a cluster-granted job-scoped key is
// still confined to its aliases; an unconfined caller without the shared
// secret fires exactly as before (no job read, no options).
func TestFireScopeFromWriteConfinementOnly(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()
	t.Setenv("CAESIUM_MANUAL_TRIGGER_API_KEY", "")
	require.NoError(t, env.Process())
	trig := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeHTTP}
	job := &models.Job{ID: uuid.New(), Alias: "payroll", Namespace: "finance", TriggerID: trig.ID}

	scoped := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_job", Grants: policy.Grants{"*": models.RoleRunner}, Scope: []byte(`{"jobs":["payroll"]}`)}
	call := stubFire(t, trig, models.Jobs{job})
	require.NoError(t, fireAs(t, Fire, trig, scoped))
	require.True(t, call.called)
	require.True(t, call.opts.Allows("payroll", "finance"))
	require.False(t, call.opts.Allows("payroll ", "finance"), "exact alias only")
	require.NoError(t, call.opts.StartGuard(job, "anywhere"), "no namespace restriction for a cluster grant")

	admin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_admin", Grants: policy.Grants{"*": models.RoleAdmin}}
	call = stubFire(t, trig, models.Jobs{job})
	require.NoError(t, fireAs(t, Fire, trig, admin))
	require.True(t, call.called)
	require.False(t, call.listed)
	require.Nil(t, call.opts.Allows)
	require.Nil(t, call.opts.StartGuard)
}
