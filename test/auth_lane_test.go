//go:build integration

package test

import (
	"os"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/env"
)

// authLaneEnv is the marker the auth-enabled integration lane sets on the test
// runner container (justfile `integration-test-agent`, CI job
// `build-and-integration-test-agent-auth`). It is the single key every
// lane-scoped scenario routes through.
const authLaneEnv = "CAESIUM_AGENT_AUTH_LANE"

// onAuthLane reports whether this process is the auth-enabled lane's runner.
func onAuthLane() bool {
	return envBool(authLaneEnv)
}

// requireAuthLane gates a scenario on the auth-enabled remediation lane.
//
// Off the lane the scenario skips: the shared no-auth integration server does
// not mount the incident/agent routes at all.
//
// On the lane it does NOT skip — it *asserts* that the runner really carries
// the auth and remediation env. The lane used to set only CAESIUM_AGENT_AUTH_LANE
// on the runner while the server got CAESIUM_AUTH_MODE / CAESIUM_AGENT_REMEDIATION_ENABLED,
// so every scenario silently skipped on its own env guard and the lane finished
// green in 0.068s having executed nothing. Failing loudly here (plus the
// recipe's `--- PASS` count floor) is what keeps that from recurring.
func (s *IntegrationTestSuite) requireAuthLane() {
	s.T().Helper()

	if !onAuthLane() {
		s.T().Skipf("%s requires the auth-enabled remediation lane (just integration-test-agent)", s.T().Name())
	}

	s.Require().NoError(env.Process())
	vars := env.Variables()
	s.Require().Equal("api-key", vars.AuthMode,
		"%s=true but the runner has CAESIUM_AUTH_MODE=%q: the auth lane is misconfigured, not skippable", authLaneEnv, vars.AuthMode)
	s.Require().True(vars.AgentRemediationEnabled,
		"%s=true but the runner has CAESIUM_AGENT_REMEDIATION_ENABLED unset: the auth lane is misconfigured, not skippable", authLaneEnv)
}

// skipOnAuthLane is the inverse guard, for scenarios that assert a route is
// *unmounted* — true on every no-auth lane, false once the auth lane enables
// the feature that mounts it.
func (s *IntegrationTestSuite) skipOnAuthLane(reason string) {
	s.T().Helper()

	if onAuthLane() {
		s.T().Skipf("%s does not apply on the auth-enabled remediation lane: %s", s.T().Name(), reason)
	}
}

// envBool reports whether an environment variable carries a truthy value.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
