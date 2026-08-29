//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// TestJobLintServerScopesBreakingContractsToLintedJobs is the regression for
// issue #362. `caesium job lint --server` posts to /v1/jobdefs/lint, which
// derives the contract graph over the posted definitions unioned with EVERY job
// already applied on the server. Before the fix the breaking-contract gate ran
// over that whole union, so a single breaking producer/consumer pair anywhere on
// a shared server failed every later `--server` lint, whatever was being linted.
//
// The scenario leaves a real breaking pair persisted on the server (applied with
// --allow-breaking so apply enforcement lets it through), then drives the CLI
// three times to pin both halves of the scope:
//
//  1. linting an unrelated job exits 0 with no breaking findings, and
//  2. linting the broken producer still fails, and
//  3. linting the broken pair's consumer — a job the persisted producer breaks —
//     still fails, which is the "plus their direct consumers" half of the scope.
//
// Assertions read the --json body from clean stdout (runCLIStdout), so a pass
// means the SERVER scoped the findings, not that the CLI happened to exit 0.
func (s *IntegrationTestSuite) TestJobLintServerScopesBreakingContractsToLintedJobs() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-lint-scope-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-lint-scope-consumer-%d", suffix)
	unrelated := fmt.Sprintf("integration-lint-scope-unrelated-%d", suffix)

	healthyDir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(healthyDir)
	s.runCLI("job", "apply", "--path", healthyDir, "--server", s.caesiumURL)

	// Drop customer_id from the producer's outputSchema while the consumer's
	// paramMapping still reads it: a durable breaking pair on the shared
	// server. The acknowledgement only gets it past APPLY enforcement; lint
	// derives findings from the graph and never consults acks, so the pair
	// stays breaking for the rest of the run.
	brokenDir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"row_count"}),
	})
	defer os.RemoveAll(brokenDir)
	_, stderr, err := s.runCLISeparate(
		"job", "apply",
		"--path", brokenDir,
		"--server", s.caesiumURL,
		"--allow-breaking", "dataset="+producer+".output.customer_id",
		"--reason", "integration lint scope fixture",
	)
	s.Require().NoError(err, stderr)

	// 1. An unrelated job must not inherit that break.
	unrelatedDir := s.writeContractManifests(map[string]string{
		unrelated: contractProducerManifest(unrelated, []string{"row_count"}),
	})
	defer os.RemoveAll(unrelatedDir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", unrelatedDir, "--server", s.caesiumURL, "--json")
	s.Require().NoError(err, "unrelated job lint must not fail on a breaking pair it has no edge to: %s", stdout)

	var unrelatedResp contractCLILintResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &unrelatedResp))
	s.Require().NotNil(unrelatedResp.Contracts, "contract enforcement must be enabled on the integration server")
	s.Empty(unrelatedResp.Contracts.Breaking, "expected no in-scope breaking findings, got %+v", unrelatedResp.Contracts.Breaking)
	s.False(contractCLIFindingsNameConsumer(unrelatedResp.Contracts.Warnings, consumer),
		"the unrelated lint must not report findings about %s either, got %+v", consumer, unrelatedResp.Contracts.Warnings)

	// 2. The producer that owns the break still fails its own lint.
	stdout, err = s.runCLIStdout("job", "lint", "--path", brokenDir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "linting the broken producer must still fail: %s", stdout)

	var producerResp contractCLILintResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &producerResp))
	s.Require().NotNil(producerResp.Contracts)
	s.True(contractCLIFindingsNameConsumer(producerResp.Contracts.Breaking, consumer),
		"expected a breaking finding naming %s in %+v", consumer, producerResp.Contracts.Breaking)

	// 3. The direct consumer of that persisted producer still fails too.
	consumerDir := s.writeContractManifests(map[string]string{
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(consumerDir)

	stdout, err = s.runCLIStdout("job", "lint", "--path", consumerDir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "linting the broken pair's consumer must still fail: %s", stdout)

	var consumerResp contractCLILintResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &consumerResp))
	s.Require().NotNil(consumerResp.Contracts)
	s.True(contractCLIFindingsNameConsumer(consumerResp.Contracts.Breaking, consumer),
		"expected a breaking finding naming %s in %+v", consumer, consumerResp.Contracts.Breaking)
}
