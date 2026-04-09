package api

import (
	"net/http"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestRegisterGraphQLWhenAuthDisabled(t *testing.T) {
	e := echo.New()
	registerGraphQL(e, env.Environment{AuthMode: "none"})

	require.True(t, hasRoute(e, http.MethodGet, "/gql"))
}

func TestRegisterGraphQLSkippedWhenAuthEnabled(t *testing.T) {
	e := echo.New()
	registerGraphQL(e, env.Environment{AuthMode: "api-key"})

	require.False(t, hasRoute(e, http.MethodGet, "/gql"))
}

func hasRoute(e *echo.Echo, method, path string) bool {
	for _, route := range e.Router().Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
