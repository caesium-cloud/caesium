package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestConnectorMessageRoutesRequireClusterRoleAndUnscopedCredentials(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	viewer := createKey(t, svc, models.RoleViewer, nil)
	runner := createKey(t, svc, models.RoleRunner, nil)
	operator := createKey(t, svc, models.RoleOperator, nil)
	scoped := createKey(t, svc, models.RoleAdmin, &models.KeyScope{Jobs: []string{"alpha"}})
	agent := mintAgentKey(t, svc, uuid.New(), []string{"alpha"})
	for _, route := range []*echo.RouteInfo{
		{Method: http.MethodPost, Path: "/v1/connectors/:connection_id/executions/:execution_id/queries/:query_id"},
		{Method: http.MethodPost, Path: "/v1/connectors/:connection_id/executions/:execution_id/actions/:action_id"},
		{Method: http.MethodGet, Path: "/v1/connectors/:connection_id/executions/:execution_id/operations"},
		{Method: http.MethodGet, Path: "/v1/connectors/:connection_id/operations/:operation_id"},
		{Method: http.MethodPost, Path: "/v1/connectors/:connection_id/operations/:operation_id/resubmit"},
	} {
		policy, ok := auth.LookupRoutePolicy(route.Method, authmw.NormalizeRoutePath(route.Path))
		require.True(t, ok)
		require.Equal(t, auth.RouteClassCluster, policy.Class)
		for _, test := range []struct {
			token  string
			role   models.Role
			scoped bool
		}{{viewer, models.RoleViewer, false}, {runner, models.RoleRunner, false}, {operator, models.RoleOperator, false}, {scoped, models.RoleAdmin, true}, {agent, models.RoleRunner, true}} {
			req := httptest.NewRequestWithContext(t.Context(), route.Method, route.Path, nil)
			req.Header.Set("Authorization", "Bearer "+test.token)
			called := false
			_, err := callMiddleware(t, svc, auditor, limiter, req, route, nil, func(c *echo.Context) error { called = true; return c.NoContent(http.StatusOK) })
			expected := !test.scoped && auth.HasRole(test.role, policy.Role)
			require.Equal(t, expected, called, route.Path)
			if expected {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Equal(t, http.StatusForbidden, err.(*echo.HTTPError).Code)
			}
		}
	}
}
