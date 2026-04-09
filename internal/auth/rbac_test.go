package auth

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestHasRole(t *testing.T) {
	tests := []struct {
		keyRole  models.Role
		required models.Role
		allowed  bool
	}{
		{models.RoleAdmin, models.RoleAdmin, true},
		{models.RoleAdmin, models.RoleViewer, true},
		{models.RoleOperator, models.RoleRunner, true},
		{models.RoleViewer, models.RoleViewer, true},
		{models.RoleViewer, models.RoleRunner, false},
		{models.RoleRunner, models.RoleOperator, false},
		{models.RoleRunner, models.RoleAdmin, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.keyRole)+"_vs_"+string(tt.required), func(t *testing.T) {
			require.Equal(t, tt.allowed, HasRole(tt.keyRole, tt.required))
		})
	}
}

func TestCheckScopeNilAllowsAll(t *testing.T) {
	require.True(t, CheckScope(nil, "any-job"))
}

func TestCheckScopeEmptyAllowsAll(t *testing.T) {
	scope, _ := json.Marshal(models.KeyScope{})
	require.True(t, CheckScope(scope, "any-job"))
}

func TestCheckScopeAllowsListedJob(t *testing.T) {
	scope, _ := json.Marshal(models.KeyScope{Jobs: []string{"etl-daily", "etl-hourly"}})
	require.True(t, CheckScope(scope, "etl-daily"))
	require.True(t, CheckScope(scope, "etl-hourly"))
	require.False(t, CheckScope(scope, "other-job"))
}

func TestCheckScopeMalformedDenies(t *testing.T) {
	require.False(t, CheckScope([]byte("{invalid"), "any-job"))
}

func TestRequiredRoleKnownEndpoints(t *testing.T) {
	role, ok := RequiredRole("GET", "/metrics")
	require.True(t, ok)
	require.Equal(t, models.RoleViewer, role)

	role, ok = RequiredRole("GET", "/v1/jobs")
	require.True(t, ok)
	require.Equal(t, models.RoleViewer, role)

	role, ok = RequiredRole("POST", "/v1/jobs/:id/run")
	require.True(t, ok)
	require.Equal(t, models.RoleRunner, role)

	role, ok = RequiredRole("POST", "/v1/triggers")
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, role)

	role, ok = RequiredRole("PATCH", "/v1/triggers/:id")
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, role)

	role, ok = RequiredRole("POST", "/v1/triggers/:id/fire")
	require.True(t, ok)
	require.Equal(t, models.RoleRunner, role)

	role, ok = RequiredRole("GET", "/v1/auth/keys")
	require.True(t, ok)
	require.Equal(t, models.RoleAdmin, role)
}

func TestRequiredRoleUnknownEndpoint(t *testing.T) {
	_, ok := RequiredRole("GET", "/v1/unknown")
	require.False(t, ok)
}
