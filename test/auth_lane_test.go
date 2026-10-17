//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
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

	// The auth lane serves the two-namespace policy fixture (plan item H-1).
	// Only the coverage journey's api-key lane runs without a policy file, on
	// purpose, and it says so; anywhere else a missing fixture is a broken
	// lane, never a skip.
	if authLaneWithoutPolicy() {
		return
	}
	path := strings.TrimSpace(os.Getenv(authPolicyPathEnv))
	s.Require().NotEmpty(path,
		"%s=true but the runner has no %s: the auth lane must serve %s (justfile integration-up-agent / integration-test-agent)", authLaneEnv, authPolicyPathEnv, policyFixtureFile)
	served, err := os.ReadFile(path)
	s.Require().NoError(err, "read the auth lane's served policy file")
	parsed, err := policy.Parse(served)
	s.Require().NoError(err, "the auth lane's served policy file must parse")
	for _, namespace := range policyFixtureNamespaces {
		s.Require().Contains(parsed.Namespaces, namespace,
			"the auth lane's served policy must declare %q (it is seeded from %s)", namespace, policyFixtureFile)
	}
}

// authLanePolicyEnv marks the one auth lane that intentionally serves no
// policy file (scripts/coverage-journeys.sh sets it to "none").
const authLanePolicyEnv = "CAESIUM_IT_AUTH_POLICY"

// policyFixtureEnv is set by the no-auth runner (justfile integration-test)
// whose server mounts the policy fixture. Lanes without a policy file
// (podman, kind/helm, coverage journeys) leave it unset.
const policyFixtureEnv = "CAESIUM_IT_POLICY_FIXTURE"

// policyFixtureFile is the fixture every justfile integration server serves.
const policyFixtureFile = "test/testdata/policy/two-namespaces.yaml"

// policyFixtureNamespaces are the namespaces the fixture declares besides
// default.
var policyFixtureNamespaces = []string{"marketing", "finance"}

func authLaneWithoutPolicy() bool {
	return strings.TrimSpace(os.Getenv(authLanePolicyEnv)) == "none"
}

// requirePolicyFixture gates a scenario on a server that serves the
// two-namespace policy fixture, and fails (never skips) when the lane says it
// does but the live server does not enforce it:
//
//   - on the auth lane, requireAuthLane already failed unless the fixture is
//     served (the coverage journey's no-policy lane skips here);
//   - on the no-auth lane, the runner carries CAESIUM_IT_POLICY_FIXTURE;
//     lanes that never mount a policy file (podman, kind) skip;
//
// and either way server lint must reject an undeclared namespace.
func (s *IntegrationTestSuite) requirePolicyFixture() {
	s.T().Helper()

	switch {
	case onAuthLane():
		s.requireAuthLane()
		if authLaneWithoutPolicy() {
			s.T().Skipf("%s needs the policy fixture; this auth lane (%s=none) serves no policy file", s.T().Name(), authLanePolicyEnv)
		}
	case strings.TrimSpace(os.Getenv(policyFixtureEnv)) == "":
		s.T().Skipf("%s needs a server serving %s (just integration-test / integration-test-agent); this lane runs without a policy file", s.T().Name(), policyFixtureFile)
	}

	probe := fmt.Sprintf("h1-undeclared-probe-%d", time.Now().UnixNano()%1_000_000)
	resp := s.serverLint(s.authAPIKey, namespaceProbeDefinition("h1-fixture-probe", probe))
	found := false
	for _, msg := range resp.Errors {
		if strings.Contains(msg.Message, fmt.Sprintf("%q", probe)) && strings.Contains(msg.Message, "not declared") {
			found = true
		}
	}
	s.Require().True(found, "%s is set but the server does not reject undeclared namespace %q: it is not serving %s (lint: %+v)",
		policyFixtureEnv, probe, policyFixtureFile, resp)
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
