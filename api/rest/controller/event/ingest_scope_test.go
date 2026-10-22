package event

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	freshnesspkg "github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type ingestCall struct {
	scope        *jobdef.TenancyScope
	observeOpts  int
	observeError error
}

// stubIngest routes every event to the given delivered and withheld jobs and
// records the scope the handler asked for.
func stubIngest(t *testing.T, call *ingestCall, delivered, withheld []triggerevent.RoutedJob) {
	t.Helper()
	original, originalArrival := routeEvent, observeEventArrival
	routeEvent = func(_ context.Context, evt *models.IngestedEvent, opts ...triggerevent.RouteOption) (*triggerevent.RouteResult, error) {
		call.scope = triggerevent.DeliveryScopeOf(opts...)
		return &triggerevent.RouteResult{
			EventID:         uuid.New(),
			EventType:       evt.Type,
			MatchedTriggers: []triggerevent.TriggerRouteResult{{TriggerID: uuid.New(), RunsStarted: []uuid.UUID{uuid.New()}}},
			Delivered:       delivered,
			Withheld:        withheld,
		}, nil
	}
	observeEventArrival = func(_ context.Context, _ *models.IngestedEvent, opts ...freshnesspkg.ObserveOption) error {
		call.observeOpts = len(opts)
		return call.observeError
	}
	t.Cleanup(func() { routeEvent, observeEventArrival = original, originalArrival })
}

func ingestAs(t *testing.T, db *gorm.DB, principal *iauth.Principal, legacyHeader string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/events", strings.NewReader(`{"type":"a6.event","data":{}}`))
	if legacyHeader != "" {
		req.Header.Set(iauth.LegacySecretHeader, legacyHeader)
	}
	req.RemoteAddr = "127.0.0.77:1"
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	ctrl := New(nil)
	if db != nil {
		ctrl = ctrl.WithAuditor(iauth.NewAuditLogger(db))
	}
	return rec, ctrl.Ingest(c)
}

func legacyRows(t *testing.T, db *gorm.DB) []models.AuditLog {
	t.Helper()
	var rows []models.AuditLog
	require.NoError(t, db.Where("action = ?", iauth.ActionLegacySecretUsed).Order("timestamp").Find(&rows).Error)
	return rows
}

var (
	mktJob = triggerevent.RoutedJob{JobID: uuid.New(), Namespace: "marketing"}
	finJob = triggerevent.RoutedJob{JobID: uuid.New(), Namespace: "finance"}
	salJob = triggerevent.RoutedJob{JobID: uuid.New(), Namespace: "sales"}
)

// TestIngestUnconfinedResponseUnchanged: auth off with the legacy secret, an
// unscoped Route; the response keeps its fields and adds delivered, skipped 0
// and an empty skipped_namespaces list; the use is audited once.
func TestIngestUnconfinedResponseUnchanged(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "test-key")
	require.NoError(t, env.Process())
	var call ingestCall
	stubIngest(t, &call, []triggerevent.RoutedJob{mktJob, finJob}, nil)

	rec, err := ingestAs(t, db, nil, "test-key")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Nil(t, call.scope)
	require.Zero(t, call.observeOpts)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Equal(t, float64(1), raw["matched_triggers"])
	require.Equal(t, float64(1), raw["runs_started"])
	require.Equal(t, float64(2), raw["delivered"])
	require.Equal(t, float64(0), raw["skipped"])
	require.Equal(t, []any{}, raw["skipped_namespaces"])

	rows := legacyRows(t, db)
	require.Len(t, rows, 1)
	require.Equal(t, iauth.ActorLegacyEventIngest, rows[0].Actor)
	require.Equal(t, iauth.OutcomeSuccess, rows[0].Outcome)
	require.Equal(t, "*", rows[0].Namespace)
	require.NotContains(t, string(rows[0].Metadata), "test-key")
}

// TestIngestConfinedByWriteConfinement: the scope comes only from
// WriteConfinement, also on a server without a policy file (fold A6-1).
func TestIngestConfinedByWriteConfinement(t *testing.T) {
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "")
	require.NoError(t, env.Process())
	previous := policy.ProcessHolder()
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })

	t.Run("namespaced key, fileless server", func(t *testing.T) {
		var call ingestCall
		stubIngest(t, &call, []triggerevent.RoutedJob{mktJob}, []triggerevent.RoutedJob{finJob, salJob, finJob})
		principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_mkt", Grants: policy.Grants{"marketing": models.RoleRunner, "finance": models.RoleViewer}}
		rec, err := ingestAs(t, nil, principal, "")
		require.NoError(t, err)
		require.NotNil(t, call.scope)
		require.Equal(t, []string{"marketing"}, call.scope.Namespaces)
		require.Nil(t, call.scope.JobAliases)
		require.Equal(t, 1, call.observeOpts, "arrival is scoped too")
		var resp ingestResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Equal(t, 1, resp.Delivered)
		require.Equal(t, 3, resp.Skipped)
		require.Equal(t, []string{"finance"}, resp.SkippedNamespaces, "only the namespaces it may read")
	})
	t.Run("cluster-granted job-scoped key", func(t *testing.T) {
		var call ingestCall
		stubIngest(t, &call, []triggerevent.RoutedJob{mktJob}, []triggerevent.RoutedJob{finJob})
		principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_job", Grants: policy.Grants{"*": models.RoleRunner}, Scope: []byte(`{"jobs":["mkt-job"]}`)}
		rec, err := ingestAs(t, nil, principal, "")
		require.NoError(t, err)
		require.NotNil(t, call.scope, "a cluster grant never removes the alias scope")
		require.Nil(t, call.scope.Namespaces)
		require.Equal(t, []string{"mkt-job"}, call.scope.JobAliases)
		var resp ingestResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Equal(t, 1, resp.Skipped)
		require.Equal(t, []string{}, resp.SkippedNamespaces, "a job-scoped key reads no other job")
	})
	t.Run("cluster runner", func(t *testing.T) {
		var call ingestCall
		stubIngest(t, &call, []triggerevent.RoutedJob{mktJob}, nil)
		principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_admin", Grants: policy.Grants{"*": models.RoleAdmin}}
		_, err := ingestAs(t, nil, principal, "")
		require.NoError(t, err)
		require.Nil(t, call.scope)
		require.Zero(t, call.observeOpts)
	})
	t.Run("undecodable scope denies", func(t *testing.T) {
		var call ingestCall
		stubIngest(t, &call, nil, nil)
		principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_bad", Grants: policy.Grants{"*": models.RoleRunner}, Scope: []byte(`{`)}
		_, err := ingestAs(t, nil, principal, "")
		require.Equal(t, http.StatusForbidden, echo.StatusCode(err))
	})
}

// TestIngestLegacyHeaderWithPrincipal: with a principal the secret is
// optional and never widens it; a wrong one is refused before routing; a
// valid one is audited even when processing fails after routing committed.
func TestIngestLegacyHeaderWithPrincipal(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	t.Setenv("CAESIUM_EVENT_INGEST_API_KEY", "test-key")
	require.NoError(t, env.Process())
	principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_mkt", Grants: policy.Grants{"marketing": models.RoleRunner}}

	var call ingestCall
	stubIngest(t, &call, []triggerevent.RoutedJob{mktJob}, []triggerevent.RoutedJob{finJob})
	_, err := ingestAs(t, db, principal, "wrong")
	require.Equal(t, http.StatusUnauthorized, echo.StatusCode(err))
	require.Nil(t, call.scope, "refused before routing")
	require.Empty(t, legacyRows(t, db))

	_, err = ingestAs(t, db, principal, "")
	require.NoError(t, err)
	require.Equal(t, []string{"marketing"}, call.scope.Namespaces)
	require.Empty(t, legacyRows(t, db), "no secret presented, nothing to audit")

	call = ingestCall{observeError: errors.New("observer down")}
	_, err = ingestAs(t, db, principal, "test-key")
	require.Equal(t, http.StatusInternalServerError, echo.StatusCode(err))
	require.Equal(t, []string{"marketing"}, call.scope.Namespaces, "the secret never widens the principal")
	rows := legacyRows(t, db)
	require.Len(t, rows, 1)
	require.Equal(t, iauth.OutcomeError, rows[0].Outcome)
	require.Equal(t, "marketing", rows[0].Namespace)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &metadata))
	require.Equal(t, "csk_mkt", metadata["principal"])
	require.Equal(t, float64(1), metadata["delivered"])
	require.Equal(t, float64(1), metadata["runs_started"])
}
