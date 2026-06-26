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
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
)

func (s *IntegrationTestSuite) TestReplayMatrixSuppressesSideEffectsAndObservability() {
	// This scenario captures notifications via a webhook bound on the TEST HOST, which
	// the server must call back into. That host-reachability holds under the docker
	// tier but not under the podman/kubernetes integration tiers (container/pod → host
	// networking differs), so the webhook positive-control can't fire there. The
	// notification suppression itself is covered engine-agnostically by the unit tests
	// (internal/notification/watcher_test.go + subscriber_test.go), and the lineage/SSE
	// suppression by internal/lineage/subscriber_test.go + the event stream tests.
	if s.engineType == "podman" || s.engineType == "kubernetes" {
		s.T().Skipf("notification-webhook capture is not host-reachable under CAESIUM_TEST_ENGINE=%s; suppression is covered by the engine-agnostic unit tests", s.engineType)
	}
	alias := fmt.Sprintf("e2e-replay-matrix-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayLineageManifest(alias, "1h"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	notifications := newReplayWebhookCapture(s.T().Context())
	defer notifications.Close()
	channelID := s.createWebhookNotificationChannel("replay-matrix-"+strconv.FormatInt(time.Now().UnixNano(), 10), notifications.URL())
	s.createNotificationPolicy("replay-lifecycle-"+strconv.FormatInt(time.Now().UnixNano(), 10), channelID, []string{"run_completed", "task_succeeded"}, job.ID)
	s.createNotificationPolicy("replay-watcher-"+strconv.FormatInt(time.Now().UnixNano(), 10), channelID, []string{"run_timed_out", "sla_missed"}, job.ID)

	defaultEvents, closeDefault := s.openSSE("/v1/events")
	defer closeDefault()

	baselineRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baselineRunID, runTimeout).Status)
	s.Require().Eventually(func() bool {
		return notifications.Count() >= 3
	}, 60*time.Second, 250*time.Millisecond, "normal baseline run should prove the real notification webhook path is active")

	rootName := alias + ".extract.output"
	wantName := alias + ".transform.output"
	s.requireLineageImpact(rootName, wantName)
	statsBefore := s.fetchStatsSummary()
	runsBefore := s.fetchRuns(job.ID)
	s.Require().Len(runsBefore, 1)
	cursorBefore := s.latestEventCursor()
	notifications.Reset()

	key := "replay-matrix-" + time.Now().Format("150405.000000000")
	replay := s.mustPostReplay(job.ID, baselineRunID, &key, `{"set":{}}`)
	s.True(replay.Quarantine)
	replayRun := s.awaitRun(job.ID, replay.RunID, runTimeout)
	s.Equal("succeeded", replayRun.Status)
	s.True(replayRun.Quarantine)

	scopedEvents, closeScoped := s.openSSE("/v1/events?run_id=" + url.QueryEscape(replay.RunID))
	defer closeScoped()
	s.Require().Eventually(func() bool {
		return hasEventForRun(scopedEvents.Drain(), replay.RunID, event.TypeRunCompleted)
	}, 30*time.Second, 250*time.Millisecond, "run-scoped replay event stream should expose the initiating replay")

	time.Sleep(2 * time.Second)
	s.Empty(notifications.Drain(), "quarantined replay must not invoke lifecycle or watcher notification policies")
	s.False(hasAnyEventForRun(defaultEvents.Drain(), replay.RunID), "default live SSE stream must not leak quarantined replay events")
	backlog := s.readSSEBacklog(fmt.Sprintf("/v1/events?cursor=%d", cursorBefore), 5*time.Second)
	s.False(hasAnyEventForRun(backlog, replay.RunID), "default SSE backlog must not leak quarantined replay events")

	// Keep this e2e on job-scoped observables; global counters drift on the shared integration server.
	s.requireLineageImpact(rootName, wantName)
	s.Len(s.fetchRuns(job.ID), len(runsBefore), "production run list must exclude quarantined replay")

	normalRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, normalRunID, runTimeout).Status)
	s.Require().Eventually(func() bool {
		return hasEventForRun(defaultEvents.Drain(), normalRunID, event.TypeRunCompleted)
	}, 30*time.Second, 250*time.Millisecond, "normal run must remain visible on default SSE")
	runsAfterNormal := s.fetchRuns(job.ID)
	s.Len(runsAfterNormal, len(runsBefore)+1, "normal non-quarantined run must remain visible in run list")
	s.GreaterOrEqual(s.fetchStatsSummary().Jobs.RecentRuns, statsBefore.Jobs.RecentRuns+1, "normal run must remain visible in stats")
}

func (s *IntegrationTestSuite) TestReplayMatrixCachePrunedFailsClosed() {
	alias := fmt.Sprintf("e2e-replay-cache-pruned-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayLineageManifest(alias, "1ns"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	baselineRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baselineRunID, runTimeout).Status)
	time.Sleep(50 * time.Millisecond)

	key := "replay-cache-pruned-" + time.Now().Format("150405.000000000")
	response := s.postReplay(job.ID, baselineRunID, &key, `{"set":{}}`)
	s.Equal(http.StatusConflict, response.status, response.body)
	s.Contains(response.body, "unchanged baseline result unavailable")
	s.Len(s.fetchRuns(job.ID), 1, "cache-pruned replay refusal must not materialize a production-visible run")
}

func (s *IntegrationTestSuite) TestReplayMatrixBaselineScopedReplaySafeGate() {
	alias := fmt.Sprintf("e2e-replay-baseline-gate-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayManifest(alias, false))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	unsafeRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, unsafeRunID, runTimeout).Status)

	s.writeJobManifestToDir(dir, replayManifest(alias, true))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	unsafeKey := "replay-baseline-unsafe-" + time.Now().Format("150405.000000000")
	unsafeReplay := s.postReplay(job.ID, unsafeRunID, &unsafeKey, `{"set":{"mode":"what-if"}}`)
	s.Equal(http.StatusUnprocessableEntity, unsafeReplay.status, unsafeReplay.body)
	s.Contains(unsafeReplay.body, "not replay safe")

	safeRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, safeRunID, runTimeout).Status)
	s.writeJobManifestToDir(dir, replayManifest(alias, false))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	safeKey := "replay-baseline-safe-" + time.Now().Format("150405.000000000")
	safeReplay := s.mustPostReplay(job.ID, safeRunID, &safeKey, `{"set":{}}`)
	s.True(safeReplay.Quarantine)
	s.Equal("succeeded", s.awaitRun(job.ID, safeReplay.RunID, runTimeout).Status)
}

type replayWebhookCapture struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newReplayWebhookCapture(ctx context.Context) *replayWebhookCapture {
	c := &replayWebhookCapture{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	return c
}

func (c *replayWebhookCapture) Close() {
	if c != nil && c.server != nil {
		c.server.Close()
	}
}

func (c *replayWebhookCapture) URL() string {
	if c == nil || c.server == nil {
		return ""
	}
	return c.server.URL
}

func (c *replayWebhookCapture) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *replayWebhookCapture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = nil
}

func (c *replayWebhookCapture) Drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]string(nil), c.bodies...)
	c.bodies = nil
	return out
}

func (s *IntegrationTestSuite) createWebhookNotificationChannel(name, target string) string {
	payload := fmt.Sprintf(`{"name":%q,"type":"webhook","config":{"url":%q},"enabled":true}`, name, target)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/notifications/channels", strings.NewReader(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, resp.StatusCode, string(body))
	var created struct {
		ID string `json:"id"`
	}
	s.Require().NoError(json.Unmarshal(body, &created))
	s.Require().NotEmpty(created.ID)
	return created.ID
}

func (s *IntegrationTestSuite) createNotificationPolicy(name, channelID string, eventTypes []string, jobID string) {
	typesJSON, err := json.Marshal(eventTypes)
	s.Require().NoError(err)
	payload := fmt.Sprintf(`{"name":%q,"channel_id":%q,"event_types":%s,"filters":{"job_ids":[%q]},"enabled":true}`, name, channelID, typesJSON, jobID)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/notifications/policies", strings.NewReader(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, resp.StatusCode, string(body))
}

type statsSummary struct {
	Jobs struct {
		RecentRuns  int64   `json:"recent_runs"`
		SuccessRate float64 `json:"success_rate"`
	} `json:"jobs"`
}

func (s *IntegrationTestSuite) fetchStatsSummary() statsSummary {
	var summary statsSummary
	s.getJSON("/v1/stats/summary?window=24h", &summary)
	return summary
}

type replaySSECapture struct {
	events chan event.Event
	errs   chan error
	cancel context.CancelFunc
	resp   *http.Response
}

func (s *IntegrationTestSuite) openSSE(path string) (*replaySSECapture, func()) {
	s.T().Helper()
	ctx, cancel := context.WithCancel(s.T().Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the returned cleanup func (defer closeFn)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	c := &replaySSECapture{
		events: make(chan event.Event, 128),
		errs:   make(chan error, 1),
		cancel: cancel,
		resp:   resp,
	}
	go readReplaySSE(resp.Body, c.events, c.errs, ctx.Done())
	return c, func() {
		cancel()
		_ = resp.Body.Close()
	}
}

func (c *replaySSECapture) Drain() []event.Event {
	var out []event.Event
	for {
		select {
		case evt := <-c.events:
			out = append(out, evt)
		default:
			return out
		}
	}
}

func (s *IntegrationTestSuite) readSSEBacklog(path string, wait time.Duration) []event.Event {
	capture, closeFn := s.openSSE(path)
	defer closeFn()
	time.Sleep(wait)
	return capture.Drain()
}

func readReplaySSE(body io.Reader, out chan<- event.Event, errs chan<- error, done <-chan struct{}) {
	scanner := bufio.NewScanner(body)
	var currentType event.Type
	var currentData []byte
	for scanner.Scan() {
		select {
		case <-done:
			return
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			if len(currentData) > 0 {
				var evt event.Event
				if err := json.Unmarshal(currentData, &evt); err == nil {
					if evt.Type == "" {
						evt.Type = currentType
					}
					select {
					case out <- evt:
					case <-done:
						return
					}
				}
			}
			currentType = ""
			currentData = nil
			continue
		}
		if bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		parts := bytes.SplitN(line, []byte(":"), 2)
		if len(parts) < 2 {
			continue
		}
		field := string(bytes.TrimSpace(parts[0]))
		value := bytes.TrimPrefix(parts[1], []byte(" "))
		switch field {
		case "event":
			currentType = event.Type(value)
		case "data":
			currentData = append(currentData[:0], value...)
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case errs <- err:
		default:
		}
	}
}

func hasAnyEventForRun(events []event.Event, runID string) bool {
	for _, evt := range events {
		if evt.RunID.String() == runID {
			return true
		}
	}
	return false
}

func hasEventForRun(events []event.Event, runID string, typ event.Type) bool {
	for _, evt := range events {
		if evt.RunID.String() == runID && evt.Type == typ {
			return true
		}
	}
	return false
}

func (s *IntegrationTestSuite) latestEventCursor() uint64 {
	events := s.readSSEBacklog("/v1/events", 500*time.Millisecond)
	var cursor uint64
	for _, evt := range events {
		if evt.Sequence > cursor {
			cursor = evt.Sequence
		}
	}
	return cursor
}

func (s *IntegrationTestSuite) requireLineageImpact(rootName, wantName string) {
	s.T().Helper()
	deadline := time.Now().Add(30 * time.Second)
	var res impactResult
	for s.T().Context().Err() == nil {
		res = impactResult{}
		_ = s.tryGetJSON("/v1/lineage/impact?namespace=caesium&name="+url.QueryEscape(rootName), &res)
		for _, n := range res.Downstream {
			if n.DatasetName == wantName {
				return
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.T().Fatalf("expected %s downstream of %s, got %+v", wantName, rootName, res.Downstream)
}

func replayLineageManifest(alias, ttl string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  replaySafe: true
  cache:
    ttl: %q
  timeout: 1s
  sla:
    duration: 1s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    outputSchema: { type: object, properties: { rows: { type: string } } }
    command: ["sh","-c","echo '##caesium::output {\"rows\": \"1\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    inputSchema: { extract: { properties: { rows: { type: string } } } }
    outputSchema: { type: object, properties: { clean: { type: string } } }
    command: ["sh","-c","echo got $CAESIUM_OUTPUT_EXTRACT_ROWS; echo '##caesium::output {\"clean\": \"y\"}'"]
    dependsOn: extract
`, alias, ttl)
}

func (s *IntegrationTestSuite) writeJobManifestToDir(dir, contents string) {
	s.T().Helper()
	path := dir + string(os.PathSeparator) + "job.yaml"
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(contents))), 0o644))
}
