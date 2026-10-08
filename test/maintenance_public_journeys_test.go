//go:build integration

package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

// These journeys never seed catalog rows or invoke a scheduler/executor directly.
// They characterize the instrumented server through its operator surfaces.
func (s *IntegrationTestSuite) TestIncidentBundleFromRealFailure() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey, "the intended auth lane must carry its operator key")
	cli, _ := s.maintenanceDockerPrerequisite()
	alias := "maintenance-bundle-" + uuid.NewString()
	marker := "bundle:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	jobID := s.maintenanceApply(alias, maintenanceManifest(alias, "0 0 31 2 *", "  runTimeout: 30s\n", "echo "+marker+"; exit 17", ""))
	runID := s.maintenanceStart(jobID)
	var taskID string
	s.T().Cleanup(func() {
		if taskID == "" {
			taskID = s.maintenanceTaskIDContext(context.WithoutCancel(s.T().Context()), jobID)
		}
		s.maintenanceRunCleanup(cli, jobID, runID, taskID, marker, map[int]string{})
	})
	taskID = s.maintenanceTaskID(jobID)
	run := s.maintenanceAwaitRun(jobID, runID, 60*time.Second)
	s.Require().Equal("failed", run.Status)
	partition := s.maintenancePartition(jobID, runID, taskID)
	s.Require().Equal("failed", partition.Status)
	s.Require().NotNil(partition.ExitCode)
	s.Require().Equal(17, *partition.ExitCode)
	s.Require().NotNil(partition.CompletedAt)

	var incidentID string
	s.maintenancePoll(30*time.Second, func() bool {
		var wire struct {
			Incidents []struct {
				ID       string `json:"id"`
				JobID    string `json:"job_id"`
				RunID    string `json:"run_id"`
				TaskID   string `json:"task_id"`
				TaskName string `json:"task_name"`
			} `json:"incidents"`
		}
		s.maintenanceJSON("/v1/incidents?job_id="+jobID, &wire)
		for _, row := range wire.Incidents {
			if row.JobID == jobID && row.RunID == runID && row.TaskName == "gate" {
				s.Require().Equal(taskID, row.TaskID)
				incidentID = row.ID
				return true
			}
		}
		return false
	}, "the real failure must reach the incident subscriber")

	var before struct {
		Actions []json.RawMessage `json:"actions"`
	}
	s.maintenanceJSON("/v1/incidents/"+incidentID, &before)
	var bundle incident.Bundle
	s.maintenanceJSON("/v1/agent/incidents/"+incidentID+"/bundle", &bundle)
	assertBundle := func(b incident.Bundle) {
		s.Require().Equal(incidentID, b.Incident.ID.String())
		s.Require().Equal(jobID, b.Incident.JobID.String())
		s.Require().NotNil(b.Incident.RunID)
		s.Require().Equal(runID, b.Incident.RunID.String())
		s.Require().Equal("gate", b.Incident.TaskName)
		s.Require().Equal(alias, b.Job.Alias)
		s.Require().Len(b.Job.Tasks, 1)
		s.Require().Equal("gate", b.Job.Tasks[0].Name)
		s.Require().NotNil(b.Failure.ExitCode)
		s.Require().Equal(17, *b.Failure.ExitCode)
		s.Require().Contains(b.Failure.LogTail, marker)
		s.Require().True(b.Failure.LogTailScrubbed)
		s.Require().NotEmpty(b.Failure.Result)
		s.Require().NotEmpty(b.Classification.Class)
		s.Require().True(b.LineageImpact.Frozen)
		s.Require().Contains(b.LineageImpact.AllowedJobs, alias)
		s.Require().NotEmpty(b.Playbook)
		s.Require().Len(b.RunHistory, 1)
		s.Require().Equal(runID, b.RunHistory[0].ID.String())
		s.Require().Equal("failed", b.RunHistory[0].Status)
		s.Require().NotNil(b.RunHistory[0].CompletedAt)
	}
	assertBundle(bundle)
	payload := []byte(`{"jsonrpc":"2.0","id":"bundle","method":"tools/call","params":{"name":"get_bundle","arguments":{}}}`)
	status, body := s.maintenanceHTTP(http.MethodPost, "/v1/agent/incidents/"+incidentID+"/mcp", payload)
	s.Require().Equal(http.StatusOK, status)
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  struct {
			StructuredContent incident.Bundle   `json:"structuredContent"`
			Content           []json.RawMessage `json:"content"`
		} `json:"result"`
	}
	s.Require().NoError(json.Unmarshal(body, &rpc))
	s.Require().Equal("2.0", rpc.JSONRPC)
	s.Require().Equal("bundle", rpc.ID)
	s.Require().Empty(rpc.Error)
	s.Require().NotEmpty(rpc.Result.Content)
	assertBundle(rpc.Result.StructuredContent)
	status, _ = s.maintenanceHTTP(http.MethodGet, "/v1/agent/incidents/"+uuid.NewString()+"/bundle", nil)
	s.Require().Equal(http.StatusNotFound, status)
	var after struct {
		Actions []json.RawMessage `json:"actions"`
	}
	s.maintenanceJSON("/v1/incidents/"+incidentID, &after)
	s.Require().Equal(before.Actions, after.Actions, "bundle reads must not propose or execute remediation")
	s.Require().Len(s.maintenanceRuns(jobID), 1)
}

func (s *IntegrationTestSuite) TestFreshnessCronTickSkipsFreshOutput() {
	var features freshnessFeatures
	s.maintenanceJSON("/v1/system/features", &features)
	if !features.FreshnessEnabled {
		s.maintenanceMissingPrerequisite("CAESIUM_FRESHNESS_ENABLED=true on the server")
	}
	s.Require().True(features.FreshnessEnabled)
	cli, _ := s.maintenanceDockerPrerequisite()
	alias := "maintenance-cron-" + uuid.NewString()
	dataset := "maintenance.cron." + uuid.NewString()
	watermark := uuid.NewString()
	metadata := "  runTimeout: 30s\n  datasets:\n    skipWhenFresh: true\n"
	step := fmt.Sprintf("    datasets:\n      produces:\n        - name: %s\n          freshness: 10m\n          maxStaleness: 20m\n          watermark:\n            key: wm\n", dataset)
	command := fmt.Sprintf("echo '##caesium::output {\"wm\":\"%s\"}'", watermark)
	registeredAfter := time.Now().UTC().Truncate(time.Minute)
	jobID := s.maintenanceApply(alias, maintenanceManifest(alias, "* * * * *", metadata, command, step))
	// Trigger discovery polls once per minute. Registration just after that
	// poll can need two minutes before the first real cron boundary, followed
	// by another minute for the fresh-output skip.
	journeyDeadline := time.Now().Add(225 * time.Second)
	var taskID string
	s.T().Cleanup(func() {
		status, _ := s.maintenanceHTTPContext(context.WithoutCancel(s.T().Context()), http.MethodPut, "/v1/jobs/"+jobID+"/pause", nil)
		if !s.Equal(http.StatusOK, status) {
			return
		}
		// Pause prevents future ticks; it does not cancel an already admitted run.
		// Read every exact public admission and join its bounded production budget.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.T().Context()), 70*time.Second)
		defer cancel()
		status, body := s.maintenanceHTTPContext(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/runs", nil)
		var runs []maintenanceRun
		if !s.Equal(http.StatusOK, status) || !s.NoError(json.Unmarshal(body, &runs)) {
			return
		}
		if len(runs) > 0 && taskID == "" {
			taskID = s.maintenanceTaskIDContext(ctx, jobID)
		}
		for _, run := range runs {
			s.maintenanceRunCleanup(cli, jobID, run.ID, taskID, watermark, map[int]string{})
		}
	})
	taskID = s.maintenanceTaskID(jobID)
	var runID string
	s.maintenancePoll(135*time.Second, func() bool {
		runs := s.maintenanceRuns(jobID)
		if len(runs) == 0 {
			return false
		}
		s.Require().Len(runs, 1)
		runID = runs[0].ID
		return true
	}, "the first real minute cron tick must admit a run")
	run := s.maintenanceAwaitRun(jobID, runID, 30*time.Second)
	s.Require().Equal("succeeded", run.Status)
	s.Require().Len(run.Tasks, 1)
	s.Require().Equal("succeeded", run.Tasks[0].Status)
	s.Require().Equal(watermark, run.Tasks[0].Output["wm"])
	logicalDate, err := time.Parse(time.RFC3339, run.Params["logical_date"])
	s.Require().NoError(err)
	s.Require().Equal(0, logicalDate.Second())
	s.Require().Equal(0, logicalDate.Nanosecond())
	s.Require().True(logicalDate.After(registeredAfter), "the first admission must be a new actual cron boundary, not catchup")
	s.Require().NotContains(run.Params, "_derived_from_dataset")
	var state struct {
		State struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			Watermark string `json:"watermark"`
			LastRunID string `json:"last_run_id"`
		} `json:"state"`
	}
	s.maintenancePoll(20*time.Second, func() bool {
		s.maintenanceJSON("/v1/datasets/_/"+dataset, &state)
		return state.State.Status == "fresh" && state.State.Watermark == watermark
	}, "the executed output must persist a fresh dataset")
	s.maintenancePoll(min(75*time.Second, time.Until(journeyDeadline)), func() bool {
		var audit struct {
			Derivations []struct {
				Decision  string    `json:"decision"`
				Reason    string    `json:"reason"`
				CreatedAt time.Time `json:"created_at"`
				RunID     string    `json:"run_id"`
			} `json:"derivations"`
		}
		s.maintenanceJSON("/v1/datasets/_/"+dataset+"/derivations?limit=100", &audit)
		for _, row := range audit.Derivations {
			if row.Decision == "skipped_fresh" && strings.HasPrefix(row.Reason, "cron tick skipped: ") && !row.CreatedAt.Before(logicalDate.Add(time.Minute)) {
				s.Require().Empty(row.RunID)
				return true
			}
		}
		return false
	}, "the next actual minute tick must record the scheduler-specific fresh-output skip")
	runs := s.maintenanceRuns(jobID)
	s.Require().Len(runs, 1, "a fresh cron tick must not admit a second run")
	s.Require().Equal(runID, runs[0].ID)
	s.maintenanceJSON("/v1/datasets/_/"+dataset, &state)
	s.Require().Equal(dataset, state.State.Name)
	s.Require().Equal("fresh", state.State.Status)
	s.Require().Equal(watermark, state.State.Watermark)
	s.Require().Equal(runID, state.State.LastRunID)
	s.Require().True(time.Now().Before(journeyDeadline), "trigger discovery and both real cron ticks must complete within 225s")
}

func (s *IntegrationTestSuite) TestAutomaticRetryDelayConstantAndBackoff() {
	// Both configured policies must take the real positive-delay execution
	// branch. Native lower bounds cannot prove wall-clock constant/backoff parity:
	// container teardown/start overhead has no independent bounded upper limit.
	cli, imageID := s.maintenanceDockerPrerequisite()
	for _, backoff := range []bool{false, true} {
		s.Run(fmt.Sprintf("backoff-%t", backoff), func() {
			alias := "maintenance-retry-" + uuid.NewString()
			// A local non-success engine result alone is terminal by design.
			// Both executors validate captured output before that result decision;
			// fail-mode schema errors take the supported automatic retry path.
			// Keep exit 17 as independent native evidence on every real attempt.
			command := "echo " + alias + "-started; sleep 3; echo '##caesium::output {\"rows\":\"not-a-number\"}'; echo " + alias + "-finished; exit 17"
			step := fmt.Sprintf("    retries: 2\n    retryDelay: 2s\n    retryBackoff: %t\n    outputSchema:\n      type: object\n      properties:\n        rows: {type: integer}\n      required: [rows]\n", backoff)
			jobID := s.maintenanceApply(alias, maintenanceManifest(alias, "0 0 31 2 *", "  runTimeout: 60s\n  schemaValidation: fail\n", command, step))
			taskID := s.maintenanceTaskID(jobID)
			ctx, cancel := context.WithTimeout(s.T().Context(), 90*time.Second)
			defer cancel()
			filter := filters.NewArgs(filters.Arg("type", "container"), filters.Arg("event", "start"), filters.Arg("event", "die"))
			messages, failures := cli.Events(ctx, events.ListOptions{Since: fmt.Sprint(time.Now().Unix()), Filters: filter})
			runID := s.maintenanceStart(jobID)
			ids := map[int]string{}
			observed := map[string]map[events.Action]events.Message{}
			concreteID := ""
			s.T().Cleanup(func() { s.maintenanceRunCleanup(cli, jobID, runID, taskID, alias, ids) })
			var terminal maintenanceRun
			poll := time.NewTicker(100 * time.Millisecond)
			defer poll.Stop()
			for {
				select {
				case message, open := <-messages:
					s.Require().True(open, "native event stream must remain open through all retries")
					if observed[message.Actor.ID] == nil {
						observed[message.Actor.ID] = map[events.Action]events.Message{}
					}
					observed[message.Actor.ID][message.Action] = message
					s.Require().LessOrEqual(len(observed), 4096, "bounded native event observation")
				case err := <-failures:
					s.Require().NoError(err)
					s.Require().FailNow("native event stream ended before retry proof")
				case <-ctx.Done():
					s.Require().NoError(ctx.Err(), "automatic retries did not finish within their bound")
				case <-poll.C:
					row := s.maintenancePartition(jobID, runID, taskID)
					if row.TaskRunID == "" {
						continue
					}
					if concreteID == "" {
						concreteID = row.TaskRunID
					}
					s.Require().Equal(concreteID, row.TaskRunID, "automatic retries reuse the exact durable task row")
					if row.Status == "running" && row.RuntimeID != "" && ids[row.Attempt] == "" {
						inspect, inspectErr := cli.ContainerInspect(ctx, row.RuntimeID)
						if errdefs.IsNotFound(inspectErr) {
							continue
						}
						s.Require().NoError(inspectErr)
						s.Require().Equal(imageID, inspect.Image)
						s.Require().Equal([]string{"sh", "-c", command}, []string(inspect.Config.Cmd))
						if inspect.State.Running {
							ids[row.Attempt] = inspect.ID
							s.maintenanceLiveAttemptLogs(jobID, runID, taskID, concreteID, alias)
						}
					}
					s.maintenanceJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &terminal)
					s.Require().Equal(runID, terminal.ID)
					if terminal.Status != "failed" {
						continue
					}
					row = s.maintenancePartition(jobID, runID, taskID)
					s.Require().Equal(concreteID, row.TaskRunID)
					s.Require().Equal("failed", row.Status)
					s.Require().Equal(3, row.Attempt)
					s.Require().Equal(fmt.Sprintf("task %s output violates declared schema: 1 violation(s)", taskID), row.Error)
					s.Require().NotNil(row.ExitCode)
					s.Require().Equal(17, *row.ExitCode)
					s.Require().NotNil(row.CompletedAt)
					// Wait for the final die event as well as durable completion.
					if len(ids) == 3 && observed[ids[3]][events.ActionDie].TimeNano > 0 {
						goto completed
					}
				}
			}
		completed:
			s.Require().NotNil(terminal.CompletedAt)
			s.Require().Len(ids, 3, "each retry must have an actual native Running witness")
			for attempt := 1; attempt <= 3; attempt++ {
				start, die := observed[ids[attempt]][events.ActionStart], observed[ids[attempt]][events.ActionDie]
				s.Require().Positive(start.TimeNano)
				s.Require().Greater(die.TimeNano, start.TimeNano)
				s.Require().Equal("17", die.Actor.Attributes["exitCode"])
				_, inspectErr := cli.ContainerInspect(ctx, ids[attempt])
				s.Require().True(errdefs.IsNotFound(inspectErr), "each exact runtime must be absent after terminal completion")
				if attempt > 1 {
					s.Require().NotEqual(ids[attempt-1], ids[attempt])
					want := 2 * time.Second
					if backoff && attempt == 3 {
						want = 4 * time.Second
					}
					gap := time.Duration(start.TimeNano - observed[ids[attempt-1]][events.ActionDie].TimeNano)
					s.Require().GreaterOrEqual(gap, want-10*time.Millisecond, "the configured policy must reach positive automatic delay before the next native attempt")
					s.T().Logf("automatic retry backoff=%t attempt=%d native gap=%s", backoff, attempt, gap)
				}
			}
			status, logs := s.maintenanceHTTP(http.MethodGet, fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s", jobID, runID, taskID), nil)
			s.Require().Equal(http.StatusOK, status)
			s.Require().Contains(string(logs), alias+"-finished")
			s.Require().Len(s.maintenanceRuns(jobID), 1, "automatic retry must not create replacement job runs")
		})
	}
}

// Native image inspection is a prerequisite, never permission to pull. The
// intended collector lanes require a top-level PASS, so every capability SKIP
// below still refuses their named coverage admission.
func (s *IntegrationTestSuite) maintenanceDockerPrerequisite() (*client.Client, string) {
	s.T().Helper()
	if s.engineType != "" && s.engineType != "docker" {
		s.maintenanceMissingPrerequisite("preloaded Docker tasks/native observation; engine=" + s.engineType)
	}
	cli := s.dockerClient()
	s.T().Cleanup(func() { s.NoError(cli.Close()) })
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	defer cancel()
	image, err := cli.ImageInspect(ctx, "alpine:3.23")
	if errdefs.IsNotFound(err) {
		s.maintenanceMissingPrerequisite("preloaded alpine:3.23; no implicit pull")
	}
	s.Require().NoError(err, "unexpected Docker prerequisite observation failure")
	return cli, image.ID
}

func (s *IntegrationTestSuite) maintenanceMissingPrerequisite(reason string) {
	s.T().Helper()
	if envBool("CAESIUM_MAINTENANCE_JOURNEYS_REQUIRED") {
		s.Require().FailNow("required maintenance journey prerequisite missing", reason)
	}
	s.T().Skipf("%s requires %s", s.T().Name(), reason)
}

// All HTTP observations require a complete bounded body, including read/close
// errors. Failures are not interpreted as negative product evidence.
func (s *IntegrationTestSuite) maintenanceHTTP(method, path string, body []byte) (int, []byte) {
	s.T().Helper()
	return s.maintenanceHTTPContext(s.T().Context(), method, path, body)
}

func (s *IntegrationTestSuite) maintenanceHTTPContext(parent context.Context, method, path string, body []byte) (int, []byte) {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.caesiumURL+path, bytes.NewReader(body))
	s.Require().NoError(err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.authorize(req)
	res, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	data, readErr := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	closeErr := res.Body.Close()
	s.Require().NoError(readErr)
	s.Require().NoError(closeErr)
	s.Require().LessOrEqual(len(data), 2<<20, "HTTP body exceeds observation cap")
	return res.StatusCode, data
}

func (s *IntegrationTestSuite) maintenanceJSON(path string, out any) {
	s.T().Helper()
	status, body := s.maintenanceHTTP(http.MethodGet, path, nil)
	s.Require().Equal(http.StatusOK, status, "public observation %s", path)
	s.Require().NoError(json.Unmarshal(body, out))
}

func (s *IntegrationTestSuite) maintenancePoll(bound time.Duration, observe func() bool, reason string) {
	s.T().Helper()
	deadline := time.Now().Add(bound)
	for {
		if observe() {
			return
		}
		s.Require().True(time.Now().Before(deadline), reason)
		select {
		case <-s.T().Context().Done():
			s.Require().NoError(s.T().Context().Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func maintenanceManifest(alias, cron, metadata, command, step string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false
%strigger:
  type: cron
  configuration:
    cron: %q
    timezone: UTC
    catchup: false
steps:
  - name: gate
    image: alpine:3.23
    command: ["sh", "-c", %q]
%s`, alias, metadata, cron, command, step)
}

func (s *IntegrationTestSuite) maintenanceApply(alias, manifest string) string {
	s.T().Helper()
	dir := s.writeJobManifest(manifest)
	s.T().Cleanup(func() { s.NoError(os.RemoveAll(dir)) })
	_, err := s.runCLIStdout("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err)
	var jobs []jobSummary
	s.maintenanceJSON("/v1/jobs?order_by=created_at%20desc", &jobs)
	for _, job := range jobs {
		if job.Alias == alias {
			return job.ID
		}
	}
	s.T().Fatalf("public job apply did not create alias %s", alias)
	return ""
}

func (s *IntegrationTestSuite) maintenanceStart(jobID string) string {
	s.T().Helper()
	stdout, err := s.runCLIStdout("run", "start", "--job-id", jobID, "--server", s.caesiumURL)
	s.Require().NoError(err)
	id, err := uuid.Parse(strings.TrimSpace(stdout))
	s.Require().NoError(err)
	s.Require().Equal(id.String()+"\n", stdout, "run start stdout must contain only its public run identity")
	return id.String()
}

type maintenanceRun struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	CompletedAt *time.Time        `json:"completed_at"`
	Params      map[string]string `json:"params"`
	Tasks       []struct {
		Status string            `json:"status"`
		Output map[string]string `json:"output"`
	} `json:"tasks"`
}

func (s *IntegrationTestSuite) maintenanceRuns(jobID string) []maintenanceRun {
	var runs []maintenanceRun
	s.maintenanceJSON("/v1/jobs/"+jobID+"/runs", &runs)
	return runs
}

func (s *IntegrationTestSuite) maintenanceAwaitRun(jobID, runID string, bound time.Duration) maintenanceRun {
	var run maintenanceRun
	s.maintenancePoll(bound, func() bool {
		s.maintenanceJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run)
		s.Require().Equal(runID, run.ID)
		return run.Status == "failed" || run.Status == "succeeded"
	}, "the real admitted run must complete")
	return run
}

func (s *IntegrationTestSuite) maintenanceTaskID(jobID string) string {
	return s.maintenanceTaskIDContext(s.T().Context(), jobID)
}

func (s *IntegrationTestSuite) maintenanceTaskIDContext(ctx context.Context, jobID string) string {
	var tasks []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	status, body := s.maintenanceHTTPContext(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/tasks", nil)
	s.Require().Equal(http.StatusOK, status)
	s.Require().NoError(json.Unmarshal(body, &tasks))
	s.Require().Len(tasks, 1)
	s.Require().Equal("gate", tasks[0].Name)
	_, err := uuid.Parse(tasks[0].ID)
	s.Require().NoError(err)
	return tasks[0].ID
}

type maintenanceTask struct {
	TaskRunID   string     `json:"task_run_id"`
	RuntimeID   string     `json:"runtime_id"`
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	Attempt     int        `json:"attempt"`
	ExitCode    *int       `json:"exit_code"`
	CompletedAt *time.Time `json:"completed_at"`
}

func (s *IntegrationTestSuite) maintenancePartition(jobID, runID, taskID string) maintenanceTask {
	var page struct {
		Total      int               `json:"total"`
		Partitions []maintenanceTask `json:"partitions"`
	}
	s.maintenanceJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions?limit=2", jobID, runID, taskID), &page)
	if page.Total == 0 {
		s.Require().Empty(page.Partitions)
		return maintenanceTask{} // admitted, but not yet materialized by the executor
	}
	s.Require().Equal(1, page.Total)
	s.Require().Len(page.Partitions, 1)
	_, err := uuid.Parse(page.Partitions[0].TaskRunID)
	s.Require().NoError(err)
	return page.Partitions[0]
}

// A live stream is intentionally sampled through the two harmless markers,
// then closed. A timeout, transport failure or incomplete prefix is a failure.
func (s *IntegrationTestSuite) maintenanceLiveAttemptLogs(jobID, runID, taskID, concreteID, marker string) {
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	defer cancel()
	path := fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s&task_run_id=%s", jobID, runID, taskID, concreteID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	s.authorize(req)
	res, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { s.NoError(res.Body.Close()) }()
	s.Require().Equal(http.StatusOK, res.StatusCode)
	scanner := bufio.NewScanner(io.LimitReader(res.Body, (2<<20)+1))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	started, finished := false, false
	for scanner.Scan() {
		started = started || strings.Contains(scanner.Text(), marker+"-started")
		finished = finished || strings.Contains(scanner.Text(), marker+"-finished")
		if started && finished {
			break
		}
	}
	s.Require().NoError(scanner.Err())
	s.Require().True(started && finished, "the witnessed native attempt must emit both actual live log markers")
}

func (s *IntegrationTestSuite) maintenanceRunCleanup(cli *client.Client, jobID, runID, taskID, marker string, ids map[int]string) {
	// There is no public whole-run cancel route. Keep the configured production run
	// deadline authoritative and join durable completion before cleanup. On a
	// failed assertion, also discover the exact current row through the public
	// endpoint, so an unwitnessed later attempt cannot escape the ownership ledger.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.T().Context()), 70*time.Second)
	defer cancel()
	for {
		var page struct {
			Partitions []maintenanceTask `json:"partitions"`
		}
		status, body := s.maintenanceHTTPContext(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions?limit=2", jobID, runID, taskID), nil)
		if !s.Equal(http.StatusOK, status) || !s.NoError(json.Unmarshal(body, &page)) {
			return
		}
		if !s.LessOrEqual(len(page.Partitions), 1) {
			return
		}
		if len(page.Partitions) == 1 && page.Partitions[0].RuntimeID != "" {
			row := page.Partitions[0]
			inspect, err := cli.ContainerInspect(ctx, row.RuntimeID)
			if errdefs.IsNotFound(err) {
				ids[row.Attempt] = row.RuntimeID // exact public runtime is already absent
			} else if s.NoError(err) && s.NotNil(inspect.Config) && s.Contains(strings.Join(inspect.Config.Cmd, " "), marker) {
				ids[row.Attempt] = inspect.ID
			}
		}
		var run maintenanceRun
		status, body = s.maintenanceHTTPContext(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), nil)
		if !s.Equal(http.StatusOK, status) || !s.NoError(json.Unmarshal(body, &run)) || !s.Equal(runID, run.ID) {
			return
		}
		if run.Status == "failed" || run.Status == "succeeded" {
			s.NotNil(run.CompletedAt, "owned terminal run must have durable completion")
			break
		}
		if !s.NoError(ctx.Err(), "owned admitted run must finish before cleanup") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, id := range ids {
		inspect, err := cli.ContainerInspect(ctx, id)
		if errdefs.IsNotFound(err) {
			continue
		}
		if !s.NoError(err) {
			continue
		}
		if !s.NotNil(inspect.Config) || !s.Contains(strings.Join(inspect.Config.Cmd, " "), marker) {
			continue
		}
		s.NoError(cli.ContainerRemove(ctx, inspect.ID, container.RemoveOptions{Force: true}))
		_, err = cli.ContainerInspect(ctx, inspect.ID)
		s.True(errdefs.IsNotFound(err), "owned admitted runtime must be absent after cleanup")
	}
}
