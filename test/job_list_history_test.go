//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

func (s *IntegrationTestSuite) TestJobsListIncludesLastRuns() {
	alias := fmt.Sprintf("integration-list-history-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "*/10 * * * *"
steps:
  - name: history
    image: alpine:3.23
    command: ["sh", "-c", "echo history"]
`, alias)

	tmpDir := s.writeJobManifest(manifest)
	defer os.RemoveAll(tmpDir)

	s.runCLI("job", "apply", "--path", tmpDir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	completed := s.awaitRun(job.ID, runID, runTimeout)

	type lastRunSummary struct {
		Status   string   `json:"status"`
		Duration *float64 `json:"duration"`
	}
	type listJobResponse struct {
		ID        string           `json:"id"`
		Alias     string           `json:"alias"`
		LatestRun *runResponse     `json:"latest_run"`
		LastRuns  []lastRunSummary `json:"last_runs"`
	}

	var jobs []listJobResponse
	s.getJSON("/v1/jobs?order_by=created_at desc", &jobs)

	var listed *listJobResponse
	for i := range jobs {
		if jobs[i].Alias == alias {
			listed = &jobs[i]
			break
		}
	}
	s.Require().NotNil(listed, "job %s not present in /v1/jobs", alias)
	s.Require().NotNil(listed.LatestRun, "latest_run missing for %s", alias)
	s.Equal(completed.ID, listed.LatestRun.ID)
	s.Equal(completed.Status, listed.LatestRun.Status)
	s.Require().NotEmpty(listed.LastRuns, "last_runs missing for %s", alias)
	s.LessOrEqual(len(listed.LastRuns), 10)

	latestHistory := listed.LastRuns[len(listed.LastRuns)-1]
	s.Equal(completed.Status, latestHistory.Status)
	s.Require().NotNil(latestHistory.Duration)
	s.GreaterOrEqual(*latestHistory.Duration, float64(0))
}
