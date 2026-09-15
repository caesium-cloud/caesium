//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestManualRunRejectsSchedulerOwnedParams() {
	alias := fmt.Sprintf("integration-manual-params-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(triggerChainCronManifest(alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	for _, key := range []string{"_trigger_depth", "_derived_from_dataset", "_consumed_watermarks", "_consumed_watermarks_start", "_future_scheduler_field", "logical_date"} {
		body := fmt.Sprintf(`{"params":{%q:"private-value","mode":"keep-me"}}`, key)
		resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, job.ID), strings.NewReader(body))
		s.Require().NoError(err)
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		s.Require().NoError(readErr)
		s.Require().NoError(closeErr)
		s.Require().Equal(http.StatusBadRequest, resp.StatusCode, string(data))
		s.Contains(string(data), key)
		s.NotContains(string(data), "private-value")
	}
	s.Empty(s.fetchRuns(job.ID), "rejected parameters must not create or enqueue a run")

	want := map[string]string{"mode": "keep-me", "customer_date": "2026-09-01"}
	runID := s.triggerRunWithParams(job.ID, want)
	finished := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", finished.Status)
	s.Equal(want, finished.Params, "manual business inputs must remain unchanged")
}

func (s *IntegrationTestSuite) TestManualTriggerFireRejectsSchedulerOwnedParams() {
	alias := fmt.Sprintf("integration-manual-trigger-params-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata: {alias: %s}
trigger: {type: http, configuration: {path: %s}}
steps:
  - name: run
    image: alpine:3.23
    command: [echo, safe]
`, alias, alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	triggerID := stringFromMap(s.jobDetailByAlias(alias), "trigger_id")
	s.Require().NotEmpty(triggerID)
	endpoint := fmt.Sprintf("%s/v1/triggers/%s/fire", s.caesiumURL, triggerID)

	for _, key := range []string{"_trigger_depth", "_derived_from_dataset", "_consumed_watermarks", "_consumed_watermarks_start", "_future_scheduler_field", "logical_date"} {
		body := fmt.Sprintf(`{"params":{%q:"private-value","mode":"keep-me"}}`, key)
		resp, err := s.doManualTriggerRequest(http.MethodPost, endpoint, strings.NewReader(body))
		s.Require().NoError(err)
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		s.Require().NoError(readErr)
		s.Require().NoError(closeErr)
		s.Require().Equal(http.StatusBadRequest, resp.StatusCode, string(data))
		s.Contains(string(data), key)
		s.NotContains(string(data), "private-value")
	}
	s.Empty(s.fetchRuns(job.ID), "rejected trigger-fire parameters must not create or enqueue a run")

	want := map[string]string{"mode": "keep-me", "customer_date": "2026-09-01"}
	body := `{"params":{"mode":"keep-me","customer_date":"2026-09-01"}}`
	resp, err := s.doManualTriggerRequest(http.MethodPost, endpoint, strings.NewReader(body))
	s.Require().NoError(err)
	data, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	s.Require().NoError(readErr)
	s.Require().NoError(closeErr)
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(data))

	started := s.awaitNewRun(job.ID, 0, 60*time.Second)
	finished := s.awaitRun(job.ID, started.ID, runTimeout)
	s.Equal("succeeded", finished.Status)
	s.Equal(want, finished.Params, "manual trigger-fire business inputs must remain unchanged")
}
