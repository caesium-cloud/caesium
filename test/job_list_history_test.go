//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

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

	listed := s.awaitListedJobRun(alias, completed.ID, completed.Status, runTimeout)
	s.Equal(completed.ID, listed.LatestRun.ID)
	s.Equal(completed.Status, listed.LatestRun.Status)
	s.Require().NotEmpty(listed.LastRuns, "last_runs missing for %s", alias)
	s.LessOrEqual(len(listed.LastRuns), 10)

	latestHistory := listed.LastRuns[len(listed.LastRuns)-1]
	s.Equal(completed.Status, latestHistory.Status)
	s.Require().NotNil(latestHistory.Duration)
	s.GreaterOrEqual(*latestHistory.Duration, float64(0))
}

func (s *IntegrationTestSuite) awaitListedJobRun(alias, runID, status string, timeout time.Duration) *listJobResponse {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	lastObservation := "not checked yet"
	var lastErr error

	for {
		var jobs []listJobResponse
		if err := s.tryGetJSON("/v1/jobs", &jobs); err != nil {
			lastErr = err
		} else {
			lastErr = nil
			found := false
			for i := range jobs {
				if jobs[i].Alias != alias {
					continue
				}
				found = true
				listed := jobs[i]
				if listed.LatestRun == nil {
					lastObservation = "job found with nil latest_run"
					break
				}

				historyStatus := "<empty>"
				durationReady := false
				if len(listed.LastRuns) > 0 {
					latestHistory := listed.LastRuns[len(listed.LastRuns)-1]
					historyStatus = latestHistory.Status
					durationReady = latestHistory.Duration != nil
				}
				lastObservation = fmt.Sprintf("latest_run=%s/%s last_history=%s duration_ready=%t",
					listed.LatestRun.ID, listed.LatestRun.Status, historyStatus, durationReady)

				if listed.LatestRun.ID == runID &&
					listed.LatestRun.Status == status &&
					len(listed.LastRuns) > 0 {
					latestHistory := listed.LastRuns[len(listed.LastRuns)-1]
					if latestHistory.Status == status && latestHistory.Duration != nil {
						return &listed
					}
				}
				break
			}
			if !found {
				lastObservation = "job not present"
			}
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				s.T().Fatalf("timeout waiting for /v1/jobs history for %s run %s: last_error=%v", alias, runID, lastErr)
			}
			s.T().Fatalf("timeout waiting for /v1/jobs history for %s run %s to converge: %s", alias, runID, lastObservation)
		}

		time.Sleep(250 * time.Millisecond)
	}
}
