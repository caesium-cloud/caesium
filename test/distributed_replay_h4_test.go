//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// replayHealthSamples takes one scrape of the production run, task, retry and
// cache series attributable to this job. Unlabelled process-wide totals can
// also change because of unrelated jobs on the shared integration server.
func (s *IntegrationTestSuite) replayHealthSamples(jobID, alias string) map[string]string {
	s.T().Helper()
	body, _ := s.getRaw("/metrics")
	samples := make(map[string]string)
	for line := range strings.SplitSeq(string(body), "\n") {
		series, value, ok := splitPromSample(line)
		if !ok {
			continue
		}
		name, labels, ok := parsePromSeries(series)
		if !ok || (labels["job_id"] != jobID && labels["job_alias"] != alias) {
			continue
		}
		switch name {
		case "caesium_jobs_active", "caesium_job_runs_total", "caesium_job_run_duration_seconds_count",
			"caesium_task_runs_total", "caesium_task_run_duration_seconds_count", "caesium_task_retries_total",
			"caesium_task_cache_hits_total", "caesium_task_cache_misses_total", "caesium_task_cache_short_circuits_total":
			samples[series] = value
		}
	}
	return samples
}

func (s *IntegrationTestSuite) TestDistributedReplayH4DiffRetryAndLiveGauges() {
	if !distributedLane() {
		s.T().Skip("requires the distributed worker lane")
	}
	alias := fmt.Sprintf("h4-replay-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  replaySafe: true
  cache: { ttl: "1h" }
trigger: { type: cron, configuration: { expression: "0 0 31 2 *" } }
steps:
  - name: render
    image: alpine:3.23
    retries: 1
    command:
      - sh
      - -ec
      - |
        case "${CAESIUM_PARAM_MODE}" in
          slow) sleep 10 ;;
          fail) exit 1 ;;
        esac
        printf '##caesium::output {"mode":"%%s"}\n' "$CAESIUM_PARAM_MODE"
`, alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	baselineID := s.triggerRunWithParams(job.ID, map[string]string{"mode": "baseline"})
	baseline := s.awaitRun(job.ID, baselineID, runTimeout)
	s.Require().Equal("succeeded", baseline.Status, baseline.Error)
	s.Require().Len(baseline.Tasks, 1)
	s.Require().Equal("baseline", baseline.Tasks[0].Output["mode"])
	before := s.replayHealthSamples(job.ID, alias)
	active := fmt.Sprintf(`caesium_jobs_active{job_id=%q}`, job.ID)
	s.Require().Equal("0", before[active], "the baseline must establish an idle gauge for this job")

	key := alias + "-slow"
	slow := s.mustPostReplay(job.ID, baselineID, &key, `{"set":{"mode":"slow"}}`)
	live := s.awaitFirstTaskStatus(job.ID, slow.RunID, runTimeout, "running")
	s.Require().True(live.Quarantine)
	s.Require().Equal("running", live.Status)
	during := s.replayHealthSamples(job.ID, alias)
	live = s.fetchRun(job.ID, slow.RunID)
	s.Require().Equal("running", live.Status, "the scrape must finish inside the replay's active window")
	s.Require().Len(live.Tasks, 1)
	s.Require().Equal("running", live.Tasks[0].Status)
	s.Equal(before, during, "a running replay must leave production metrics unchanged")

	done := s.awaitRun(job.ID, slow.RunID, runTimeout)
	s.Require().Equal("succeeded", done.Status, done.Error)
	s.Require().Len(done.Tasks, 1)
	s.Equal("succeeded", done.Tasks[0].Status)
	s.Equal("slow", done.Tasks[0].Output["mode"])
	diff := s.getRunDiff(job.ID, baselineID, slow.RunID)
	s.Equal(baselineID, diff.LeftRunID)
	s.Equal(slow.RunID, diff.RightRunID)
	s.True(hasRunDiffChange(diff.ParamChanges, "params.mode", "baseline", "slow"), "diff: %+v", diff.ParamChanges)
	s.Equal(before, s.replayHealthSamples(job.ID, alias), "successful replay finalization")

	key = alias + "-fail"
	retry := s.mustPostReplay(job.ID, baselineID, &key, `{"set":{"mode":"fail"}}`)
	failed := s.awaitRun(job.ID, retry.RunID, runTimeout)
	s.Require().True(failed.Quarantine)
	s.Require().Equal("failed", failed.Status)
	var detail struct {
		Tasks []struct {
			Status  string `json:"status"`
			Attempt int    `json:"attempt"`
		} `json:"tasks"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, retry.RunID), &detail)
	s.Require().Len(detail.Tasks, 1)
	s.Equal("failed", detail.Tasks[0].Status)
	s.Equal(2, detail.Tasks[0].Attempt, "the worker exhausted the configured retry")
	s.Equal(before, s.replayHealthSamples(job.ID, alias), "retry and failed replay finalization")
	runs := s.fetchRuns(job.ID)
	s.Require().Len(runs, 1)
	s.Equal(baselineID, runs[0].ID)
}
