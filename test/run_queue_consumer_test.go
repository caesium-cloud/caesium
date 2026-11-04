//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
)

// A job-policy queue is drained only by the full dequeuer. The shared
// integration server starts that dequeuer (both flags true). The private
// server below forces both flags off while keeping the lane's policy file, so
// the quota-only dequeuer is running and must still not accept job-policy
// overflow.

const (
	queueFlagServerName = "caesium-queue-flag-it"
	queueFlagServerPort = "18554"
	queueFlagNode       = "127.0.0.1:19554"
)

func (s *IntegrationTestSuite) TestQueueStrategyDrainsAndKeepsParams() {
	alias := fmt.Sprintf("e2e-queue-drain-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(concurrencyJobManifest(alias, "queue", `echo "$CAESIUM_PARAM_TOKEN"; sleep 20`))
	s.T().Cleanup(func() { _ = os.RemoveAll(dir) })
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	holderOut, holderErr, err := s.runCLISeparate(
		"run", "start", "--alias", alias, "--params", "token=holder", "--server", s.caesiumURL,
	)
	s.Require().NoError(err, "holder start failed: %s", holderErr)
	holderID := strings.TrimSpace(holderOut)
	s.Require().NoError(uuid.Validate(holderID), "holder stdout=%q", holderOut)
	s.awaitRunHasTasks(job.ID, holderID, 30*time.Second)
	s.Equal("running", s.fetchRun(job.ID, holderID).Status)

	queuedOut, queuedErr, err := s.runCLISeparate(
		"run", "start", "--alias", alias, "--params", "token=queued-keep", "--server", s.caesiumURL,
	)
	s.Require().NoError(err, "queued start failed: %s", queuedErr)
	s.Empty(strings.TrimSpace(queuedOut), "a queued start prints no run id on stdout:\n%s", queuedOut)
	s.Contains(queuedErr, "queued")

	s.Equal("succeeded", s.awaitRunStatus(job.ID, holderID, runTimeout, "succeeded").Status)
	var kept runResponse
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(job.ID) {
			if run.Params["token"] != "queued-keep" {
				continue
			}
			kept = run
			return run.Status == "succeeded" || run.Status == "failed"
		}
		return false
	}, runTimeout, time.Second, "the queued run never started")
	s.Equal("succeeded", kept.Status)
	s.Equal("queued-keep", kept.Params["token"])
	s.NotEqual(holderID, kept.ID)
}

func (s *IntegrationTestSuite) TestQueueStrategyRefusesOverflowWhenDequeuerDisabled() {
	peer := s.startQueueFlagServer()
	alias := fmt.Sprintf("e2e-queue-disabled-%d", time.Now().UnixNano())
	dir := peer.writeJobManifest(concurrencyJobManifest(alias, "queue", `echo "$CAESIUM_PARAM_TOKEN"; sleep 40`))
	peer.T().Cleanup(func() { _ = os.RemoveAll(dir) })
	peer.runCLI("job", "apply", "--path", dir, "--server", peer.caesiumURL)
	job := peer.requireJobByAlias(alias)

	holderOut, holderErr, err := peer.runCLISeparate(
		"run", "start", "--alias", alias, "--params", "token=holder", "--server", peer.caesiumURL,
	)
	peer.Require().NoError(err, "holder start failed: %s", holderErr)
	holderID := strings.TrimSpace(holderOut)
	peer.Require().NoError(uuid.Validate(holderID), "holder stdout=%q", holderOut)
	peer.awaitRunHasTasks(job.ID, holderID, 30*time.Second)
	peer.Equal("running", peer.fetchRun(job.ID, holderID).Status)

	refusedOut, refusedErr, err := peer.runCLISeparate(
		"run", "start", "--alias", alias, "--params", "token=stranded-cli", "--server", peer.caesiumURL,
	)
	peer.Require().Error(err, "overflow must be refused while the queue consumer is off\nstdout=%s\nstderr=%s", refusedOut, refusedErr)
	for _, line := range strings.Split(refusedOut, "\n") {
		peer.Error(uuid.Validate(strings.TrimSpace(line)), "stdout must not contain a run id:\n%s", refusedOut)
	}
	peer.Contains(refusedErr, "CAESIUM_RUN_QUEUE_ENABLED")
	peer.Contains(refusedErr, "CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED")
	peer.Contains(refusedErr, "disabled")
	peer.NotContains(refusedOut, "stranded-cli")

	status, message := peer.postRunExpectingMessage(job.ID, `{"params":{"token":"stranded-http"}}`)
	peer.Equal(http.StatusConflict, status)
	peer.Contains(message, "CAESIUM_RUN_QUEUE_ENABLED")
	peer.Contains(message, "CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED")
	peer.Contains(message, "disabled")

	var rows []queueCLIItem
	peer.getJSON("/v1/jobs/"+job.ID+"/queue", &rows)
	peer.Empty(rows, "a refused overflow must not be parked")

	peer.Equal("succeeded", peer.awaitRunStatus(job.ID, holderID, runTimeout, "succeeded").Status)
	for _, run := range peer.fetchRuns(job.ID) {
		peer.NotEqual("stranded-cli", run.Params["token"])
		peer.NotEqual("stranded-http", run.Params["token"])
	}
	peer.Len(peer.fetchRuns(job.ID), 1)

	againOut, againErr, err := peer.runCLISeparate(
		"run", "start", "--alias", alias, "--params", "token=after-slot", "--server", peer.caesiumURL,
	)
	peer.Require().NoError(err, "a start that fits in maxRuns must still run: %s", againErr)
	againID := strings.TrimSpace(againOut)
	peer.Require().NoError(uuid.Validate(againID), "stdout=%q", againOut)
	finished := peer.awaitRun(job.ID, againID, runTimeout)
	peer.Equal("succeeded", finished.Status)
	peer.Equal("after-slot", finished.Params["token"])
	peer.Len(peer.fetchRuns(job.ID), 2)
}

func (s *IntegrationTestSuite) postRunExpectingMessage(jobID, body string) (int, string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+"/v1/jobs/"+jobID+"/run", strings.NewReader(body))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	s.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	var payload struct {
		Message string `json:"message"`
	}
	s.Require().NoError(json.Unmarshal(raw, &payload), string(raw))
	return resp.StatusCode, payload.Message
}

// startQueueFlagServer clones the integration server with both queue flags
// forced off. It joins the template's network namespace on its own port, the
// same way the restart scenarios do, and leaves the shared server running.
func (s *IntegrationTestSuite) startQueueFlagServer() *IntegrationTestSuite {
	s.T().Helper()
	template := os.Getenv(restartTemplateEnv)
	if template == "" {
		s.T().Skipf("%s is not set: only the default integration lane clones a server", restartTemplateEnv)
	}
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the disabled-queue server is cloned over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	s.T().Cleanup(func() { _ = cli.Close() })
	ctx := s.T().Context()
	info, err := cli.ContainerInspect(ctx, template)
	s.Require().NoError(err, "%s names a container that does not exist", restartTemplateEnv)

	remove := func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(removeCtx, queueFlagServerName, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
	remove()

	created, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: info.Image,
			Cmd:   []string{"start"},
			User:  info.Config.User,
			Env: overriddenEnv(info.Config.Env, []string{
				"CAESIUM_PORT=" + queueFlagServerPort,
				"CAESIUM_NODE_ADDRESS=" + queueFlagNode,
				"CAESIUM_RUN_QUEUE_ENABLED=false",
				"CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=false",
			}),
		},
		&container.HostConfig{
			Binds:       info.HostConfig.Binds,
			Privileged:  info.HostConfig.Privileged,
			NetworkMode: container.NetworkMode("container:" + template),
		}, nil, nil, queueFlagServerName)
	s.Require().NoError(err)
	server := &restartableServer{cli: cli, id: created.ID}
	s.T().Cleanup(func() {
		if s.T().Failed() {
			s.logRestartServer(server)
		}
		remove()
	})

	peer := &IntegrationTestSuite{
		caesiumURL:          "http://127.0.0.1:" + queueFlagServerPort,
		cliPath:             s.cliPath,
		projectRoot:         s.projectRoot,
		engineType:          s.engineType,
		manualTriggerAPIKey: s.manualTriggerAPIKey,
		eventIngestAPIKey:   s.eventIngestAPIKey,
		authAPIKey:          s.authAPIKey,
	}
	peer.SetT(s.T())
	peer.Require().NoError(cli.ContainerStart(ctx, created.ID, container.StartOptions{}))
	peer.Require().Eventually(func() bool {
		resp, err := peer.doRequest(http.MethodGet, peer.caesiumURL+"/health", nil)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 90*time.Second, 500*time.Millisecond, "the queue-flag server never became healthy")
	return peer
}
