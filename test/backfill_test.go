//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// --------------------------------------------------------------------------
// Types
// --------------------------------------------------------------------------

type backfillResponse struct {
	ID            string     `json:"id"`
	JobID         string     `json:"job_id"`
	Status        string     `json:"status"`
	Start         time.Time  `json:"start"`
	End           time.Time  `json:"end"`
	MaxConcurrent int        `json:"max_concurrent"`
	Reprocess     string     `json:"reprocess"`
	TotalRuns     int        `json:"total_runs"`
	CompletedRuns int        `json:"completed_runs"`
	FailedRuns    int        `json:"failed_runs"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

type backfillRunSummary struct {
	ID         string            `json:"id"`
	JobID      string            `json:"job_id"`
	BackfillID string            `json:"backfill_id,omitempty"`
	Status     string            `json:"status"`
	Params     map[string]string `json:"params,omitempty"`
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// createBackfill sends POST /v1/jobs/:id/backfill and returns the created record.
func (s *IntegrationTestSuite) createBackfill(jobID string, start, end time.Time, maxConcurrent int, reprocess string) *backfillResponse {
	s.T().Helper()

	body := map[string]interface{}{
		"start":          start,
		"end":            end,
		"max_concurrent": maxConcurrent,
		"reprocess":      reprocess,
	}
	payload, err := json.Marshal(body)
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/backfill", s.caesiumURL, jobID),
		bytes.NewReader(payload),
	)
	s.Require().NoError(err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	s.Require().Equalf(http.StatusAccepted, resp.StatusCode,
		"createBackfill: unexpected status %d: %s", resp.StatusCode, string(respBody))

	var b backfillResponse
	s.Require().NoError(json.Unmarshal(respBody, &b))
	return &b
}

// awaitBackfill polls GET /v1/jobs/:id/backfills/:backfill_id until the backfill
// leaves the "running" state or the timeout elapses.
func (s *IntegrationTestSuite) awaitBackfill(jobID, backfillID string, timeout time.Duration) *backfillResponse {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for backfill %s to leave running state", backfillID)
		}

		var b backfillResponse
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/backfills/%s", jobID, backfillID), &b)
		if b.Status != "running" {
			return &b
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// listBackfills returns all backfills for a job.
func (s *IntegrationTestSuite) listBackfills(jobID string) []backfillResponse {
	s.T().Helper()
	var bs []backfillResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/backfills", jobID), &bs)
	return bs
}

// listJobRunSummaries returns all runs for a job as backfillRunSummary records.
func (s *IntegrationTestSuite) listJobRunSummaries(jobID string) []backfillRunSummary {
	s.T().Helper()
	var runs []backfillRunSummary
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs", jobID), &runs)
	return runs
}

// tryListJobRunSummaries is like listJobRunSummaries but returns an error
// instead of calling require.  Safe for use inside Eventually/Never callbacks
// which testify runs in a separate goroutine.
func (s *IntegrationTestSuite) tryListJobRunSummaries(jobID string) ([]backfillRunSummary, error) {
	var runs []backfillRunSummary
	err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs", jobID), &runs)
	return runs, err
}

func countBackfillRuns(runs []backfillRunSummary, backfillID string) int {
	count := 0
	for _, run := range runs {
		if run.BackfillID == backfillID {
			count++
		}
	}
	return count
}

// cancelBackfillRequest sends PUT /v1/jobs/:id/backfills/:backfill_id/cancel
// and returns the HTTP response.
func (s *IntegrationTestSuite) cancelBackfillRequest(jobID, backfillID string) *http.Response {
	s.T().Helper()
	resp, err := s.doJSONRequest(
		http.MethodPut,
		fmt.Sprintf("%s/v1/jobs/%s/backfills/%s/cancel", s.caesiumURL, jobID, backfillID),
		nil,
	)
	s.Require().NoError(err)
	return resp
}

// backfillJobManifest returns a minimal job YAML definition for backfill tests.
func backfillJobManifest(alias, cronExpr, taskCmd string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: %q
steps:
  - name: run
    image: alpine:3.20
    command: ["sh", "-c", %q]
`, alias, cronExpr, taskCmd)
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

// TestBackfillBasicHappyPath creates a backfill for a 2-hour window with a
// 30-minute cron schedule (4 fire times) and verifies it completes successfully.
func (s *IntegrationTestSuite) TestBackfillBasicHappyPath() {
	alias := fmt.Sprintf("integration-backfill-basic-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "*/30 * * * *", "echo $CAESIUM_PARAM_LOGICAL_DATE"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	// 2024-01-01 00:00 → 02:00 UTC: fires at 00:00, 00:30, 01:00, 01:30 → 4 dates.
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)

	b := s.createBackfill(job.ID, start, end, 2, "none")
	s.Equal("running", b.Status)
	s.Equal(job.ID, b.JobID)
	s.Equal("none", b.Reprocess)
	s.Equal(2, b.MaxConcurrent)

	result := s.awaitBackfill(job.ID, b.ID, 3*time.Minute)

	s.Equal("succeeded", result.Status, "backfill should succeed")
	s.Equal(4, result.TotalRuns, "should queue 4 logical dates")
	s.Equal(4, result.CompletedRuns, "all 4 runs should complete")
	s.Equal(0, result.FailedRuns, "no runs should fail")
	s.NotNil(result.CompletedAt, "completed_at should be set when done")
}

// TestBackfillListAndGet verifies that created backfills appear in the list
// endpoint and can be retrieved individually by ID.
func (s *IntegrationTestSuite) TestBackfillListAndGet() {
	alias := fmt.Sprintf("integration-backfill-list-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hello"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC) // 3 hourly dates

	created := s.createBackfill(job.ID, start, end, 1, "none")
	s.Require().NotEmpty(created.ID)

	// GET /v1/jobs/:id/backfills/:backfill_id returns the correct record.
	var fetched backfillResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/backfills/%s", job.ID, created.ID), &fetched)
	s.Equal(created.ID, fetched.ID)
	s.Equal(job.ID, fetched.JobID)
	s.Equal("none", fetched.Reprocess)

	// Wait for completion so the list reflects the final state.
	s.awaitBackfill(job.ID, created.ID, 3*time.Minute)

	// GET /v1/jobs/:id/backfills returns the backfill in the list.
	backfills := s.listBackfills(job.ID)
	s.Require().NotEmpty(backfills)
	var found bool
	for _, bf := range backfills {
		if bf.ID == created.ID {
			found = true
			s.Equal(job.ID, bf.JobID)
			break
		}
	}
	s.True(found, "created backfill should appear in list for job %s", job.ID)
}

// TestBackfillLogicalDateParam verifies that each backfill run receives a
// logical_date parameter matching the cron fire time, and that runs are linked
// to the backfill via BackfillID.
func (s *IntegrationTestSuite) TestBackfillLogicalDateParam() {
	alias := fmt.Sprintf("integration-backfill-param-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo $CAESIUM_PARAM_LOGICAL_DATE"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	// 2-hour window with hourly schedule → 2 dates: 00:00 and 01:00.
	start := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 6, 1, 2, 0, 0, 0, time.UTC)

	b := s.createBackfill(job.ID, start, end, 1, "none")
	result := s.awaitBackfill(job.ID, b.ID, 3*time.Minute)
	s.Require().Equal("succeeded", result.Status)
	s.Equal(2, result.TotalRuns)

	// Verify each run carries the correct logical_date param.
	runs := s.listJobRunSummaries(job.ID)
	s.Require().NotEmpty(runs, "runs should be present after backfill completes")

	logicalDates := make(map[string]bool)
	for _, r := range runs {
		if r.BackfillID == b.ID && r.Params != nil {
			if ld := r.Params["logical_date"]; ld != "" {
				logicalDates[ld] = true
			}
		}
	}
	s.Equal(2, len(logicalDates), "should have exactly 2 distinct logical dates")
	s.True(logicalDates["2024-06-01T00:00:00Z"], "logical_date 00:00 should be present")
	s.True(logicalDates["2024-06-01T01:00:00Z"], "logical_date 01:00 should be present")
}

// TestBackfillReprocessNone verifies that a second backfill over the same
// window with reprocess=none produces 0 new runs (all dates already have runs).
func (s *IntegrationTestSuite) TestBackfillReprocessNone() {
	alias := fmt.Sprintf("integration-backfill-reprocess-none-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hello"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	start := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 1, 3, 0, 0, 0, time.UTC) // 3 hourly dates

	// First backfill: runs 3 dates.
	b1 := s.createBackfill(job.ID, start, end, 2, "none")
	r1 := s.awaitBackfill(job.ID, b1.ID, 3*time.Minute)
	s.Require().Equal("succeeded", r1.Status)
	s.Equal(3, r1.TotalRuns)
	s.Equal(3, r1.CompletedRuns)

	// Second backfill with reprocess=none over the same window.
	// All 3 dates have successful runs → total_runs should be 0.
	b2 := s.createBackfill(job.ID, start, end, 1, "none")
	r2 := s.awaitBackfill(job.ID, b2.ID, 30*time.Second)
	s.Equal("succeeded", r2.Status, "empty backfill should still succeed")
	s.Equal(0, r2.TotalRuns, "reprocess=none should skip all already-run dates")
	s.Equal(0, r2.CompletedRuns)
}

// TestBackfillReprocessAll verifies that reprocess=all re-queues every date in
// the window regardless of existing successful runs.
func (s *IntegrationTestSuite) TestBackfillReprocessAll() {
	alias := fmt.Sprintf("integration-backfill-reprocess-all-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hello"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	start := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 1, 2, 0, 0, 0, time.UTC) // 2 hourly dates

	// First backfill.
	b1 := s.createBackfill(job.ID, start, end, 1, "none")
	r1 := s.awaitBackfill(job.ID, b1.ID, 3*time.Minute)
	s.Require().Equal("succeeded", r1.Status)
	s.Equal(2, r1.TotalRuns)

	// Second backfill with reprocess=all: should queue the 2 dates again.
	b2 := s.createBackfill(job.ID, start, end, 1, "all")
	r2 := s.awaitBackfill(job.ID, b2.ID, 3*time.Minute)
	s.Equal("succeeded", r2.Status)
	s.Equal(2, r2.TotalRuns, "reprocess=all should re-queue all 2 dates")
	s.Equal(2, r2.CompletedRuns)
}

// TestBackfillCancel starts a large backfill backed by a slow task and cancels
// it before completion, verifying the status transitions to "cancelled".
func (s *IntegrationTestSuite) TestBackfillCancel() {
	alias := fmt.Sprintf("integration-backfill-cancel-%d", time.Now().UnixNano())
	// sleep 30 ensures the first container is still running when we cancel,
	// so there is definitely at least one in-flight run.
	dir := s.writeJobManifest(backfillJobManifest(alias, "* * * * *", "sleep 30"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	// 5-hour window at per-minute schedule → 300 dates; max_concurrent=1
	// means at most one container at a time, so the backfill cannot complete
	// within the time we allow before cancelling.
	start := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 4, 1, 5, 0, 0, 0, time.UTC)

	b := s.createBackfill(job.ID, start, end, 1, "none")
	s.Require().Equal("running", b.Status)

	var runsBeforeCancel int
	s.Require().Eventually(func() bool {
		runs, err := s.tryListJobRunSummaries(job.ID)
		if err != nil {
			return false
		}
		runsBeforeCancel = countBackfillRuns(runs, b.ID)
		return runsBeforeCancel == 1
	}, 10*time.Second, 200*time.Millisecond, "expected exactly one in-flight run before cancellation")

	cancelResp := s.cancelBackfillRequest(job.ID, b.ID)
	defer cancelResp.Body.Close()
	s.Require().Equal(http.StatusOK, cancelResp.StatusCode)

	// Allow up to 90 s for the single in-flight container (sleep 30) to finish
	// and for the backfill goroutine to mark the record cancelled.
	result := s.awaitBackfill(job.ID, b.ID, 90*time.Second)
	s.Equal("cancelled", result.Status, "backfill should be marked cancelled")

	runsAfterCancel := countBackfillRuns(s.listJobRunSummaries(job.ID), b.ID)
	s.Equal(runsBeforeCancel, runsAfterCancel, "cancel should not launch any additional backfill runs")

	s.Require().Never(func() bool {
		runs, err := s.tryListJobRunSummaries(job.ID)
		if err != nil {
			return false // treat transient errors as "condition not satisfied"
		}
		return countBackfillRuns(runs, b.ID) > runsBeforeCancel
	}, 2*time.Second, 100*time.Millisecond, "no late backfill runs should appear after cancellation")
}

// TestBackfillValidationEndBeforeStart verifies that end ≤ start returns 400.
func (s *IntegrationTestSuite) TestBackfillValidationEndBeforeStart() {
	alias := fmt.Sprintf("integration-backfill-validation-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hi"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	body := map[string]interface{}{
		"start": time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC),
		"end":   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), // end before start
	}
	payload, _ := json.Marshal(body)

	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/backfill", s.caesiumURL, job.ID),
		bytes.NewReader(payload),
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}

// TestBackfillValidationHTTPTrigger verifies that a job with an HTTP trigger
// returns 422 when a backfill is requested (backfill requires a cron trigger).
func (s *IntegrationTestSuite) TestBackfillValidationHTTPTrigger() {
	alias := fmt.Sprintf("integration-backfill-http-trigger-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: /webhook/%s
steps:
  - name: run
    image: alpine:3.20
    command: ["echo", "hi"]
`, alias, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	body := map[string]interface{}{
		"start": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		"end":   time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC),
	}
	payload, _ := json.Marshal(body)

	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/backfill", s.caesiumURL, job.ID),
		bytes.NewReader(payload),
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusUnprocessableEntity, resp.StatusCode)
}

// TestBackfillValidationPausedJob verifies that initiating a backfill on a
// paused job returns 409 Conflict.
func (s *IntegrationTestSuite) TestBackfillValidationPausedJob() {
	alias := fmt.Sprintf("integration-backfill-paused-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hi"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	// Pause the job first.
	pauseResp, err := s.doJSONRequest(
		http.MethodPut,
		fmt.Sprintf("%s/v1/jobs/%s/pause", s.caesiumURL, job.ID),
		nil,
	)
	s.Require().NoError(err)
	defer pauseResp.Body.Close()
	s.Require().Equal(http.StatusOK, pauseResp.StatusCode)

	body := map[string]interface{}{
		"start": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		"end":   time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC),
	}
	payload, _ := json.Marshal(body)

	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/backfill", s.caesiumURL, job.ID),
		bytes.NewReader(payload),
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusConflict, resp.StatusCode)
}

// TestBackfillGetNotFound verifies that GET on an unknown backfill ID returns 404.
func (s *IntegrationTestSuite) TestBackfillGetNotFound() {
	alias := fmt.Sprintf("integration-backfill-404-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hi"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	resp, err := s.doRequest(
		http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/backfills/00000000-0000-0000-0000-000000000000", s.caesiumURL, job.ID),
		nil,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusNotFound, resp.StatusCode)
}

// TestBackfillCancelAlreadyDone verifies that cancelling a completed backfill
// returns 409 Conflict.
func (s *IntegrationTestSuite) TestBackfillCancelAlreadyDone() {
	alias := fmt.Sprintf("integration-backfill-cancel-done-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(backfillJobManifest(alias, "0 * * * *", "echo hi"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	// 1-hour window → 1 date.  Wait for completion before trying to cancel.
	start := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 5, 1, 1, 0, 0, 0, time.UTC)

	b := s.createBackfill(job.ID, start, end, 1, "none")
	done := s.awaitBackfill(job.ID, b.ID, 3*time.Minute)
	s.Require().Equal("succeeded", done.Status)

	// Cancel on a completed backfill → 409.
	cancelResp := s.cancelBackfillRequest(job.ID, b.ID)
	defer cancelResp.Body.Close()
	s.Equal(http.StatusConflict, cancelResp.StatusCode)
}
