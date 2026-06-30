//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestPriorityRunStartSurfacesAndCronDefault() {
	alias := fmt.Sprintf("e2e-priority-start-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(priorityJobManifest(alias, "low", `echo priority-start`))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	lowRunID := s.startRunREST(job.ID, "low", map[string]string{"lane": "low"})
	normalRunID := s.startRunREST(job.ID, "normal", map[string]string{"lane": "normal"})

	stdout, stderr, err := s.runCLISeparate(
		"run", "start",
		"--job-id", job.ID,
		"--priority", "high",
		"--params", "lane=high",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium run start failed:\n%s", stderr)
	// stdout must be the clean, parseable run id (asserted below); stderr legitimately
	// carries the live server's logs (the integration server runs at debug level), so we
	// assert only that no WARN/ERROR diagnostics surfaced — not that stderr is empty.
	s.NotContains(stderr, `"level":"warn"`, "caesium run start should surface no warnings when none are needed")
	s.NotContains(stderr, `"level":"error"`, "caesium run start should surface no errors")
	highRunID := strings.TrimSpace(stdout)
	s.Require().NotEmpty(highRunID)
	s.NotContains(highRunID, "\n")

	highRun := s.awaitRun(job.ID, highRunID, runTimeout)
	normalRun := s.awaitRun(job.ID, normalRunID, runTimeout)
	lowRun := s.awaitRun(job.ID, lowRunID, runTimeout)

	s.Equal(3, highRun.Priority, "CLI --priority must override the job metadata baseline")
	s.Equal(2, normalRun.Priority, "REST priority=normal must override the low job metadata baseline")
	s.Equal(1, lowRun.Priority)
	s.Require().NotEmpty(highRun.Tasks)
	s.Require().NotEmpty(normalRun.Tasks)
	s.Require().NotEmpty(lowRun.Tasks)
	s.Equal(3, highRun.Tasks[0].Priority)
	s.Equal(2, normalRun.Tasks[0].Priority)
	s.Equal(1, lowRun.Tasks[0].Priority)

	s.Run("distributed claimer claim order", func() {
		if !strings.EqualFold(strings.TrimSpace(os.Getenv("CAESIUM_EXECUTION_MODE")), "distributed") {
			s.T().Skip("distributed claimer claim-order e2e requires CAESIUM_EXECUTION_MODE=distributed")
		}

		fillerAlias := fmt.Sprintf("e2e-priority-filler-%d", time.Now().UnixNano())
		fillerDir := s.writeJobManifest(priorityJobManifest(fillerAlias, "high", `sleep 15`))
		defer os.RemoveAll(fillerDir)
		s.runCLI("job", "apply", "--path", fillerDir, "--server", s.caesiumURL)
		fillerJob := s.requireJobByAlias(fillerAlias)
		s.Require().NotNil(fillerJob)

		fillerRunID := s.triggerRun(fillerJob.ID)
		fillerRunning := s.awaitFirstTaskStatus(fillerJob.ID, fillerRunID, runTimeout, "running")
		s.Require().NotNil(fillerRunning.Tasks[0].StartedAt, "filler task must occupy the single worker slot before priority runs are queued")

		blockedLowRunID := s.startRunREST(job.ID, "low", map[string]string{"lane": "blocked-low"})
		blockedNormalRunID := s.startRunREST(job.ID, "normal", map[string]string{"lane": "blocked-normal"})
		blockedHighRunID := s.startRunREST(job.ID, "high", map[string]string{"lane": "blocked-high"})

		for label, runID := range map[string]string{
			"high":   blockedHighRunID,
			"normal": blockedNormalRunID,
			"low":    blockedLowRunID,
		} {
			pendingRun := s.awaitFirstTaskStatus(job.ID, runID, 10*time.Second, "pending")
			s.Nil(pendingRun.Tasks[0].StartedAt, "%s run must remain unclaimed while the filler occupies the worker slot", label)
		}

		fillerDone := s.awaitRun(fillerJob.ID, fillerRunID, runTimeout)
		s.Equal("succeeded", fillerDone.Status)

		blockedHighRun := s.awaitRun(job.ID, blockedHighRunID, runTimeout)
		blockedNormalRun := s.awaitRun(job.ID, blockedNormalRunID, runTimeout)
		blockedLowRun := s.awaitRun(job.ID, blockedLowRunID, runTimeout)
		s.Require().NotEmpty(blockedHighRun.Tasks)
		s.Require().NotNil(blockedHighRun.Tasks[0].StartedAt)
		s.Require().NotEmpty(blockedNormalRun.Tasks)
		s.Require().NotNil(blockedNormalRun.Tasks[0].StartedAt)
		s.Require().NotEmpty(blockedLowRun.Tasks)
		s.Require().NotNil(blockedLowRun.Tasks[0].StartedAt)

		drained := priorityDrainOrder(map[string]*runResponse{
			"high":   blockedHighRun,
			"normal": blockedNormalRun,
			"low":    blockedLowRun,
		})
		s.Equal([]string{"high", "normal", "low"}, drained)
		s.True(blockedHighRun.Tasks[0].StartedAt.Before(*blockedNormalRun.Tasks[0].StartedAt), "high priority run should start before normal priority run")
		s.True(blockedNormalRun.Tasks[0].StartedAt.Before(*blockedLowRun.Tasks[0].StartedAt), "normal priority run should start before low priority run")
	})

	cronAlias := fmt.Sprintf("e2e-priority-cron-%d", time.Now().UnixNano())
	cronDir := s.writeJobManifest(priorityJobManifest(cronAlias, "high", `echo cron-priority`))
	defer os.RemoveAll(cronDir)
	s.runCLI("job", "apply", "--path", cronDir, "--server", s.caesiumURL)
	cronJob := s.requireJobByAlias(cronAlias)

	cronRunID := s.triggerRun(cronJob.ID)
	cronRun := s.awaitRun(cronJob.ID, cronRunID, runTimeout)
	s.Equal(3, cronRun.Priority, "cron-configured job runs should inherit metadata.priority")
	s.Require().NotEmpty(cronRun.Tasks)
	s.Equal(3, cronRun.Tasks[0].Priority, "cron-configured job tasks should inherit metadata.priority")
}

func (s *IntegrationTestSuite) startRunREST(jobID, priority string, params map[string]string) string {
	s.T().Helper()
	body, err := json.Marshal(map[string]any{
		"priority": priority,
		"params":   params,
	})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var run runResponse
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&run))
	s.Require().NotEmpty(run.ID)
	return run.ID
}

func (s *IntegrationTestSuite) awaitFirstTaskStatus(jobID, runID string, timeout time.Duration, statuses ...string) runResponse {
	s.T().Helper()
	want := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		want[status] = struct{}{}
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s first task to reach one of %v", runID, statuses)
		}

		run := s.fetchRun(jobID, runID)
		if len(run.Tasks) > 0 {
			if _, ok := want[run.Tasks[0].Status]; ok {
				return run
			}
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func priorityDrainOrder(runs map[string]*runResponse) []string {
	type observed struct {
		label     string
		startedAt time.Time
	}
	observedRuns := make([]observed, 0, len(runs))
	for label, run := range runs {
		if run == nil || len(run.Tasks) == 0 || run.Tasks[0].StartedAt == nil {
			continue
		}
		observedRuns = append(observedRuns, observed{
			label:     label,
			startedAt: *run.Tasks[0].StartedAt,
		})
	}
	sort.Slice(observedRuns, func(i, j int) bool {
		return observedRuns[i].startedAt.Before(observedRuns[j].startedAt)
	})
	out := make([]string, 0, len(observedRuns))
	for _, run := range observedRuns {
		out = append(out, run.label)
	}
	return out
}

func priorityJobManifest(alias, priority, command string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  priority: %s
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
  - name: priority-step
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, priority, command)
}
