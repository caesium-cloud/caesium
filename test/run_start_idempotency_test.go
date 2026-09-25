//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// run_start_idempotency_test.go drives POST /v1/jobs/:id/run and
// `caesium run start` against the live server the way an at-least-once caller
// (a Temporal activity, a retrying CI job) does: the same Idempotency-Key must
// resolve to one admission, and a start that did not create a run must say
// what happened instead of answering with an empty 202.

type runStartResult struct {
	status   int
	replayed string
	body     map[string]any
}

func (r runStartResult) str(key string) string {
	value, _ := r.body[key].(string)
	return value
}

func (s *IntegrationTestSuite) postRunStart(jobID, key, body string) runStartResult {
	s.T().Helper()
	result, err := s.tryPostRunStart(jobID, key, body)
	s.Require().NoError(err)
	return result
}

// tryPostRunStart is safe to call from goroutines and Eventually callbacks.
func (s *IntegrationTestSuite) tryPostRunStart(jobID, key, body string) (runStartResult, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), reader)
	if err != nil {
		return runStartResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	s.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return runStartResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return runStartResult{}, err
	}
	out := runStartResult{status: resp.StatusCode, replayed: resp.Header.Get("Idempotent-Replayed")}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			return runStartResult{}, fmt.Errorf("decode run start body %q: %w", string(raw), err)
		}
	}
	return out, nil
}

func (s *IntegrationTestSuite) TestRunStartIdempotencyKey() {
	s.Run("retry with the same key returns the original run", func() {
		// strategy fail + maxRuns 1: without idempotency the retry would be
		// refused 409 by the very run its first attempt started.
		job := s.applyConcurrencyJob("fail", `sleep 4`)
		key := "wf-" + job.ID + "/act-1"

		first := s.postRunStart(job.ID, key, `{"params":{"region":"eu"}}`)
		s.Require().Equal(http.StatusAccepted, first.status, first.body)
		s.Equal("created", first.str("outcome"))
		s.Empty(first.replayed)
		runID := first.str("id")
		s.Require().NotEmpty(runID)

		retry := s.postRunStart(job.ID, key, `{"params":{"region":"eu"}}`)
		s.Require().Equal(http.StatusAccepted, retry.status, retry.body)
		s.Equal("true", retry.replayed)
		s.Equal("created", retry.str("outcome"))
		s.Equal(runID, retry.str("id"))

		reused := s.postRunStart(job.ID, key, `{"params":{"region":"us"}}`)
		s.Equal(http.StatusUnprocessableEntity, reused.status, "a key reused for different params must be refused")

		s.Equal("succeeded", s.awaitRunStatus(job.ID, runID, runTimeout, "succeeded").Status)
		s.Len(s.fetchRuns(job.ID), 1, "retries must not admit extra runs")

		// Once the run finished, the retry reports its terminal state.
		late := s.postRunStart(job.ID, key, `{"params":{"region":"eu"}}`)
		s.Equal(runID, late.str("id"))
		s.Equal("succeeded", late.str("status"))
	})

	s.Run("concurrent retries admit exactly one run", func() {
		job := s.applyConcurrencyJob("fail", `sleep 4`)
		key := "race-" + job.ID

		const callers = 5
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]runStartResult, callers)
		errs := make([]error, callers)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = s.tryPostRunStart(job.ID, key, "")
			}()
		}
		close(start)
		wg.Wait()

		runID := ""
		for i := range callers {
			s.Require().NoError(errs[i])
			s.Require().Equal(http.StatusAccepted, results[i].status, results[i].body)
			if runID == "" {
				runID = results[i].str("id")
			}
			s.Equal(runID, results[i].str("id"), "every concurrent retry must see the one run")
		}
		s.Require().NotEmpty(runID)
		s.Len(s.fetchRuns(job.ID), 1)
		s.awaitRunStatus(job.ID, runID, runTimeout, "succeeded")
	})

	s.Run("queued start resolves to the run it becomes", func() {
		job := s.applyConcurrencyJob("queue", `sleep 3`)
		holder := s.postRunStart(job.ID, "", "")
		s.Require().Equal("created", holder.str("outcome"), holder.body)
		holderID := holder.str("id")

		key := "queued-" + job.ID
		queued := s.postRunStart(job.ID, key, "")
		s.Require().Equal(http.StatusAccepted, queued.status)
		s.Equal("queued", queued.str("outcome"))
		s.NotEmpty(queued.str("queue_id"), "a queued start must name its queue entry")
		s.Empty(queued.str("id"), "a queued start has no run yet")

		again := s.postRunStart(job.ID, key, "")
		s.Equal("queued", again.str("outcome"))
		s.Equal("true", again.replayed)
		s.Equal(queued.str("queue_id"), again.str("queue_id"))

		s.awaitRunStatus(job.ID, holderID, runTimeout, "succeeded")
		var promotedID string
		s.Require().Eventually(func() bool {
			res, err := s.tryPostRunStart(job.ID, key, "")
			if err != nil || res.str("outcome") != "created" {
				return false
			}
			promotedID = res.str("id")
			return promotedID != ""
		}, 30*time.Second, time.Second, "the key must follow its queued start into the promoted run")
		s.NotEqual(holderID, promotedID)
		s.Equal("succeeded", s.awaitRunStatus(job.ID, promotedID, runTimeout, "succeeded").Status)
		s.Len(s.fetchRuns(job.ID), 2, "one holder run plus the one promoted start")
	})

	s.Run("skipped start says why", func() {
		job := s.applyConcurrencyJob("skip", `sleep 4`)
		holder := s.postRunStart(job.ID, "", "")
		s.Require().Equal("created", holder.str("outcome"), holder.body)

		skipped := s.postRunStart(job.ID, "", "")
		s.Equal(http.StatusAccepted, skipped.status)
		s.Equal("skipped", skipped.str("outcome"))
		s.Equal("max_concurrency", skipped.str("reason"))
		s.Empty(skipped.str("id"))
		s.awaitRunStatus(job.ID, holder.str("id"), runTimeout, "succeeded")
	})
}

// TestRunStartIdempotencyKeyCLI drives `caesium run start --idempotency-key`
// through the real binary, capturing stdout apart from stderr: a retried
// command must print the same run ID, and only the run ID, on stdout.
func (s *IntegrationTestSuite) TestRunStartIdempotencyKeyCLI() {
	job := s.applyConcurrencyJob("fail", `sleep 3`)
	key := "cli-" + job.ID
	args := []string{
		"run", "start",
		"--job-id", job.ID,
		"--idempotency-key", key,
		"--server", s.caesiumURL,
	}

	firstOut, firstErr, err := s.runCLISeparate(args...)
	s.Require().NoError(err, "caesium run start failed:\n%s", firstErr)
	runID := strings.TrimSpace(firstOut)
	s.Require().NotEmpty(runID)
	s.NotContains(runID, "\n", "stdout must be the bare run ID")

	secondOut, secondErr, err := s.runCLISeparate(args...)
	s.Require().NoError(err, "a retried start must not fail on its own concurrency slot:\n%s", secondErr)
	s.Equal(runID+"\n", secondOut, "a retried start must print the original run ID and nothing else")
	s.Contains(secondErr, "matched an earlier start")

	s.Equal("succeeded", s.awaitRunStatus(job.ID, runID, runTimeout, "succeeded").Status)
	s.Len(s.fetchRuns(job.ID), 1)
}
