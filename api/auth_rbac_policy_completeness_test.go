package api

import (
	"context"
	"net/http"
	"sort"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestAuthMiddlewareMountedRoutesHaveRBACPolicy(t *testing.T) {
	t.Cleanup(func() {
		require.NoError(t, env.Process())
	})

	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	t.Setenv("CAESIUM_AUTH_OIDC_ENABLED", "true")
	t.Setenv("CAESIUM_AUTH_SAML_ENABLED", "true")
	t.Setenv("CAESIUM_AUTH_LDAP_ENABLED", "true")
	t.Setenv("CAESIUM_LOG_CONSOLE_ENABLED", "true")
	t.Setenv("CAESIUM_DATABASE_CONSOLE_ENABLED", "true")
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	require.NoError(t, env.Process())

	vars := env.Variables()
	e := echo.New()
	authSvc := iauth.NewService(nil)
	sessions := iauth.NewSessionStore(nil)
	providers := SSOProviders{
		OIDC: routeOnlyRedirectProvider{name: "oidc"},
		SAML: routeOnlyRedirectProvider{name: "saml"},
		LDAP: routeOnlyCredentialProvider{name: "ldap"},
	}

	e.GET("/health", Health)
	e.GET("/auth/status", authStatus(vars))
	registerSSORoutes(e, vars, authSvc, nil, nil, sessions, nil, providers)
	registerMetrics(e, vars, authSvc, nil, nil, sessions)
	bind.All(e.Group("/v1"), nil, authSvc, nil, nil, sessions)

	mounted := make(map[string]struct{})
	var missing []string
	for _, route := range e.Router().Routes() {
		if authmw.IsPublicAuthPath(route.Path) {
			continue
		}

		path := authmw.NormalizeRoutePath(route.Path)
		key := route.Method + " " + path
		mounted[key] = struct{}{}
		if _, ok := iauth.RequiredRole(route.Method, path); !ok {
			missing = append(missing, key)
		}
	}

	require.Contains(t, mounted, "GET /metrics")
	require.Contains(t, mounted, "GET /auth/whoami")
	require.Contains(t, mounted, "GET /v1/events")
	require.Contains(t, mounted, "GET /v1/auth/keys")
	require.Contains(t, mounted, "GET /v1/logs/level")
	require.Contains(t, mounted, "GET /v1/database/schema")
	require.Contains(t, mounted, "GET /v1/contracts/graph")

	sort.Strings(missing)
	require.Empty(t, missing, "auth-middleware mounted routes missing endpointPolicy entries")
}

type routeOnlyRedirectProvider struct {
	name string
}

func (p routeOnlyRedirectProvider) Name() string {
	return p.name
}

func (p routeOnlyRedirectProvider) Begin(http.ResponseWriter, *http.Request, string) (string, error) {
	return "", nil
}

func (p routeOnlyRedirectProvider) Complete(*http.Request) (*iauth.ExternalIdentity, error) {
	return nil, nil
}

type routeOnlyCredentialProvider struct {
	name string
}

func (p routeOnlyCredentialProvider) Name() string {
	return p.name
}

func (p routeOnlyCredentialProvider) Authenticate(context.Context, string, string) (*iauth.ExternalIdentity, error) {
	return nil, nil
}
