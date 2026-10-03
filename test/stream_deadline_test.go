//go:build integration

package test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise both actual REST streams past the API server's 30-second write
// timeout. Healthy idle connections must keep delivering 15-second heartbeats.
func (s *IntegrationTestSuite) TestIdleStreamsOutliveHTTPWriteTimeout() {
	for _, path := range []string{"/v1/events?types=integration_idle_stream", "/v1/logs/stream?level=error"} {
		s.T().Run(path, func(t *testing.T) {
			// These subtests share only immutable suite configuration. Revisit
			// parallel execution if suite SetupTest/TearDownTest hooks are added.
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
			require.NoError(t, err)
			if s.authAPIKey != "" {
				req.Header.Set("Authorization", "Bearer "+s.authAPIKey)
			}
			started := time.Now()
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
			scanner := bufio.NewScanner(resp.Body)
			pings := 0
			for scanner.Scan() {
				if scanner.Text() == ": ping" {
					pings++
					if pings == 4 {
						require.Greater(t, time.Since(started), 30*time.Second)
						return
					}
				}
			}
			require.NoError(t, scanner.Err(), "stream disconnected before its fourth heartbeat")
			t.Fatalf("stream ended after %d heartbeats", pings)
		})
	}
}

// Both runtime logs and secret-bearing snapshot tails must survive the server
// deadline. The branching job runs these two producers concurrently.
func (s *IntegrationTestSuite) TestLiveTaskLogsOutliveHTTPWriteTimeout() {
	alias := fmt.Sprintf("stream-deadline-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: start
    image: alpine:3.23
    cache: false
    command: ["true"]
    next: [plain, scrubbed]
  - name: plain
    image: alpine:3.23
    cache: false
    dependsOn: [start]
    command: ["sh", "-c", "for i in 0 1 2 3 4; do echo deadline-$i; sleep 10; done; echo deadline-finished"]
  - name: scrubbed
    image: alpine:3.23
    cache: false
    dependsOn: [start]
    env:
      QA_STREAM_SECRET: secret://env/PATH
    command: ["sh", "-c", "for i in 0 1 2 3 4; do echo deadline-$i secret=$QA_STREAM_SECRET; sleep 10; done; echo deadline-finished"]
`, alias)
	// PATH is a nonsensitive canary present in every server image. Declaring it
	// through secret:// exercises the real scrubbed routing and redaction path.
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	plainID := s.jobTaskIDByName(job.ID, "plain")
	scrubbedID := s.jobTaskIDByName(job.ID, "scrubbed")
	s.Require().Eventually(func() bool {
		run := s.fetchRun(job.ID, runID)
		ready := 0
		for _, task := range run.Tasks {
			if (task.ID == plainID || task.ID == scrubbedID) && task.Status == "running" {
				ready++
			}
		}
		return ready == 2
	}, 30*time.Second, 100*time.Millisecond, "both log producers must be running before opening their streams")
	ctx, cancel := context.WithTimeout(s.T().Context(), 110*time.Second)
	defer cancel()
	type stream struct {
		name   string
		body   io.ReadCloser
		opened time.Time
	}
	var streams []stream
	for _, name := range []string{"plain", "scrubbed"} {
		taskID := s.jobTaskIDByName(job.ID, name)
		url := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, runID, taskID)
		readyDeadline := time.Now().Add(30 * time.Second)
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			s.Require().NoError(err)
			s.authorize(req)
			resp, err := http.DefaultClient.Do(req)
			s.Require().NoError(err)
			if resp.StatusCode == http.StatusNoContent {
				_ = resp.Body.Close()
				s.Require().Equal("pending", resp.Header.Get("X-Caesium-Log-State"))
				s.Require().True(time.Now().Before(readyDeadline), "task did not expose a live stream")
				time.Sleep(100 * time.Millisecond)
				continue
			}
			defer resp.Body.Close()
			s.Require().Equal(http.StatusOK, resp.StatusCode)
			s.Require().Equal("live", resp.Header.Get("X-Caesium-Log-Source"), name)
			streams = append(streams, stream{name, resp.Body, time.Now()})
			break
		}
	}
	for _, live := range streams {
		body, err := io.ReadAll(live.body)
		s.Require().NoError(err, "%s log stream must not truncate", live.name)
		s.Require().Greater(time.Since(live.opened), 40*time.Second)
		for i := range 5 {
			s.Contains(string(body), fmt.Sprintf("deadline-%d", i), live.name)
		}
		s.Contains(string(body), "deadline-finished", live.name)
		if live.name == "scrubbed" {
			s.Contains(string(body), "secret=[REDACTED]")
		}
	}
	s.Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)
}
