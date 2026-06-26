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
	"sync"
	"time"
)

type replayResponse struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Quarantine bool   `json:"quarantine"`
}

func (s *IntegrationTestSuite) TestReplayRESTEndpointIdempotencyAndSafety() {
	aliasA := fmt.Sprintf("e2e-replay-safe-a-%d", time.Now().UnixNano())
	dirA := s.writeJobManifest(replayManifest(aliasA, true))
	defer os.RemoveAll(dirA)
	s.runCLI("job", "apply", "--path", dirA, "--server", s.caesiumURL)
	jobA := s.requireJobByAlias(aliasA)
	s.Require().NotNil(jobA)

	runA := s.triggerRun(jobA.ID)
	s.Require().Equal("succeeded", s.awaitRun(jobA.ID, runA, runTimeout).Status)

	key := "replay-key-" + time.Now().Format("150405.000000000")
	first := s.mustPostReplay(jobA.ID, runA, &key, `{"set":{}}`)
	s.NotEmpty(first.RunID)
	s.True(first.Quarantine)
	firstRun := s.awaitRun(jobA.ID, first.RunID, runTimeout)
	s.Equal("succeeded", firstRun.Status)
	s.True(firstRun.Quarantine)

	second := s.mustPostReplay(jobA.ID, runA, &key, `{"set":{}}`)
	s.Equal(first.RunID, second.RunID, "same scoped fingerprint must return the existing replay run")
	secondRun := s.fetchRun(jobA.ID, second.RunID)
	s.True(secondRun.Quarantine)

	distinctKey := key + "-distinct"
	third := s.mustPostReplay(jobA.ID, runA, &distinctKey, `{"set":{}}`)
	s.NotEqual(first.RunID, third.RunID, "changed idempotency key must derive a distinct fingerprint")
	thirdRun := s.fetchRun(jobA.ID, third.RunID)
	s.True(thirdRun.Quarantine)

	overrideKey := key + "-override-local"
	overrideBody := s.postReplay(jobA.ID, runA, &overrideKey, `{"set":{"mode":"what-if"}}`)
	s.Contains([]int{http.StatusConflict, http.StatusUnprocessableEntity}, overrideBody.status, overrideBody.body)
	s.Contains(overrideBody.body, "distributed execution mode")

	missingBody := s.postReplay(jobA.ID, runA, nil, `{"set":{}}`)
	s.Equal(http.StatusBadRequest, missingBody.status, missingBody.body)
	blank := "   "
	blankBody := s.postReplay(jobA.ID, runA, &blank, `{"set":{}}`)
	s.Equal(http.StatusBadRequest, blankBody.status, blankBody.body)

	rejectKey := key + "-quarantine"
	quarantineBody := s.postReplay(jobA.ID, runA, &rejectKey, `{"set":{},"quarantine":false}`)
	s.Equal(http.StatusBadRequest, quarantineBody.status, quarantineBody.body)

	oversizedKey := key + "-oversized"
	oversizedBody := s.postReplay(jobA.ID, runA, &oversizedKey, `{"set":{"k":"`+strings.Repeat("x", 70*1024)+`"}}`)
	s.GreaterOrEqual(oversizedBody.status, 400, oversizedBody.body)
	s.Less(oversizedBody.status, 500, oversizedBody.body)

	overCapKey := key + "-over-cap"
	overCapBody := s.postReplay(jobA.ID, runA, &overCapKey, replayOverCapSetPayload())
	s.GreaterOrEqual(overCapBody.status, 400, overCapBody.body)
	s.Less(overCapBody.status, 500, overCapBody.body)

	aliasB := fmt.Sprintf("e2e-replay-safe-b-%d", time.Now().UnixNano())
	dirB := s.writeJobManifest(replayManifest(aliasB, true))
	defer os.RemoveAll(dirB)
	s.runCLI("job", "apply", "--path", dirB, "--server", s.caesiumURL)
	jobB := s.requireJobByAlias(aliasB)
	s.Require().NotNil(jobB)

	beforeA := len(s.fetchRuns(jobA.ID))
	beforeB := len(s.fetchRuns(jobB.ID))
	crossKey := key + "-cross"
	crossBody := s.postReplay(jobB.ID, runA, &crossKey, `{"set":{}}`)
	s.Contains([]int{http.StatusNotFound, http.StatusForbidden}, crossBody.status, crossBody.body)
	s.Len(s.fetchRuns(jobA.ID), beforeA, "cross-job replay must not create a replay under the baseline owner")
	s.Len(s.fetchRuns(jobB.ID), beforeB, "cross-job replay must not create a replay under the path job")

	aliasUnsafe := fmt.Sprintf("e2e-replay-unsafe-%d", time.Now().UnixNano())
	dirUnsafe := s.writeJobManifest(replayManifest(aliasUnsafe, false))
	defer os.RemoveAll(dirUnsafe)
	s.runCLI("job", "apply", "--path", dirUnsafe, "--server", s.caesiumURL)
	jobUnsafe := s.requireJobByAlias(aliasUnsafe)
	s.Require().NotNil(jobUnsafe)
	runUnsafe := s.triggerRun(jobUnsafe.ID)
	s.Require().Equal("succeeded", s.awaitRun(jobUnsafe.ID, runUnsafe, runTimeout).Status)

	unsafeKey := key + "-unsafe"
	unsafeBody := s.postReplay(jobUnsafe.ID, runUnsafe, &unsafeKey, `{"set":{"mode":"force-rerun"}}`)
	s.Equal(http.StatusUnprocessableEntity, unsafeBody.status, unsafeBody.body)
	s.Contains(unsafeBody.body, "deploy")
	s.Len(s.fetchRuns(jobUnsafe.ID), 1, "unsafe replay refusal must not materialize a replay run")
}

func (s *IntegrationTestSuite) TestReplayRESTEndpointConcurrentIdempotency() {
	alias := fmt.Sprintf("e2e-replay-concurrent-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayManifest(alias, true))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	baselineRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baselineRunID, runTimeout).Status)

	const workers = 8
	key := "replay-concurrent-" + time.Now().Format("150405.000000000")
	start := make(chan struct{})
	var wg sync.WaitGroup
	responses := make([]replayPostBody, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			responses[idx], errs[idx] = s.postReplayNoRequire(job.ID, baselineRunID, &key, `{"set":{}}`)
		}(i)
	}
	close(start)
	wg.Wait()

	runIDs := make(map[string]struct{})
	for i, response := range responses {
		s.Require().NoError(errs[i])
		s.Equal(http.StatusAccepted, response.status, response.body)
		var decoded replayResponse
		s.Require().NoError(json.Unmarshal([]byte(response.body), &decoded))
		s.NotEmpty(decoded.RunID)
		runIDs[decoded.RunID] = struct{}{}
	}
	s.Len(runIDs, 1, "parallel identical replays must all resolve to one replay run")
	for runID := range runIDs {
		run := s.fetchRun(job.ID, runID)
		s.True(run.Quarantine)
		s.Equal("succeeded", run.Status)
	}
}

func (s *IntegrationTestSuite) TestReplayRESTEndpointBaselineWithoutTaskRunsIs4xx() {
	alias := fmt.Sprintf("e2e-replay-empty-%d", time.Now().UnixNano())
	jobID := s.createEmptyReplayJob(alias)
	baselineRunID := s.triggerRun(jobID)
	s.Require().Equal("failed", s.awaitRun(jobID, baselineRunID, runTimeout).Status)

	key := "replay-empty-" + time.Now().Format("150405.000000000")
	response := s.postReplay(jobID, baselineRunID, &key, `{"set":{}}`)
	s.GreaterOrEqual(response.status, 400, response.body)
	s.Less(response.status, 500, response.body)
	s.NotEqual(http.StatusInternalServerError, response.status, response.body)
}

type replayPostBody struct {
	status int
	body   string
}

func (s *IntegrationTestSuite) mustPostReplay(jobID, runID string, key *string, payload string) replayResponse {
	s.T().Helper()
	observed := s.postReplay(jobID, runID, key, payload)
	s.Require().Equal(http.StatusAccepted, observed.status, observed.body)
	var fields map[string]json.RawMessage
	s.Require().NoError(json.Unmarshal([]byte(observed.body), &fields))
	s.NotContains(fields, "id")
	var decoded replayResponse
	s.Require().NoError(json.Unmarshal([]byte(observed.body), &decoded))
	s.Require().NotEmpty(decoded.RunID)
	return decoded
}

func (s *IntegrationTestSuite) fetchRun(jobID, runID string) runResponse {
	s.T().Helper()
	var run runResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run)
	return run
}

func (s *IntegrationTestSuite) postReplay(jobID, runID string, key *string, payload string) replayPostBody {
	s.T().Helper()
	observed, err := s.postReplayNoRequire(jobID, runID, key, payload)
	s.Require().NoError(err)
	return observed
}

func (s *IntegrationTestSuite) postReplayNoRequire(jobID, runID string, key *string, payload string) (replayPostBody, error) {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/replay", s.caesiumURL, jobID, runID),
		strings.NewReader(payload),
	)
	if err != nil {
		return replayPostBody{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != nil {
		req.Header.Set("Idempotency-Key", *key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return replayPostBody{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return replayPostBody{}, err
	}
	return replayPostBody{status: resp.StatusCode, body: string(body)}, nil
}

func (s *IntegrationTestSuite) createEmptyReplayJob(alias string) string {
	s.T().Helper()
	payload := fmt.Sprintf(`{
		"alias": %q,
		"trigger": {"type": "cron", "configuration": {"cron": "0 2 * * *"}},
		"tasks": []
	}`, alias)
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%v/v1/jobs", s.caesiumURL), bytes.NewBufferString(payload))
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

func replayOverCapSetPayload() string {
	var b strings.Builder
	b.WriteString(`{"set":{`)
	for i := 0; i < 257; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%q", fmt.Sprintf("key-%03d", i), "value")
	}
	b.WriteString(`}}`)
	return b.String()
}

func replayManifest(alias string, replaySafe bool) string {
	replaySafeLine := ""
	if replaySafe {
		replaySafeLine = "\n  replaySafe: true"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s%s
  cache:
    ttl: "1h"
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: deploy
    image: alpine:3.23
    command: ["sh","-c","echo deploy"]
`, alias, replaySafeLine)
}
