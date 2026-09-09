//go:build integration

package test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/caesium-cloud/caesium/internal/lineage"
)

// The hold UI joins by the registry's exact identity. A real declared
// producer/consumer must populate that graph without output-path heuristics.
func (s *IntegrationTestSuite) TestDataAssertionsDeclaredLineageImpact() {
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	producerAlias := fmt.Sprintf("declared-lineage-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("declared-lineage-consumer-%d", suffix)
	root := fmt.Sprintf("warehouse/raw-%d", suffix)
	downstream := fmt.Sprintf("warehouse/clean-%d", suffix)
	producer := s.applyHoldProducer(producerAlias, root, "manual", 2000)
	s.runProducer(producer)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata: {alias: %s}
trigger: {type: cron, configuration: {cron: "0 2 * * *"}}
steps:
  - name: transform
    image: alpine:3.23
    command: ["sh", "-c", "echo consumed"]
    datasets:
      consumes: [%s]
      produces: [{name: %s}]
`, consumerAlias, root, downstream)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	consumer := s.requireJobByAlias(consumerAlias)
	consumerRun := s.triggerRun(consumer.ID)
	s.Require().Equal("succeeded", s.awaitRun(consumer.ID, consumerRun, 90*time.Second).Status)
	path := "/v1/lineage/impact?" + url.Values{"namespace": {""}, "name": {root}}.Encode()
	var graph lineage.ImpactResult
	s.Require().Eventually(func() bool {
		graph = lineage.ImpactResult{}
		if err := s.tryGetJSON(path, &graph); err != nil {
			return false
		}
		for _, node := range graph.Downstream {
			if node.DatasetNamespace == "" && node.DatasetName == downstream && node.JobID.String() == consumer.ID {
				return node.ProducingStep == "transform"
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "declared consumer never appeared in the held dataset's graph")
	s.Equal("", graph.RootNamespace)
	s.Equal(root, graph.RootName)
	producer = s.applyHoldProducer(producerAlias, root, "manual", 12)
	s.runProducer(producer)
	holds := s.fetchDatasetOperatorHolds(root, "active")
	s.Require().Len(holds.Holds, 1)
	s.Equal(graph.RootNamespace, holds.Holds[0].Namespace)
	s.Equal(graph.RootName, holds.Holds[0].Name)
	blocked := s.triggerHeldRun(consumer.ID)
	s.Equal("skipped", blocked.Status)

	// Omission is distinct from the explicit empty registry namespace.
	resp, err := s.doJSONRequest(http.MethodGet, s.caesiumURL+"/v1/lineage/impact?name="+url.QueryEscape(root), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}
