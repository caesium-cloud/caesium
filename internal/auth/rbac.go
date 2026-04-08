package auth

import "github.com/caesium-cloud/caesium/internal/models"

// RequiredRole returns the minimum role needed for a given HTTP method + path.
func RequiredRole(method, path string) (models.Role, bool) {
	role, ok := endpointPolicy[method+" "+path]
	return role, ok
}

// HasRole returns true if the key's role is at or above the required level.
func HasRole(keyRole, required models.Role) bool {
	return models.RoleLevel(keyRole) >= models.RoleLevel(required)
}

// CheckScope validates whether the key is allowed to act on the given job alias.
// A nil/empty scope means unrestricted access.
func CheckScope(scopeJSON []byte, jobAlias string) bool {
	scope, err := DecodeScope(scopeJSON)
	if err != nil {
		return false // malformed scope denies access
	}
	if scope == nil {
		return true
	}
	return containsScopedJob(scope.Jobs, jobAlias)
}

// endpointPolicy maps "METHOD /path-pattern" to the minimum required role.
// Parametric segments are replaced with `:id` by the middleware normaliser.
var endpointPolicy = map[string]models.Role{
	// Public — no auth required (handled by skip list, not here)

	// Viewer
	"GET /v1/jobs":                   models.RoleViewer,
	"GET /v1/jobs/:id":               models.RoleViewer,
	"GET /v1/jobs/:id/tasks":         models.RoleViewer,
	"GET /v1/jobs/:id/dag":           models.RoleViewer,
	"GET /v1/jobs/:id/runs":          models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id":      models.RoleViewer,
	"GET /v1/jobs/:id/runs/:id/logs": models.RoleViewer,
	"GET /v1/jobs/:id/cache":         models.RoleViewer,
	"GET /v1/jobs/:id/backfills":     models.RoleViewer,
	"GET /v1/jobs/:id/backfills/:id": models.RoleViewer,
	"GET /v1/events":                 models.RoleViewer,
	"GET /v1/stats":                  models.RoleViewer,
	"GET /v1/triggers":               models.RoleViewer,
	"GET /v1/triggers/:id":           models.RoleViewer,
	"GET /v1/atoms":                  models.RoleViewer,
	"GET /v1/atoms/:id":              models.RoleViewer,
	"GET /v1/nodes/:id/workers":      models.RoleViewer,

	// Runner
	"POST /v1/jobs/:id/run":                      models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/retry":           models.RoleRunner,
	"POST /v1/jobs/:id/runs/:id/callbacks/retry": models.RoleRunner,
	"POST /v1/jobs/:id/backfill":                 models.RoleRunner,
	"POST /v1/triggers/:id/fire":                 models.RoleRunner,

	// Operator
	"POST /v1/jobs":                         models.RoleOperator,
	"DELETE /v1/jobs/:id":                   models.RoleOperator,
	"PUT /v1/jobs/:id/pause":                models.RoleOperator,
	"PUT /v1/jobs/:id/unpause":              models.RoleOperator,
	"POST /v1/jobdefs/apply":                models.RoleOperator,
	"POST /v1/cache/prune":                  models.RoleOperator,
	"DELETE /v1/jobs/:id/cache":             models.RoleOperator,
	"DELETE /v1/jobs/:id/cache/:id":         models.RoleOperator,
	"PUT /v1/jobs/:id/backfills/:id/cancel": models.RoleOperator,
	"POST /v1/triggers":                     models.RoleOperator,
	"PATCH /v1/triggers/:id":                models.RoleOperator,
	"POST /v1/atoms":                        models.RoleOperator,
	"DELETE /v1/atoms/:id":                  models.RoleOperator,

	// Admin
	"PUT /v1/logs/level":            models.RoleAdmin,
	"GET /v1/logs/level":            models.RoleAdmin,
	"GET /v1/logs/stream":           models.RoleAdmin,
	"GET /v1/database/schema":       models.RoleAdmin,
	"POST /v1/database/query":       models.RoleAdmin,
	"GET /v1/auth/keys":             models.RoleAdmin,
	"POST /v1/auth/keys":            models.RoleAdmin,
	"POST /v1/auth/keys/:id/revoke": models.RoleAdmin,
	"POST /v1/auth/keys/:id/rotate": models.RoleAdmin,
	"GET /v1/auth/audit":            models.RoleAdmin,
}
