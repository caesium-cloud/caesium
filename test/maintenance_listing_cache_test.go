//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/google/uuid"
)

// These scenarios use the candidate CLI, public REST data and exact native
// runtimes. They neither seed catalog rows nor call execution/parser helpers.
func (s *IntegrationTestSuite) TestCacheCLIListsInvalidatesAndPrunes() {
	// The cache CLI currently has no operator-auth flag/header. Its intended
	// collector admission is the auth-none lane, where a skip is not a PASS.
	authMode := strings.ToLower(strings.TrimSpace(os.Getenv("CAESIUM_AUTH_MODE")))
	if onAuthLane() || s.authAPIKey != "" || (authMode != "" && authMode != "none") {
		s.T().Skip("cache CLI management requires the auth-none integration lane")
	}
	cli, _ := s.maintenanceDockerPrerequisite()
	alias := "maintenance-cache-cli-" + uuid.NewString()
	command := func(step string) string {
		return fmt.Sprintf("echo '##caesium::output {\"token\":\"%s-%s\"}'", alias, step)
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  runTimeout: 30s
  cache: {enabled: true, ttl: never, pinDigests: false}
trigger:
  type: cron
  configuration: {cron: "0 0 31 2 *", timezone: UTC, catchup: false}
steps:
  - name: step-a
    image: alpine:3.23
    command: ["sh", "-c", %q]
  - name: step-b
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, command("a"), command("b"))
	jobID := s.maintenanceApply(alias, manifest)
	var tasks []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	s.maintenanceJSON("/v1/jobs/"+jobID+"/tasks", &tasks)
	s.Require().Len(tasks, 2)
	taskIDs := map[string]string{}
	for _, task := range tasks {
		_, err := uuid.Parse(task.ID)
		s.Require().NoError(err)
		s.Require().Contains([]string{"step-a", "step-b"}, task.Name)
		s.Require().NotContains(taskIDs, task.Name)
		taskIDs[task.Name] = task.ID
	}
	runID := s.maintenanceStart(jobID)
	s.T().Cleanup(func() {
		for _, task := range tasks {
			s.maintenanceRunCleanup(cli, jobID, runID, task.ID, alias, map[int]string{})
		}
	})
	run := s.maintenanceAwaitRun(jobID, runID, 60*time.Second)
	s.Require().Equal("succeeded", run.Status)
	s.Require().NotNil(run.CompletedAt)
	s.Require().Len(run.Tasks, 2)
	outputs := map[string]bool{}
	for _, task := range run.Tasks {
		s.Require().Equal("succeeded", task.Status)
		outputs[task.Output["token"]] = true
	}
	s.Require().Equal(map[string]bool{alias + "-a": true, alias + "-b": true}, outputs)

	var listed listingCacheResponse
	stdout, stderr, err := s.listingCacheCLI("list", "--job-id", jobID, "--server", s.caesiumURL)
	s.Require().NoError(err, "%s", stderr)
	s.Require().NoError(json.Unmarshal([]byte(stdout), &listed), "cache list must emit clean JSON on stdout")
	s.Require().Len(listed.Entries, 2)
	var httpListed listingCacheResponse
	s.maintenanceJSON("/v1/jobs/"+jobID+"/cache", &httpListed)
	s.Require().ElementsMatch(httpListed.Entries, listed.Entries)
	byTask := map[string]listingCacheEntry{}
	for _, entry := range listed.Entries {
		s.Require().Contains(taskIDs, entry.TaskName)
		s.Require().NotContains(byTask, entry.TaskName)
		s.Require().NotEmpty(entry.Hash)
		s.Require().Equal("success", entry.Result)
		s.Require().Equal(runID, entry.RunID)
		s.Require().False(entry.CreatedAt.IsZero())
		s.Require().Nil(entry.ExpiresAt, "ttl: never must keep the management observations stable")
		partition := s.listingPartition(jobID, runID, taskIDs[entry.TaskName])
		s.Require().Equal("succeeded", partition.Status)
		s.Require().Equal("success", partition.Result)
		s.Require().NotNil(partition.CompletedAt)
		// Local execution stores the catalog task ID in cache TaskRunID; the
		// partition listing exposes the concrete TaskRun row ID.
		s.Require().Equal(taskIDs[entry.TaskName], entry.TaskRunID)
		s.Require().NotEqual(entry.TaskRunID, partition.TaskRunID)
		byTask[entry.TaskName] = entry
	}
	stdout, stderr, err = s.listingCacheCLI("invalidate", "--job-id", jobID, "--task", "step-a", "--server", s.caesiumURL)
	s.Require().NoError(err, "%s", stderr)
	s.Require().Empty(stdout)
	taskAck := fmt.Sprintf("Cache invalidated for task %q in job %s", "step-a", jobID)
	s.requireCacheCLIStderrAck(stderr, func(line string) bool { return line == taskAck }, "task invalidation")
	s.maintenanceJSON("/v1/jobs/"+jobID+"/cache", &httpListed)
	s.Require().Equal([]listingCacheEntry{byTask["step-b"]}, httpListed.Entries)
	stdout, stderr, err = s.listingCacheCLI("invalidate", "--job-id", jobID, "--server", s.caesiumURL)
	s.Require().NoError(err, "%s", stderr)
	s.Require().Empty(stdout)
	jobAck := "Cache invalidated for job " + jobID
	s.requireCacheCLIStderrAck(stderr, func(line string) bool { return line == jobAck }, "job invalidation")
	s.maintenanceJSON("/v1/jobs/"+jobID+"/cache", &httpListed)
	s.Require().Empty(httpListed.Entries)

	stdout, stderr, err = s.listingCacheCLI("prune", "--server", s.caesiumURL)
	s.Require().NoError(err, "%s", stderr)
	s.Require().Empty(stdout)
	pruneAck := regexp.MustCompile(`^Pruned [0-9]+ expired cache entries$`)
	s.requireCacheCLIStderrAck(stderr, pruneAck.MatchString, "prune")
	// Prune's count is global and the list route hides expired entries. This
	// asserts the real command/response contract, not an invented exact delete.
	s.maintenanceJSON("/v1/jobs/"+jobID+"/cache", &httpListed)
	s.Require().Empty(httpListed.Entries)
	s.maintenanceJSON("/v1/jobs/"+jobID+"/runs/"+runID, &run)
	s.Require().Equal("succeeded", run.Status)
	s.Require().NotNil(run.CompletedAt)
	s.Require().Len(s.maintenanceRuns(jobID), 1)
}

func (s *IntegrationTestSuite) TestNativeTaskSIGTERMResultClassification() {
	cli, imageID := s.maintenanceDockerPrerequisite()
	alias := "maintenance-exit-" + uuid.NewString()
	// PID 1 may ignore default TERM. Install a real TERM handler and
	// signal the shell itself; exit 99 refuses an ignored signal. This
	// characterizes process exit, not server cancellation or a run deadline.
	command := "trap 'echo " + alias + "-term-received; echo " + alias + "-finished; exit 143' TERM; echo " + alias + "-started; sleep 3; kill -TERM $$; exit 99"
	jobID := s.maintenanceApply(alias, maintenanceManifest(alias, "0 0 31 2 *", "  runTimeout: 30s\n", command, "    retries: 0\n"))
	taskID := s.maintenanceTaskID(jobID)
	ctx, cancel := context.WithTimeout(s.T().Context(), 60*time.Second)
	defer cancel()
	filter := filters.NewArgs(filters.Arg("type", "container"), filters.Arg("event", "start"), filters.Arg("event", "die"))
	messages, failures := cli.Events(ctx, events.ListOptions{Since: fmt.Sprint(time.Now().Unix()), Filters: filter})
	runID := s.maintenanceStart(jobID)
	ids := map[int]string{}
	s.T().Cleanup(func() { s.maintenanceRunCleanup(cli, jobID, runID, taskID, alias, ids) })
	observed := map[string]map[events.Action]events.Message{}
	var concreteID string
	var terminal maintenanceRun
	for {
		select {
		case message, open := <-messages:
			s.Require().True(open, "native event stream ended before classification evidence")
			if observed[message.Actor.ID] == nil {
				observed[message.Actor.ID] = map[events.Action]events.Message{}
			}
			observed[message.Actor.ID][message.Action] = message
			s.Require().LessOrEqual(len(observed), 4096)
		case eventErr := <-failures:
			s.Require().NoError(eventErr)
			s.Require().FailNow("native event stream ended before classification evidence")
		case <-ctx.Done():
			s.Require().NoError(ctx.Err(), "native classification did not complete within its bound")
		case <-time.After(100 * time.Millisecond):
			row := s.listingPartition(jobID, runID, taskID)
			if row.TaskRunID == "" {
				continue
			}
			if concreteID == "" {
				concreteID = row.TaskRunID
			}
			s.Require().Equal(concreteID, row.TaskRunID)
			if row.Status == "running" && row.RuntimeID != "" && ids[1] == "" {
				inspect, inspectErr := cli.ContainerInspect(ctx, row.RuntimeID)
				if errdefs.IsNotFound(inspectErr) {
					continue
				}
				s.Require().NoError(inspectErr)
				s.Require().NotNil(inspect.Config)
				s.Require().NotNil(inspect.State)
				s.Require().Equal(imageID, inspect.Image)
				s.Require().Equal(row.RuntimeID, inspect.ID)
				s.Require().Equal([]string{"sh", "-c", command}, []string(inspect.Config.Cmd))
				s.Require().False(inspect.State.OOMKilled)
				if inspect.State.Running {
					ids[1] = inspect.ID
					s.maintenanceLiveAttemptLogs(jobID, runID, taskID, concreteID, alias)
				}
			}
			s.maintenanceJSON("/v1/jobs/"+jobID+"/runs/"+runID, &terminal)
			s.Require().Equal(runID, terminal.ID)
			if terminal.Status != "failed" || ids[1] == "" || observed[ids[1]][events.ActionDie].TimeNano == 0 {
				continue
			}
			row = s.listingPartition(jobID, runID, taskID)
			s.Require().Equal(concreteID, row.TaskRunID)
			s.Require().Equal(ids[1], row.RuntimeID)
			s.Require().Equal("failed", row.Status)
			s.Require().Equal("terminated", row.Result)
			s.Require().Equal(1, row.Attempt)
			s.Require().False(row.OOMKilled)
			s.Require().NotNil(row.ExitCode)
			s.Require().Equal(143, *row.ExitCode)
			s.Require().NotNil(row.CompletedAt)
			s.Require().NotNil(terminal.CompletedAt)
			start, die := observed[ids[1]][events.ActionStart], observed[ids[1]][events.ActionDie]
			s.Require().Positive(start.TimeNano)
			s.Require().Greater(die.TimeNano, start.TimeNano)
			s.Require().Equal("143", die.Actor.Attributes["exitCode"])
			_, inspectErr := cli.ContainerInspect(ctx, ids[1])
			s.Require().True(errdefs.IsNotFound(inspectErr), "the exact classified native runtime must be absent")
			status, logs := s.maintenanceHTTP(http.MethodGet, fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s&task_run_id=%s", jobID, runID, taskID, concreteID), nil)
			s.Require().Equal(http.StatusOK, status)
			s.Require().Contains(string(logs), alias+"-started")
			s.Require().Contains(string(logs), alias+"-finished")
			s.Require().Contains(string(logs), alias+"-term-received")
			s.Require().Len(s.maintenanceRuns(jobID), 1)
			s.T().Logf("native classification exit=%d result=%s task_run_id=%s runtime_id=%s start_ns=%d die_ns=%d", 143, "terminated", concreteID, ids[1], start.TimeNano, die.TimeNano)
			return
		}
	}
}

func (s *IntegrationTestSuite) TestPublicListingsOrderByAndRefuseInvalidTerms() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey, "the intended auth lane must carry its operator key")
	for _, endpoint := range []string{"agentprofiles", "notifications/channels"} {
		s.Run(endpoint, func() {
			prefix := "maintenance-order-" + uuid.NewString()
			owned := map[string]string{}
			for _, suffix := range []string{"a", "z"} {
				name := prefix + "-" + suffix
				payload := fmt.Sprintf(`{"name":%q,"image":"alpine:3.23","engine":"docker"}`, name)
				if endpoint == "notifications/channels" {
					payload = fmt.Sprintf(`{"name":%q,"type":"webhook","config":{"url":"https://example.invalid/"},"enabled":false}`, name)
				}
				status, body := s.maintenanceHTTP(http.MethodPost, "/v1/"+endpoint, []byte(payload))
				s.Require().Equal(http.StatusCreated, status)
				var created listingObject
				s.Require().NoError(json.Unmarshal(body, &created))
				_, err := uuid.Parse(created.ID)
				s.Require().NoError(err)
				id := created.ID
				s.T().Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(s.T().Context()), 10*time.Second)
					defer cancel()
					status, _ := s.maintenanceHTTPContext(ctx, http.MethodDelete, "/v1/"+endpoint+"/"+id, nil)
					s.Equal(http.StatusNoContent, status, "owned listing resource deletion must complete")
					status, _ = s.maintenanceHTTPContext(ctx, http.MethodGet, "/v1/"+endpoint+"/"+id, nil)
					s.Equal(http.StatusNotFound, status, "owned listing resource must be durably absent")
				})
				s.Require().Equal(name, created.Name)
				if endpoint == "notifications/channels" {
					// The channel model's GORM default is enabled=true; set the
					// owned row inactive through the public PATCH before testing
					// listings. No notification policy references this fixture.
					status, body = s.maintenanceHTTP(http.MethodPatch, "/v1/"+endpoint+"/"+id, []byte(`{"enabled":false}`))
					s.Require().Equal(http.StatusOK, status)
					var disabled listingObject
					s.Require().NoError(json.Unmarshal(body, &disabled))
					s.Require().Equal(id, disabled.ID)
					s.Require().Equal(name, disabled.Name)
					s.Require().False(disabled.Enabled)
					s.Require().NotContains(string(body), "https://example.invalid/")
					s.Require().Contains(fmt.Sprint(disabled.Config["url"]), "****")
				}
				owned[id] = name
			}
			check := func(term string, descending bool) {
				var listed []listingObject
				s.maintenanceJSON("/v1/"+endpoint+"?order_by="+url.QueryEscape(term), &listed)
				var names []string
				found := map[string]bool{}
				for _, row := range listed {
					if name, exists := owned[row.ID]; exists {
						s.Require().False(found[row.ID], "ordered listing must not duplicate the owned row")
						s.Require().Equal(name, row.Name)
						found[row.ID] = true
						names = append(names, name)
						if endpoint == "notifications/channels" {
							s.Require().False(row.Enabled)
							s.Require().Contains(fmt.Sprint(row.Config["url"]), "****")
						}
					}
				}
				want := []string{prefix + "-a", prefix + "-z"}
				if descending {
					want[0], want[1] = want[1], want[0]
				}
				s.Require().Equal(want, names, "nonempty real rows must obey the requested order")
			}
			check("name", false)
			check(", NAME dEsC, created_at ASC, ,", true)
			for _, term := range []string{"name;DROP TABLE agent_profiles", "name sideways", "name asc extra"} {
				status, body := s.maintenanceHTTP(http.MethodGet, "/v1/"+endpoint+"?order_by="+url.QueryEscape(term), nil)
				s.Require().Equal(http.StatusBadRequest, status, "invalid order term %q", term)
				var refusal struct {
					Message string `json:"message"`
				}
				s.Require().NoError(json.Unmarshal(body, &refusal))
				s.Require().Equal("bad request", refusal.Message)
			}
			check("name aSc", false)
			for id, name := range owned {
				var actual listingObject
				s.maintenanceJSON("/v1/"+endpoint+"/"+id, &actual)
				s.Require().Equal(id, actual.ID)
				s.Require().Equal(name, actual.Name)
				if endpoint == "notifications/channels" {
					s.Require().False(actual.Enabled)
					s.Require().Contains(fmt.Sprint(actual.Config["url"]), "****")
				}
			}
		})
	}
}

type listingCacheEntry struct {
	Hash      string     `json:"hash"`
	TaskName  string     `json:"task_name"`
	Result    string     `json:"result"`
	RunID     string     `json:"run_id"`
	TaskRunID string     `json:"task_run_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type listingCacheResponse struct {
	Entries []listingCacheEntry `json:"entries"`
}

type listingObject struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
}

type listingTask struct {
	maintenanceTask
	Result    string `json:"result"`
	OOMKilled bool   `json:"oom_killed"`
}

func (s *IntegrationTestSuite) listingPartition(jobID, runID, taskID string) listingTask {
	var page struct {
		Total      int           `json:"total"`
		Partitions []listingTask `json:"partitions"`
	}
	s.maintenanceJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions?limit=2", jobID, runID, taskID), &page)
	if page.Total == 0 {
		s.Require().Empty(page.Partitions)
		return listingTask{}
	}
	s.Require().Equal(1, page.Total)
	s.Require().Len(page.Partitions, 1)
	_, err := uuid.Parse(page.Partitions[0].TaskRunID)
	s.Require().NoError(err)
	return page.Partitions[0]
}

func (s *IntegrationTestSuite) listingCacheCLI(args ...string) (string, string, error) {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(s.T().Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cliPath, append([]string{"cache"}, args...)...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (s *IntegrationTestSuite) requireCacheCLIStderrAck(stderr string, isAck func(string) bool, action string) {
	s.T().Helper()
	ackCount := 0
	for _, line := range strings.Split(stderr, "\n") {
		if line == "" {
			continue
		}
		if isAck(line) {
			ackCount++
			continue
		}
		var diagnostic map[string]any
		s.Require().NoError(json.Unmarshal([]byte(line), &diagnostic), "non-acknowledgement stderr must be a structured diagnostic line: %q", line)
		s.Require().NotEmpty(diagnostic, "non-acknowledgement stderr diagnostic must be a JSON object")
	}
	s.Require().Equal(1, ackCount, "stderr must contain exactly one full %s acknowledgement line", action)
}
