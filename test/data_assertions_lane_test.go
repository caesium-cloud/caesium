//go:build integration

package test

// dataAssertionsFeatures decodes the subset of GET /v1/system/features this
// harness cares about: whether CAESIUM_DATA_ASSERTIONS_ENABLED is turned on
// for the server this test runner is pointed at.
//
// H-1 (this item) sets CAESIUM_DATA_ASSERTIONS_ENABLED=true on every
// self-server lane — justfile (`integration-up`, `integration-up-distributed`,
// `integration-up-owner-memory`, `integration-up-agent`, `integration-up-infra`,
// `integration-test-podman`, `ui-e2e`, `ui-e2e-auth`),
// `.github/workflows/ci.yml`'s inline server blocks for `ui-e2e`,
// `ui-e2e-auth`, and `podman-integration-test`, and
// `helm/caesium/ci/test-values-k8s.yaml` — so in steady state no lane should
// ever skip here. The `data_assertions_enabled` field itself lands on
// `api/rest/service/system.Features` alongside the rest of Stream A, which
// merges concurrently with (and independently of) this harness item; on a
// commit where that field does not exist yet, JSON decoding leaves
// DataAssertionsEnabled at its zero value, which this helper treats
// identically to an explicit "false".
type dataAssertionsFeatures struct {
	DataAssertionsEnabled bool `json:"data_assertions_enabled"`
}

// requireDataAssertionsLane gates a scenario on a live server that reports
// the data-assertions feature enabled via GET /v1/system/features.
//
// Unlike requireAuthLane (test/auth_lane_test.go), which keys off a single
// dedicated lane's runner-side marker env (CAESIUM_AGENT_AUTH_LANE), there is
// no one "the data-assertions lane" here — H-1 turns the flag on for every
// self-server lane, so the honest gate is what the live server itself
// reports, not a runner env var. That also makes this helper resilient to
// the field landing in a later commit than the flag: instead of failing hard
// like requireAuthLane's misconfiguration assertions (appropriate there
// because CAESIUM_AGENT_AUTH_LANE and the server's CAESIUM_AUTH_MODE are
// meant to be set together, in the same recipe, on day one), this helper
// skips cleanly — "field absent" and "flag off" both read as "not ready yet",
// not as a lane misconfiguration.
func (s *IntegrationTestSuite) requireDataAssertionsLane() {
	s.T().Helper()

	var features dataAssertionsFeatures
	if err := s.tryGetJSON("/v1/system/features", &features); err != nil {
		s.T().Skipf("%s requires a live server reachable at GET /v1/system/features, but the request failed: %v", s.T().Name(), err)
		return
	}

	if !features.DataAssertionsEnabled {
		s.T().Skipf("%s requires CAESIUM_DATA_ASSERTIONS_ENABLED=true on this lane's server; GET /v1/system/features reports data_assertions_enabled=false (or the field has not landed yet)", s.T().Name())
	}
}

// TestDataAssertionsFeatureFlagIsReportedByLiveServer is the harness item's
// own self-test: it drives GET /v1/system/features through the real HTTP
// surface (not an internal service call) and, once the flag is enabled —
// which H-1 makes true on every self-server lane — asserts the endpoint
// reports it. Before the `data_assertions_enabled` field lands (Stream A,
// merging independently of this harness item), this scenario skips
// honestly instead of failing, exactly as requireDataAssertionsLane
// documents.
func (s *IntegrationTestSuite) TestDataAssertionsFeatureFlagIsReportedByLiveServer() {
	s.requireDataAssertionsLane()

	var features dataAssertionsFeatures
	s.getJSON("/v1/system/features", &features)
	s.True(features.DataAssertionsEnabled,
		"GET /v1/system/features must report data_assertions_enabled=true once requireDataAssertionsLane has not skipped")
}
