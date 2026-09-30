//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// run_list_counters_test.go pins issue #489: GET /v1/jobs/:id/runs used to
// report cache_hits/executed_tasks/total_tasks as zero for every run because
// internal/run/store.go's List called Preload("Tasks") ahead of a Scan(),
// which never populates the association — so the counters were computed from
// an empty task slice while GET /v1/jobs/:id/runs/:run_id (which loads real
// TaskRun rows) reported the true numbers. These tests run real jobs and
// assert the LIST entry's counters equal the DETAIL endpoint's for an
// executed run, a run with a real cache hit, and a failed run.

// TestRunListCountersReconcileWithDetailAcrossCacheAndFailure applies a
// cache-enabled two-step job, runs it twice (first execution, second a real
// cache hit), and separately runs a job with one failing step — then
// compares each run's list-entry counters against its own detail endpoint.
func (s *IntegrationTestSuite) TestRunListCountersReconcileWithDetailAcrossCacheAndFailure() {
	alias := fmt.Sprintf("integration-run-list-counters-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: alpha
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"1\"}'"]
  - name: beta
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"2\"}'"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: nothing to cache from yet, so both tasks execute fresh.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Require().Equal("succeeded", run1.Status)
	s.Equal(0, run1.CacheHits, "a job's first run has nothing to cache from")
	s.Equal(2, run1.ExecutedTasks)
	s.Equal(2, run1.TotalTasks)

	// Second run: identical deterministic inputs, so at least the leading
	// task (which has no predecessor whose hash could have shifted) must be
	// a real cache hit.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Require().Equal("succeeded", run2.Status)
	s.Equal(2, run2.TotalTasks)
	s.Greater(run2.CacheHits, 0, "identical second run must record a real cache hit in its own detail")
	s.Equal(2, run2.CacheHits+run2.ExecutedTasks, "every task is either cached or executed")

	// A failing run for the third counter combination.
	failAlias := fmt.Sprintf("integration-run-list-counters-fail-%d", time.Now().UnixNano())
	failManifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: boom
    image: alpine:3.23
    command: ["sh", "-c", "exit 1"]
`, failAlias)
	failDir := s.writeJobManifest(failManifest)
	defer os.RemoveAll(failDir)
	s.runCLI("job", "apply", "--path", failDir, "--server", s.caesiumURL)
	failJob := s.requireJobByAlias(failAlias)
	s.Require().NotNil(failJob)

	failRunID := s.triggerRun(failJob.ID)
	failRun := s.awaitRun(failJob.ID, failRunID, runTimeout)
	s.Require().Equal("failed", failRun.Status)
	s.Equal(0, failRun.CacheHits)
	s.Equal(1, failRun.ExecutedTasks, "a failed task still counts as executed")
	s.Equal(1, failRun.TotalTasks)

	// Now reconcile: the LIST entry for each of the three runs must report
	// exactly what its own DETAIL endpoint reports — not a measured zero.
	listRuns := s.fetchRuns(job.ID)
	listByID := make(map[string]runResponse, len(listRuns))
	for _, r := range listRuns {
		listByID[r.ID] = r
	}

	list1, ok := listByID[run1ID]
	s.Require().True(ok, "first run must appear in the list")
	s.Equal(run1.CacheHits, list1.CacheHits, "list cache_hits must match detail for the executed run")
	s.Equal(run1.ExecutedTasks, list1.ExecutedTasks)
	s.Equal(run1.TotalTasks, list1.TotalTasks)

	list2, ok := listByID[run2ID]
	s.Require().True(ok, "second run must appear in the list")
	s.Equal(run2.CacheHits, list2.CacheHits, "list cache_hits must match detail for the cached run")
	s.Equal(run2.ExecutedTasks, list2.ExecutedTasks)
	s.Equal(run2.TotalTasks, list2.TotalTasks)

	failList := s.fetchRuns(failJob.ID)
	s.Require().Len(failList, 1)
	s.Equal(failRun.CacheHits, failList[0].CacheHits, "list cache_hits must match detail for the failed run")
	s.Equal(failRun.ExecutedTasks, failList[0].ExecutedTasks)
	s.Equal(failRun.TotalTasks, failList[0].TotalTasks)
}
