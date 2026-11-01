//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// #553: a run in flight when its server stops. These scenarios stop and restart
// a real server, so they cannot use the lane's shared one: a private server is
// cloned from it (same image, environment and mounts, so the lane's env parity
// carries over) and joins its network namespace, which is where this runner
// lives too. Stopping the private server leaves the shared one untouched.
//
// `just integration-test` names the container to clone. Other lanes do not, and
// skip: the default lane is the one that proves this.
const (
	restartTemplateEnv = "CAESIUM_IT_RESTART_TEMPLATE"
	restartServerName  = "caesium-restart-it"
	restartServerPort  = "18553"
	restartNodeAddress = "127.0.0.1:19553"
	// restartRecoveryBound is a sweep to first see the run, one TTL unchanged,
	// a beat behind the mark, and headroom for a loaded CI host.
	restartRecoveryBound = 60 * time.Second
)

type restartableServer struct {
	cli *client.Client
	id  string
}

func (r *restartableServer) stopGracefully(s *IntegrationTestSuite) {
	s.T().Helper()
	timeout := 30
	s.Require().NoError(r.cli.ContainerStop(s.T().Context(), r.id, container.StopOptions{Timeout: &timeout}))
}

func (r *restartableServer) crash(s *IntegrationTestSuite) {
	s.T().Helper()
	s.Require().NoError(r.cli.ContainerKill(s.T().Context(), r.id, "SIGKILL"))
}

// startRestartableServer starts the private server and returns a suite aimed at
// it. The container keeps its data directory across a stop, so a restart is the
// same server on the same database.
func (s *IntegrationTestSuite) startRestartableServer(overrides ...string) (*IntegrationTestSuite, *restartableServer) {
	s.T().Helper()
	template := os.Getenv(restartTemplateEnv)
	if template == "" {
		s.T().Skipf("%s is not set: only the default integration lane restarts a server", restartTemplateEnv)
	}
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the scenario drives containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	s.T().Cleanup(func() { _ = cli.Close() })
	ctx := s.T().Context()
	info, err := cli.ContainerInspect(ctx, template)
	s.Require().NoError(err, "%s names a container that does not exist", restartTemplateEnv)

	remove := func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(removeCtx, restartServerName, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
	remove()

	overrides = append([]string{
		"CAESIUM_PORT=" + restartServerPort,
		"CAESIUM_NODE_ADDRESS=" + restartNodeAddress,
		"CAESIUM_RUN_LEASE_TTL=6s",
	}, overrides...)
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: info.Image, Cmd: []string{"start"}, User: info.Config.User, Env: overriddenEnv(info.Config.Env, overrides)},
		&container.HostConfig{
			Binds:       info.HostConfig.Binds,
			Privileged:  info.HostConfig.Privileged,
			NetworkMode: container.NetworkMode("container:" + template),
		}, nil, nil, restartServerName)
	s.Require().NoError(err)
	server := &restartableServer{cli: cli, id: created.ID}
	s.T().Cleanup(func() {
		if s.T().Failed() {
			s.logRestartServer(server)
		}
		remove()
	})

	peer := &IntegrationTestSuite{
		caesiumURL:          "http://127.0.0.1:" + restartServerPort,
		cliPath:             s.cliPath,
		projectRoot:         s.projectRoot,
		engineType:          s.engineType,
		manualTriggerAPIKey: s.manualTriggerAPIKey,
		eventIngestAPIKey:   s.eventIngestAPIKey,
		authAPIKey:          s.authAPIKey,
	}
	peer.SetT(s.T())
	peer.startRestartServer(server)
	return peer, server
}

// startRestartServer starts the container and waits for its API.
func (s *IntegrationTestSuite) startRestartServer(server *restartableServer) {
	s.T().Helper()
	s.Require().NoError(server.cli.ContainerStart(s.T().Context(), server.id, container.StartOptions{}))
	s.Require().Eventually(func() bool {
		resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health", nil)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 90*time.Second, 500*time.Millisecond, "the private server never became healthy")
}

func (s *IntegrationTestSuite) logRestartServer(server *restartableServer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.T().Context()), 15*time.Second)
	defer cancel()
	logs, err := server.cli.ContainerLogs(ctx, server.id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "60"})
	if err != nil {
		s.T().Logf("private server logs unavailable: %v", err)
		return
	}
	defer logs.Close()
	tail, _ := io.ReadAll(logs)
	s.T().Logf("private server logs:\n%s", tail)
}

// overriddenEnv is base with every overridden variable replaced.
func overriddenEnv(base, overrides []string) []string {
	replaced := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		name, _, _ := strings.Cut(override, "=")
		replaced[name] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if !replaced[name] {
			out = append(out, entry)
		}
	}
	return append(out, overrides...)
}

// restartJobManifest is a manual-only job: an http trigger nothing calls, so no
// cron tick or webhook can adopt its run and recovery has to do it alone.
func restartJobManifest(alias, marker, concurrency, revision string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
  labels:
    revision: %[4]s
%[3]s
trigger:
  type: http
  configuration:
    path: %[1]s
steps:
  - name: work
    image: alpine:3.23
    command: ["sh", "-c", "echo %[2]s; sleep $CAESIUM_PARAM_SLEEP"]
`, alias, marker, concurrency, revision)
}

// restartDistributedEnv is default SQL advancement on one node.
var restartDistributedEnv = []string{
	"CAESIUM_EXECUTION_MODE=distributed",
	"CAESIUM_WORKER_ENABLED=true",
	"CAESIUM_WORKER_POOL_SIZE=1",
	"CAESIUM_WORKER_POLL_INTERVAL=500ms",
	"CAESIUM_WORKER_RECLAIM_INTERVAL=500ms",
	"CAESIUM_WORKER_LEASE_TTL=6s",
}

const restartQueueConcurrency = `  concurrency:
    maxRuns: 1
    strategy: queue`

// awaitRunTaskRunning waits for the run's task row AND its container, and
// returns the container id: the crash has to land on real work.
func (s *IntegrationTestSuite) awaitRunTaskRunning(cli *client.Client, jobID, runID, marker string) string {
	s.T().Helper()
	var containerID string
	s.Require().Eventually(func() bool {
		var run runResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run); err != nil {
			return false
		}
		if run.Status != "running" || len(run.Tasks) != 1 || run.Tasks[0].Status != "running" {
			return false
		}
		ids := s.containerIDsForRun(cli, marker, runID)
		if len(ids) != 1 {
			return false
		}
		containerID = ids[0]
		return true
	}, 60*time.Second, 250*time.Millisecond, "run %s never had a running task with one container", runID)
	return containerID
}

// containerIDsForRun returns every container one run ever started. The name
// carries the run id, so the queued run's container is not counted.
func (s *IntegrationTestSuite) containerIDsForRun(cli *client.Client, marker, runID string) []string {
	s.T().Helper()
	var out []string
	for _, id := range s.containerIDsWithMarker(cli, marker) {
		if strings.Contains(s.containerName(cli, id), runID) {
			out = append(out, id)
		}
	}
	return out
}

// queueRun posts a start that must be queued behind the run holding the slot.
func (s *IntegrationTestSuite) queueRun(jobID string, params map[string]string) {
	s.T().Helper()
	body, err := json.Marshal(map[string]any{"params": params})
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var outcome struct {
		Outcome string `json:"outcome"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&outcome))
	s.Require().Equal("queued", outcome.Outcome)
}

func (s *IntegrationTestSuite) awaitRunWithToken(jobID, token, status string) {
	s.T().Helper()
	s.Require().Eventually(func() bool {
		var runs []runResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs", jobID), &runs); err != nil {
			return false
		}
		for _, run := range runs {
			if run.Params["TOKEN"] == token {
				return run.Status == status
			}
		}
		return false
	}, restartRecoveryBound, 500*time.Millisecond, "no run with token %q reached %q", token, status)
}

func (s *IntegrationTestSuite) applyRestartJob(contents string) {
	s.T().Helper()
	dir := s.writeJobManifest(contents)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
}

// assertInFlightRunRecovered is the whole of #553 for local mode: after the
// restart, with no trigger to help, the run ends failed and says why, nothing
// executed a second time, the queue behind it drains, and apply works again.
func (s *IntegrationTestSuite) assertInFlightRunRecovered(alias string, stop func(*restartableServer, *IntegrationTestSuite), orphanSurvives bool) {
	peer, server := s.startRestartableServer()
	marker := fmt.Sprintf("caesium-restart-marker-%d", time.Now().UnixNano())
	defer peer.removeContainersWithMarker(server.cli, marker)

	peer.applyRestartJob(restartJobManifest(alias, marker, restartQueueConcurrency, "one"))
	job := peer.requireJobByAlias(alias)
	runID := peer.triggerRunWithParams(job.ID, map[string]string{"SLEEP": "300", "TOKEN": "in-flight"})
	containerID := peer.awaitRunTaskRunning(server.cli, job.ID, runID, marker)
	peer.queueRun(job.ID, map[string]string{"SLEEP": "0", "TOKEN": "queued"})

	stop(server, peer)
	peer.startRestartServer(server)

	run := peer.awaitRun(job.ID, runID, restartRecoveryBound)
	s.Equal("failed", run.Status, "the run has no evidence of success and must not report one")
	s.Contains(run.Error, "stopped before it finished")
	s.Require().Len(run.Tasks, 1)
	s.Equal("failed", run.Tasks[0].Status)

	if orphanSurvives {
		s.Equal([]string{containerID}, peer.containerIDsForRun(server.cli, marker, runID),
			"a task whose container may still be running must not be executed again")
		s.True(peer.containerIsRunning(server.cli, containerID), "the crash should have left the container running")
	} else {
		s.Empty(peer.containerIDsForRun(server.cli, marker, runID),
			"a graceful stop removes its container and recovery must not start another")
	}

	peer.awaitRunWithToken(job.ID, "queued", "succeeded")
	// The manifest changed, so this reaches the running-run check that used to
	// refuse it forever.
	peer.applyRestartJob(restartJobManifest(alias, marker, restartQueueConcurrency, "two"))

	if orphanSurvives {
		// Even an operator's retry cannot start a second copy: the orphan holds
		// the container name, so the retry fails until the orphan is removed.
		peer.runCLI("run", "retry", "--job-id", job.ID, "--run-id", runID, "--server", peer.caesiumURL)
		retried := peer.awaitRun(job.ID, runID, restartRecoveryBound)
		s.Equal("failed", retried.Status)
		s.Contains(retried.Error, "already in use")
		s.Equal([]string{containerID}, peer.containerIDsForRun(server.cli, marker, runID))
	}
}

func (s *IntegrationTestSuite) TestInFlightRunRecoveredAfterServerCrash() {
	s.assertInFlightRunRecovered("restart-crash-job", func(server *restartableServer, peer *IntegrationTestSuite) {
		server.crash(peer)
	}, true)
}

func (s *IntegrationTestSuite) TestInFlightRunRecoveredAfterGracefulStop() {
	s.assertInFlightRunRecovered("restart-graceful-job", func(server *restartableServer, peer *IntegrationTestSuite) {
		server.stopGracefully(peer)
	}, false)
}

// In distributed mode the workers own the tasks and the lost process only
// finalized the run, so the same recovery adopts the run and lets it succeed.
// Default SQL advancement on a manual-only job, which used to stay running.
func (s *IntegrationTestSuite) TestDistributedRunAdoptedAfterServerCrash() {
	peer, server := s.startRestartableServer(restartDistributedEnv...)
	marker := fmt.Sprintf("caesium-restart-marker-%d", time.Now().UnixNano())
	defer peer.removeContainersWithMarker(server.cli, marker)

	alias := "restart-distributed-job"
	peer.applyRestartJob(restartJobManifest(alias, marker, "", "one"))
	job := peer.requireJobByAlias(alias)
	runID := peer.triggerRunWithParams(job.ID, map[string]string{"SLEEP": "5", "TOKEN": "in-flight"})
	peer.awaitRunTaskRunning(server.cli, job.ID, runID, marker)

	server.crash(peer)
	peer.startRestartServer(server)

	run := peer.awaitRun(job.ID, runID, restartRecoveryBound)
	s.Equal("succeeded", run.Status, "error: %s", run.Error)
	peer.applyRestartJob(restartJobManifest(alias, marker, "", "two"))
}

// Deleting a job with a run in flight is allowed and only soft-deletes it, so the
// run must still reach an outcome: left running, it would hold a quota slot. The
// run API answers 404 for a deleted job, so this reads the server's own count of
// running rows, which is the number that matters.
func (s *IntegrationTestSuite) TestRunOfDeletedJobAdoptedAfterServerCrash() {
	peer, server := s.startRestartableServer(restartDistributedEnv...)
	marker := fmt.Sprintf("caesium-restart-marker-%d", time.Now().UnixNano())
	defer peer.removeContainersWithMarker(server.cli, marker)

	alias := "restart-deleted-job"
	peer.applyRestartJob(restartJobManifest(alias, marker, "", "one"))
	job := peer.requireJobByAlias(alias)
	runID := peer.triggerRunWithParams(job.ID, map[string]string{"SLEEP": "5", "TOKEN": "in-flight"})
	peer.awaitRunTaskRunning(server.cli, job.ID, runID, marker)
	s.Require().Equal(1, peer.runningRowCount(), "the scenario needs a run in flight")

	resp, err := peer.doRequest(http.MethodDelete, fmt.Sprintf("%s/v1/jobs/%s", peer.caesiumURL, job.ID), nil)
	s.Require().NoError(err)
	resp.Body.Close()
	s.Require().Less(resp.StatusCode, 300, "the job could not be deleted")

	server.crash(peer)
	peer.startRestartServer(server)

	s.Require().Eventually(func() bool { return peer.runningRowCount() == 0 },
		restartRecoveryBound, 500*time.Millisecond, "a deleted job's run stayed running after the restart")
}

// runningRowCount is /health's count of running runs, or -1 if it is unreadable.
func (s *IntegrationTestSuite) runningRowCount() int {
	var health struct {
		Checks *struct {
			ActiveRuns *struct {
				Count int `json:"count"`
			} `json:"active_runs"`
		} `json:"checks"`
	}
	if err := s.tryGetJSON("/health", &health); err != nil || health.Checks == nil || health.Checks.ActiveRuns == nil {
		return -1
	}
	return health.Checks.ActiveRuns.Count
}
