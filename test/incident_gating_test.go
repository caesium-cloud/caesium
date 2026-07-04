//go:build integration

package test

import (
	"net/http"
)

// TestIncidentRoutesGatedOffByDefault drives the live server to prove the
// agent-in-the-loop D1/D2 routes are INERT when the feature is disabled — the
// integration server runs with CAESIUM_AGENT_REMEDIATION_ENABLED unset (and
// CAESIUM_AUTH_MODE=none), so bind.go never mounts the incident routes and they
// must 404. This is the "disabled-gate inertness" acceptance scenario, exercised
// against the real HTTP surface.
//
// The full live approve/reject flow requires the feature enabled AND an active
// auth mode together (the D1 master-gate precondition refuses remediation under
// AUTH_MODE=none). That combination needs a dedicated auth-enabled integration
// server lane, which is stood up by the plan's harness item (H-1); it cannot be
// enabled on this shared no-auth server without breaking every other scenario.
func (s *IntegrationTestSuite) TestIncidentRoutesGatedOffByDefault() {
	for _, path := range []string{
		"/v1/incidents",
		"/v1/incidents/00000000-0000-0000-0000-000000000000",
	} {
		resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
		s.Require().NoError(err)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		s.Equal(http.StatusNotFound, resp.StatusCode,
			"incident route %s must be unmounted (404) when remediation is disabled", path)
	}
}
