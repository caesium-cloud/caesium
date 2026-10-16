package authz_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAllowedNamespacesNeverConfusesUnrestrictedWithNothing(t *testing.T) {
	allowed, restricted := authz.AllowedNamespaces(context.Background())
	require.False(t, restricted, "no restriction in the context means unrestricted")
	require.Nil(t, allowed)

	allowed, restricted = authz.AllowedNamespaces(authz.WithAllowedNamespaces(context.Background(), nil))
	require.True(t, restricted, "restricting to nothing is still a restriction")
	require.NotNil(t, allowed)
	require.Empty(t, allowed)

	allowed, restricted = authz.AllowedNamespaces(authz.WithAllowedNamespaces(context.Background(), []string{"finance", "marketing", "finance", " "}))
	require.True(t, restricted)
	require.Equal(t, []string{"finance", "marketing"}, allowed, "sorted, de-duplicated, blanks dropped")

	// The returned slice is a copy: a handler cannot widen the stored set.
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	got, _ := authz.AllowedNamespaces(ctx)
	got[0] = "finance"
	again, _ := authz.AllowedNamespaces(ctx)
	require.Equal(t, []string{"marketing"}, again)
}

func seedNamespacedJobs(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}).Error)
	for alias, namespace := range map[string]string{"m1": "marketing", "f1": "finance", "d1": "default", "legacy": ""} {
		require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: alias, Namespace: "placeholder", TriggerID: triggerID, CreatedAt: now, UpdatedAt: now}).Error)
		// A legacy row predates the column default; write the raw value.
		require.NoError(t, db.Exec("UPDATE jobs SET namespace = ? WHERE alias = ?", namespace, alias).Error)
	}
}

func aliases(t *testing.T, q *gorm.DB) []string {
	t.Helper()
	var out []string
	require.NoError(t, q.Model(&models.Job{}).Order("alias").Pluck("alias", &out).Error)
	slices.Sort(out)
	return out
}

func TestNamespaceFilter(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	seedNamespacedJobs(t, db)

	// Unrestricted: a no-op, and nothing to mark.
	unrestricted := db.WithContext(context.Background())
	require.Equal(t, []string{"d1", "f1", "legacy", "m1"}, aliases(t, authz.NamespaceFilter(unrestricted, "jobs.namespace")))
	require.True(t, authz.Filtered(context.Background()))

	// Restricted to one namespace: IN (...), and the request is marked.
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.False(t, authz.Filtered(ctx))
	require.Equal(t, []string{"m1"}, aliases(t, authz.NamespaceFilter(db.WithContext(ctx), "jobs.namespace")))
	require.True(t, authz.Filtered(ctx))

	// default also matches the legacy empty value.
	ctx = authz.WithAllowedNamespaces(context.Background(), []string{"default", "finance"})
	require.Equal(t, []string{"d1", "f1", "legacy"}, aliases(t, authz.NamespaceFilter(db.WithContext(ctx), "namespace")))

	// Restricted to nothing matches nothing.
	ctx = authz.WithAllowedNamespaces(context.Background(), []string{})
	require.Empty(t, aliases(t, authz.NamespaceFilter(db.WithContext(ctx), "jobs.namespace")))

	// An invalid column fails closed.
	ctx = authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.Empty(t, aliases(t, authz.NamespaceFilter(db.WithContext(ctx), "namespace; DROP TABLE jobs")))

	// A query built without the request context finds no restriction and does
	// not mark the request: that is what the collection guard catches.
	ctx = authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	_ = aliases(t, authz.NamespaceFilter(db, "jobs.namespace"))
	require.False(t, authz.Filtered(ctx))

	require.Nil(t, authz.NamespaceFilter(nil, "namespace"))
}

func TestMarkFiltered(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	require.False(t, authz.Filtered(c.Request().Context()))
	authz.MarkFiltered(c)
	require.True(t, authz.Filtered(c.Request().Context()))

	// No-op without a restriction.
	plain := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), httptest.NewRecorder())
	authz.MarkFiltered(plain)
	require.True(t, authz.Filtered(plain.Request().Context()))
	authz.MarkFiltered(nil)
}

func TestDecisionAuditNamespace(t *testing.T) {
	var nilDecision *authz.Decision
	require.Equal(t, "", nilDecision.AuditNamespace())
	for _, tc := range []struct {
		decision authz.Decision
		want     string
	}{
		{authz.Decision{Class: auth.RouteClassIdentity}, ""},
		{authz.Decision{Class: auth.RouteClassCluster}, "*"},
		{authz.Decision{Class: auth.RouteClassCollection, Namespaces: []string{"marketing"}}, "*"},
		{authz.Decision{Class: auth.RouteClassNamespaced}, ""},
		{authz.Decision{Class: auth.RouteClassNamespaced, Namespaces: []string{"marketing"}}, "marketing"},
		{authz.Decision{Class: auth.RouteClassNamespaced, Namespaces: []string{"*"}}, "*"},
		{authz.Decision{Class: auth.RouteClassNamespaced, Namespaces: []string{"finance", "marketing"}}, "*"},
	} {
		require.Equal(t, tc.want, tc.decision.AuditNamespace(), "%+v", tc.decision)
	}

	e := echo.New()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), httptest.NewRecorder())
	require.Nil(t, authz.DecisionFrom(c))
	require.Equal(t, "", authz.AuditNamespace(c))
	authz.SetDecision(c, &authz.Decision{Class: auth.RouteClassNamespaced, Namespaces: []string{"marketing"}})
	require.Equal(t, "marketing", authz.AuditNamespace(c))
	require.Nil(t, authz.DecisionFrom(nil))

	// NoteNamespace fills an unresolved namespaced decision and never
	// overrides a resolved one or another class.
	authz.NoteNamespace(c, "finance")
	require.Equal(t, "marketing", authz.AuditNamespace(c))
	authz.SetDecision(c, &authz.Decision{Class: auth.RouteClassNamespaced, Unrestricted: true})
	authz.NoteNamespace(c, "")
	require.Equal(t, "default", authz.AuditNamespace(c))
	authz.SetDecision(c, &authz.Decision{Class: auth.RouteClassCluster})
	authz.NoteNamespace(c, "finance")
	require.Equal(t, "*", authz.AuditNamespace(c))
	plain := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), httptest.NewRecorder())
	authz.NoteNamespace(plain, "finance") // auth off: no decision, no-op
	require.Equal(t, "", authz.AuditNamespace(plain))
}

func TestRequireRoleIn(t *testing.T) {
	require.NoError(t, authz.RequireRoleIn(nil, models.RoleViewer, "finance"), "auth off")

	cluster := &auth.Principal{Grants: policy.Grants{"*": models.RoleViewer}}
	require.NoError(t, authz.RequireRoleIn(cluster, models.RoleViewer, "finance"), "unrestricted")

	confined := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator}}
	require.NoError(t, authz.RequireRoleIn(confined, models.RoleViewer, "marketing"))
	err := authz.RequireRoleIn(confined, models.RoleViewer, "finance")
	var denial *authz.Denial
	require.ErrorAs(t, err, &denial)
	require.Equal(t, "insufficient_role", denial.Reason)
	require.Equal(t, models.RoleViewer, denial.Required)
	require.Equal(t, []string{"finance"}, denial.Namespaces)
	require.Equal(t, "insufficient permissions", denial.Message)

	// "" is the default namespace, never the cluster role.
	defaultOnly := &auth.Principal{Grants: policy.Grants{"default": models.RoleViewer}}
	require.NoError(t, authz.RequireRoleIn(defaultOnly, models.RoleViewer, ""))
	require.Error(t, authz.RequireRoleIn(confined, models.RoleViewer, ""))

	empty := &auth.Principal{Grants: policy.Grants{}}
	require.Error(t, authz.RequireRoleIn(empty, models.RoleViewer, "marketing"))
}

func TestParseNamespaceQuery(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		present bool
		wantErr bool
	}{
		{"", "", false, false},
		{"marketing", "marketing", true, false},
		{"*", "*", true, false},
		{"default", "default", true, false},
		{" marketing", "", true, true},
		{"   ", "", true, true},
		{"Marketing", "", true, true},
		{"a/b", "", true, true},
		{"-bad", "", true, true},
	} {
		got, present, err := authz.ParseNamespaceQuery(tc.raw)
		require.Equal(t, tc.present, present, tc.raw)
		if tc.wantErr {
			require.Error(t, err, tc.raw)
			continue
		}
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.want, got, tc.raw)
	}
}
