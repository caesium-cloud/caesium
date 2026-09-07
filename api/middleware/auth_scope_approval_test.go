package middleware_test

import (
	"context"
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

// approvalRoute builds the RouteInfo + path values for the tier-3 approve route.
func approvalRoute(incidentID, approvalID uuid.UUID) (*echo.RouteInfo, echo.PathValues) {
	return &echo.RouteInfo{
		Path:   "/v1/incidents/:id/approvals/:approval_id/approve",
		Method: http.MethodPost,
	}, echo.PathValues{
		{Name: "id", Value: incidentID.String()},
		{Name: "approval_id", Value: approvalID.String()},
	}
}

// TestMiddlewareApprovalRouteRejectsAgentToken is the D1 security proof: a token
// carrying the per-session agent claim (Stream C1) is rejected OUTRIGHT on the
// approval route, even at operator role — an agent may never approve its own
// action. The rejection is by scope claim, independent of RBAC role.
//
// Read it together with TestMiddlewareApprovalRouteDeniesMintedAgentKeyAtRBAC
// below: this case constructs the credential the SCOPE arm exists for (an agent
// claim on an operator-or-above key), which is NOT what MintAgentSessionKey
// issues today. Both layers are asserted so neither can be mistaken for the
// other.
func TestMiddlewareApprovalRouteRejectsAgentToken(t *testing.T) {
	db, svc, auditor, limiter, _ := setupAuth(t)

	// Mint an operator key, then stamp its scope with a per-session agent claim
	// (as the incident manager does when it launches an agent session).
	resp, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "agent"})
	require.NoError(t, err)
	incidentID := uuid.New()
	agentScope := []byte(`{"agent":{"incident_id":"` + incidentID.String() + `"}}`)
	require.NoError(t, db.Model(&models.APIKey{}).Where("id = ?", resp.Key.ID).Update("scope", agentScope).Error)

	approvalID := uuid.New()
	route, pv := approvalRoute(incidentID, approvalID)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/incidents/"+incidentID.String()+"/approvals/"+approvalID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Plaintext)

	_, err = callMiddleware(t, svc, auditor, limiter, req, route, pv, nil)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, authmw.ApprovalAgentTokenDenyMessage, he.Message)
}

// TestMiddlewareApprovalRouteDeniesMintedAgentKeyAtRBAC pins what a REAL
// agent-session credential — one produced by auth.MintAgentSessionKey, not
// hand-stamped — actually meets on the wire.
//
// auth.Auth checks RBAC before authorizeScope, and the mint issues at
// auth.AgentSessionKeyRole (RoleRunner) while the approval routes require
// RoleOperator, so the role gate answers FIRST with the generic
// "insufficient permissions" and ApprovalAgentTokenDenyMessage is never seen by
// today's agent tokens. That does not make the scope arm dead code — it is the
// deny that survives if an agent claim ever rides an operator-or-above key —
// but the boundary must be documented as it really behaves, not as the
// hand-built fixture above suggests.
//
// The assertion here is the OUTCOME (403, request never reaches the handler),
// which is the security property. It deliberately does not lower a role or
// widen a scope to route the request through the other arm.
func TestMiddlewareApprovalRouteDeniesMintedAgentKeyAtRBAC(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)

	incidentID := uuid.New()
	key := mintAgentKey(t, svc, incidentID, []string{"alpha"})

	approvalID := uuid.New()
	route, pv := approvalRoute(incidentID, approvalID)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/incidents/"+incidentID.String()+"/approvals/"+approvalID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	called := false
	_, err := callMiddleware(t, svc, auditor, limiter, req, route, pv, func(c *echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})
	require.Error(t, err)
	require.False(t, called, "an agent-session token must never reach the approval handler")

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	// The role gate, not the scope arm, is what a minted agent key meets.
	require.Equal(t, "insufficient permissions", he.Message)
	require.Less(t, models.RoleLevel(auth.AgentSessionKeyRole), models.RoleLevel(models.RoleOperator),
		"this test's premise is that a minted agent key sits BELOW the approval routes' required role; "+
			"if that ever changes, ApprovalAgentTokenDenyMessage becomes the first-line deny and this expectation must flip")
}

// TestMiddlewareApprovalRouteAllowsUnscopedOperator confirms the rejection is
// narrow: a legitimate unscoped operator (a human) passes the scope check and
// reaches the handler.
func TestMiddlewareApprovalRouteAllowsUnscopedOperator(t *testing.T) {
	_, svc, auditor, limiter, _ := setupAuth(t)
	key := createKey(t, svc, models.RoleOperator, nil)

	incidentID, approvalID := uuid.New(), uuid.New()
	route, pv := approvalRoute(incidentID, approvalID)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/incidents/"+incidentID.String()+"/approvals/"+approvalID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	rec, err := callMiddleware(t, svc, auditor, limiter, req, route, pv,
		func(c *echo.Context) error { return c.String(http.StatusOK, "ok") })
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}
