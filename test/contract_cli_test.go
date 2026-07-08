//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestContractGraphCLIJSONReportsInferredEdge() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-contract-cli-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-cli-consumer-%d", suffix)

	dir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	stdout, err := s.runCLIStdout("contract", "graph", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)

	var graph contractGraphResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &graph))
	s.Require().NotNil(contractCLIInferredEdge(graph, producer, consumer))
}

func (s *IntegrationTestSuite) TestJobLintServerJSONReportsContractFinding() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-contract-lint-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-lint-consumer-%d", suffix)

	dir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	brokenDir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"row_count"}),
	})
	defer os.RemoveAll(brokenDir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", brokenDir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err)

	var lintResp contractCLILintResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &lintResp))
	s.Require().NotNil(lintResp.Contracts)
	s.True(contractCLIFindingsNameConsumer(lintResp.Contracts.Breaking, consumer), "expected breaking finding naming %s in %+v", consumer, lintResp.Contracts.Breaking)
}

func (s *IntegrationTestSuite) TestContractCheckFailsOnBreakingLocalChange() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-contract-check-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-check-consumer-%d", suffix)

	dir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	brokenDir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"row_count"}),
	})
	defer os.RemoveAll(brokenDir)

	output, err := s.runCLIExpectError("contract", "check", "--path", brokenDir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(output, consumer)
	s.Contains(output, "customer_id")
}

type contractCLILintResponse struct {
	Contracts *contractCLIContractSummary `json:"contracts,omitempty"`
}

type contractCLIContractSummary struct {
	Breaking []contractCLIContractFinding `json:"breaking"`
	Warnings []contractCLIContractFinding `json:"warnings"`
	Edges    int                          `json:"edges"`
}

type contractCLIContractFinding struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Detail  string `json:"detail"`
	Verdict string `json:"verdict"`
}

func contractCLIInferredEdge(graph contractGraphResponse, producer, consumer string) *contractGraphEdge {
	for i := range graph.Edges {
		edge := &graph.Edges[i]
		if edge.From == "job:"+producer && edge.To == "job:"+consumer && edge.Class == "inferred" {
			return edge
		}
	}
	return nil
}

func contractCLIFindingsNameConsumer(findings []contractCLIContractFinding, consumer string) bool {
	for _, finding := range findings {
		if strings.Contains(finding.To, consumer) || strings.Contains(finding.Detail, consumer) {
			return true
		}
	}
	return false
}
