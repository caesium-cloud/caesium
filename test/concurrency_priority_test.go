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
	s.Empty(strings.TrimSpace(stderr), "caesium run start must keep diagnostics off stdout/stderr when no warning is needed")
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
			s.T().Skip("distributed claimer claim-order e2e is wired by Stream H-1 (CAESIUM_EXECUTION_MODE=distributed on integration-up); the priority sort itself is unit-proven in internal/worker/claimer_test.go")
		}

		drained := priorityDrainOrder(map[string]*runResponse{
			"high":   highRun,
			"normal": normalRun,
			"low":    lowRun,
		})
		s.Equal([]string{"high", "normal", "low"}, drained)
	})

	cronAlias := fmt.Sprintf("e2e-priority-cron-%d", time.Now().UnixNano())
	cronDir := s.writeJobManifest(priorityJobManifest(cronAlias, "high", `echo cron-priority`))
	defer os.RemoveAll(cronDir)
	s.runCLI("job", "apply", "--path", cronDir, "--server", s.caesiumURL)
	cronJob := s.requireJobByAlias(cronAlias)

	var cronRun *runResponse
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(cronJob.ID)
		if len(runs) == 0 {
			return false
		}
		for i := range runs {
			run := s.fetchRun(cronJob.ID, runs[i].ID)
			if len(run.Tasks) == 0 {
				continue
			}
			if run.Priority == 3 && run.Tasks[0].Priority == 3 {
				cronRun = &run
				return true
			}
		}
		return false
	}, 75*time.Second, time.Second, "cron-triggered tasks should inherit metadata.priority")
	s.Require().NotNil(cronRun)
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
