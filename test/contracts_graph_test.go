//go:build integration

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestContractsGraphEndpointReportsInferredEdgeAndFeatureFlag() {
	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("integration-contract-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-contract-consumer-%d", suffix)

	dir, err := os.MkdirTemp("", "caesium-contract-graph-*")
	s.Require().NoError(err)
	defer os.RemoveAll(dir)

	producer := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  labels:
    team: data-platform
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: export
    image: alpine:3.23
    command: ["sh", "-c", "echo export"]
    outputSchema:
      type: object
      required: [row_count]
      properties:
        row_count:
          type: integer
`, producerAlias)

	consumer := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  labels:
    team: reporting
trigger:
  type: event
  configuration:
    events:
      - type: run_completed
        source: caesium
        filter:
          job_alias: %s
    paramMapping:
      upstream_rows: "$.tasks[0].output.row_count"
steps:
  - name: load
    image: alpine:3.23
    command: ["sh", "-c", "echo load"]
`, consumerAlias, producerAlias)

	s.Require().NoError(os.WriteFile(filepath.Join(dir, "producer.job.yaml"), []byte(strings.TrimSpace(s.injectEngine(producer))), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "consumer.job.yaml"), []byte(strings.TrimSpace(s.injectEngine(consumer))), 0o644))

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	var features struct {
		ContractEnforcementEnabled bool `json:"contract_enforcement_enabled"`
	}
	s.getJSON("/v1/system/features", &features)
	s.True(features.ContractEnforcementEnabled)

	var graph contractGraphResponse
	s.getJSON("/v1/contracts/graph", &graph)

	s.Contains(nodeAliases(graph.Nodes), producerAlias)
	s.Contains(nodeAliases(graph.Nodes), consumerAlias)

	var matched *contractGraphEdge
	for i := range graph.Edges {
		edge := &graph.Edges[i]
		if edge.From == "job:"+producerAlias && edge.To == "job:"+consumerAlias && edge.Class == "inferred" {
			matched = edge
			break
		}
	}
	s.Require().NotNil(matched, "expected inferred contract edge from %s to %s in %+v", producerAlias, consumerAlias, graph.Edges)
	s.Equal("compatible", matched.Verdict)
	s.Empty(matched.Findings)
}

type contractGraphResponse struct {
	Nodes []contractGraphNode `json:"nodes"`
	Edges []contractGraphEdge `json:"edges"`
}

type contractGraphNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Alias string `json:"alias"`
}

type contractGraphEdge struct {
	ID       string                 `json:"id"`
	From     string                 `json:"from"`
	To       string                 `json:"to"`
	Class    string                 `json:"class"`
	Verdict  string                 `json:"verdict"`
	Findings []contractGraphFinding `json:"findings"`
}

type contractGraphFinding struct {
	Verdict string `json:"verdict"`
	Path    string `json:"path"`
}

func nodeAliases(nodes []contractGraphNode) []string {
	aliases := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Kind == "job" {
			aliases = append(aliases, node.Alias)
		}
	}
	return aliases
}
