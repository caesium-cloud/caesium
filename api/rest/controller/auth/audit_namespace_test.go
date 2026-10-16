package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestQueryAuditNamespaceFilter: ?namespace= filters rows exactly; omitted
// returns every row (cluster admins keep seeing everything); an invalid value
// is a 400.
func TestQueryAuditNamespaceFilter(t *testing.T) {
	ctrl, _, auditor := setupControllerDeps(t)
	for actor, namespace := range map[string]string{"m": "marketing", "f": "finance", "c": "*", "i": ""} {
		require.NoError(t, auditor.Log(iauth.AuditEntry{Actor: actor, Action: iauth.ActionJobPause, Outcome: iauth.OutcomeSuccess, Namespace: namespace}))
	}

	query := func(target string) []models.AuditLog {
		t.Helper()
		c, rec := newAuthContext(t, http.MethodGet, target, "")
		require.NoError(t, ctrl.QueryAudit(c))
		var entries []models.AuditLog
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
		return entries
	}
	marketing := query("/v1/auth/audit?namespace=marketing")
	require.Len(t, marketing, 1)
	require.Equal(t, "m", marketing[0].Actor)
	require.Equal(t, "marketing", marketing[0].Namespace)
	require.Len(t, query("/v1/auth/audit?namespace=*"), 1)
	require.Len(t, query("/v1/auth/audit"), 4)

	for _, bad := range []string{"Bad", "%20marketing", "a%2Fb"} {
		c, _ := newAuthContext(t, http.MethodGet, "/v1/auth/audit?namespace="+bad, "")
		err := ctrl.QueryAudit(c)
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, bad)
		require.Equal(t, http.StatusBadRequest, he.Code, bad)
	}
}
