//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

func (s *IntegrationTestSuite) TestRateLimitRequeuesUntilWindowRollover() {
	alias := fmt.Sprintf("integration-rate-limit-%d", time.Now().UnixNano())
	resource := fmt.Sprintf("shared-api-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  maxParallelTasks: 2
  rateLimits:
    - resource: %s
      limit: 1
      window: 1m
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: fanout
    image: alpine:3.23
    command: ["sh", "-c", "echo fanout"]
    next: [limited-a, limited-b]
  - name: limited-a
    image: alpine:3.23
    command: ["sh", "-c", "echo limited-a"]
    dependsOn: fanout
    rateLimit:
      resource: %s
      units: 1
  - name: limited-b
    image: alpine:3.23
    command: ["sh", "-c", "echo limited-b"]
    dependsOn: fanout
    rateLimit:
      resource: %s
      units: 1
`, alias, resource, resource, resource)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	nameByID := s.taskNameMap(job.ID)
	s.waitForRateLimitWindowHeadroom(20 * time.Second)
	runID := s.triggerRun(job.ID)

	s.Require().Eventually(func() bool {
		var run runResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &run); err != nil {
			return false
		}
		statuses := statusesByName(nameByID, &run)
		if statuses["fanout"] != "succeeded" {
			return false
		}
		succeeded := 0
		pending := 0
		for _, name := range []string{"limited-a", "limited-b"} {
			switch statuses[name] {
			case "succeeded":
				succeeded++
			case "pending":
				pending++
			}
		}
		return run.Status == "running" && succeeded == 1 && pending == 1
	}, 30*time.Second, 500*time.Millisecond, "one contending task should be requeued as pending in the first window")

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)
	statuses := statusesByName(nameByID, run)
	s.Equal("succeeded", statuses["limited-a"])
	s.Equal("succeeded", statuses["limited-b"])
}

func (s *IntegrationTestSuite) taskNameMap(jobID string) map[string]string {
	s.T().Helper()

	var tasks []struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)

	nameByID := make(map[string]string, len(tasks))
	for _, task := range tasks {
		nameByID[task.ID] = task.Name
	}
	return nameByID
}

func statusesByName(nameByID map[string]string, run *runResponse) map[string]string {
	statuses := make(map[string]string, len(run.Tasks))
	for _, task := range run.Tasks {
		name := nameByID[task.ID]
		if name == "" {
			name = task.ID
		}
		statuses[name] = task.Status
	}
	return statuses
}

func (s *IntegrationTestSuite) waitForRateLimitWindowHeadroom(headroom time.Duration) {
	s.T().Helper()

	for {
		now := time.Now().UTC()
		remaining := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
		if remaining >= headroom {
			return
		}
		time.Sleep(remaining + time.Second)
	}
}
