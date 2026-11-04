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

// TestLineageImpactNodesCarryProducingRunAndRunDatasets drives a real run and
// asserts, over the live HTTP surface, that
//   - /v1/lineage/impact nodes name the run that produced the dataset (run_id),
//   - GET /v1/jobs/:id/runs/:run_id/datasets lists the run's persisted dataset
//     identities, and
//   - each identity is a valid entry point: querying impact from the run's
//     input dataset reaches the same run.
//
// Requires CAESIUM_OPEN_LINEAGE_ENABLED=true (set in `just integration-up`).
func (s *IntegrationTestSuite) TestLineageImpactNodesCarryProducingRunAndRunDatasets() {
	alias := fmt.Sprintf("e2e-lineage-run-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    cache: false
    outputSchema: { type: object, properties: { rows: { type: string } } }
    command: ["sh","-c","echo '##caesium::output {\"rows\": \"1\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    cache: false
    inputSchema: { extract: { properties: { rows: { type: string } } } }
    outputSchema: { type: object, properties: { clean: { type: string } } }
    command: ["sh","-c","echo '##caesium::output {\"clean\": \"y\"}'"]
    dependsOn: extract
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, 90*time.Second).Status)

	rootName := alias + ".extract.output"
	wantName := alias + ".transform.output"

	// Persistence is asynchronous (event subscriber): poll the impact endpoint.
	var graph lineage.ImpactResult
	impactPath := "/v1/lineage/impact?" + url.Values{"namespace": {"caesium"}, "name": {rootName}}.Encode()
	s.Require().Eventually(func() bool {
		graph = lineage.ImpactResult{}
		if err := s.tryGetJSON(impactPath, &graph); err != nil {
			return false
		}
		for _, n := range graph.Downstream {
			if n.DatasetName == wantName {
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "impact never reached %s", wantName)
	for _, n := range graph.Downstream {
		if n.DatasetName == wantName {
			s.Equal(runID, n.RunID, "impact node must name the run that produced the dataset")
			s.Equal(job.ID, n.JobID.String())
		}
	}

	// The run lists its persisted dataset identities.
	var datasets lineage.RunDatasets
	datasetsPath := fmt.Sprintf("/v1/jobs/%s/runs/%s/datasets", job.ID, runID)
	s.Require().Eventually(func() bool {
		datasets = lineage.RunDatasets{}
		if err := s.tryGetJSON(datasetsPath, &datasets); err != nil {
			return false
		}
		seen := map[string]bool{}
		for _, d := range datasets.Datasets {
			seen[d.Direction+":"+d.Name] = true
		}
		return seen["output:"+rootName] && seen["input:"+rootName] && seen["output:"+wantName]
	}, 30*time.Second, 500*time.Millisecond, "run never listed its lineage datasets: %+v", datasets)
	s.Equal(runID, datasets.RunID.String())
	s.Equal(job.ID, datasets.JobID.String())
	for _, d := range datasets.Datasets {
		s.Equal("caesium", d.Namespace)
		s.NotEmpty(d.TaskName)
	}

	// Run the job again: repeated runs collapse to ONE node per (dataset, job)
	// whose run_id is the latest run, never a historical one.
	secondRun := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, secondRun, 90*time.Second).Status)
	s.Require().Eventually(func() bool {
		graph = lineage.ImpactResult{}
		if err := s.tryGetJSON(impactPath, &graph); err != nil {
			return false
		}
		for _, n := range graph.Downstream {
			if n.DatasetName == wantName {
				return n.RunID == secondRun
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "impact node never moved to the second run (want %s): %+v", secondRun, graph.Downstream)
	count := 0
	for _, n := range graph.Downstream {
		if n.DatasetName == wantName && n.JobID.String() == job.ID {
			count++
		}
	}
	s.Equal(1, count, "repeated runs must collapse to a single node: %+v", graph.Downstream)

	// A run id that does not belong to the job is a 404; a non-UUID is a 400.
	s.assertGetStatus(fmt.Sprintf("/v1/jobs/%s/runs/%s/datasets", job.ID, "00000000-0000-0000-0000-000000000001"), http.StatusNotFound)
	s.assertGetStatus(fmt.Sprintf("/v1/jobs/%s/runs/not-a-uuid/datasets", job.ID), http.StatusBadRequest)
}

func (s *IntegrationTestSuite) assertGetStatus(path string, want int) {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(want, resp.StatusCode, path)
}
