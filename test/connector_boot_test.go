//go:build integration

package test

import (
	"fmt"
	"net/http"

	"github.com/caesium-cloud/caesium/pkg/env"
)

// TestAgentConnectorConfigBoots proves the auth lane started with the
// connector gate on. integration-up-agent mounts the documented example and
// caesium start refuses to serve when that file is rejected. The main
// integration lane does not set the gate and skips this scenario.
func (s *IntegrationTestSuite) TestAgentConnectorConfigBoots() {
	s.requireAuthLane()

	s.Require().NoError(env.Process())
	vars := env.Variables()
	s.Require().True(vars.ConnectorsEnabled, "the auth lane must set CAESIUM_CONNECTORS_ENABLED")
	s.Require().NotEmpty(vars.ConnectorsConfigFile)

	resp, err := s.doRequest(http.MethodGet, fmt.Sprintf("%v/health", s.caesiumURL), nil)
	s.Require().NoError(err)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	s.Equal(http.StatusOK, resp.StatusCode)
}
