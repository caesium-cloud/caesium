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

func TestRequiredRoleBackfilledProtectedEndpoints(t *testing.T) {
	tests := []struct {
		method string
		path   string
		role   models.Role
	}{
		{"GET", "/v1/jobs/:id/runs/:id/why", models.RoleViewer},
		{"GET", "/v1/jobs/:id/runs/:id/receipt", models.RoleViewer},
		{"POST", "/v1/jobs/:id/runs/:id/receipt/verify", models.RoleViewer},
		{"GET", "/v1/jobs/:id/topology", models.RoleViewer},
		{"GET", "/v1/jobs/:id/topology/history", models.RoleViewer},
		{"GET", "/v1/lineage/impact", models.RoleViewer},
		{"GET", "/v1/stats/summary", models.RoleViewer},
		{"GET", "/v1/system/features", models.RoleViewer},
		{"GET", "/v1/system/nodes", models.RoleViewer},
		{"GET", "/v1/notifications/channels", models.RoleViewer},
		{"GET", "/v1/notifications/channels/:id", models.RoleViewer},
		{"GET", "/v1/notifications/policies", models.RoleViewer},
		{"GET", "/v1/notifications/policies/:id", models.RoleViewer},
		{"POST", "/v1/jobdefs/lint", models.RoleViewer},
		{"POST", "/v1/jobdefs/diff", models.RoleViewer},
		{"POST", "/v1/notifications/channels", models.RoleOperator},
		{"PATCH", "/v1/notifications/channels/:id", models.RoleOperator},
		{"DELETE", "/v1/notifications/channels/:id", models.RoleOperator},
		{"POST", "/v1/notifications/policies", models.RoleOperator},
		{"PATCH", "/v1/notifications/policies/:id", models.RoleOperator},
		{"DELETE", "/v1/notifications/policies/:id", models.RoleOperator},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			role, ok := RequiredRole(tt.method, tt.path)
			require.True(t, ok)
			require.Equal(t, tt.role, role)
		})
	}
}

func TestRequiredRoleUnknownEndpoint(t *testing.T) {
	_, ok := RequiredRole("GET", "/v1/unknown")
	require.False(t, ok)
}
