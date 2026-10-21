//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Namespace run quota scenarios (identity-and-access D1 + D2, design §10.3).
//
// Every integration server serves a policy fixture that caps `marketing` at
// ONE concurrent run (test/testdata/policy/two-namespaces.yaml on the
// integration lanes, coverage-secret-glob.yaml on the coverage journey), so
// these scenarios own marketing's single slot while they run: each starts a
// holder run, observes what the quota does to the next start, and leaves the
// namespace idle.

// quotaFixtures are the CAESIUM_IT_POLICY_FIXTURE values whose policy caps
// marketing at maxConcurrentRuns 1.
var quotaFixtures = map[string]bool{"two-namespaces": true, "coverage-secret-glob": true}

// requireMarketingQuota gates a scenario on a server whose policy caps
// marketing at one concurrent run, and FAILS (never skips) when the lane says
// it serves a policy fixture that does not.
func (s *IntegrationTestSuite) requireMarketingQuota() {
	s.T().Helper()
	s.requirePolicyFixture()
	if onAuthLane() {
		return
	}
	fixture := strings.TrimSpace(os.Getenv(policyFixtureEnv))
	s.Require().True(quotaFixtures[fixture],
		"%s=%q: this lane's policy fixture must cap marketing at maxConcurrentRuns 1 for the quota scenarios", policyFixtureEnv, fixture)
}

// startResponse decodes POST /v1/jobs/:id/run: a created run, or a
// StartOutcomeResponse for a queued/skipped start.
type startResponse struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	QueueID string `json:"queue_id"`
	Status  string `json:"status"`
}

func (s *IntegrationTestSuite) postStart(jobID string) startResponse {
	s.T().Helper()
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "start %s: %s", jobID, body)
	var out startResponse
	s.Require().NoError(json.Unmarshal(body, &out), "start %s: %s", jobID, body)
	return out
}

// quotaDefinition is a policy-free marketing job whose single step runs
// command. Its cron never fires.
func (s *IntegrationTestSuite) quotaDefinition(alias, namespace, command string) schema.Definition {
	step := schema.Step{Name: "work", Image: "alpine:3.23", Command: []string{"sh", "-c", command}}
	if s.engineType != "" && s.engineType != "docker" {
		step.Engine = s.engineType
	}
	return schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:      []schema.Step{step},
	}
}

func (s *IntegrationTestSuite) applyQuotaJob(alias, namespace, command string) *jobSummary {
	s.T().Helper()
	s.applyDefinition(s.quotaDefinition(alias, namespace, command))
	return s.requireJobByAlias(alias)
}

// startHolder starts a run that occupies its namespace's slot and waits until
// it is running. A run another scenario left behind may make the holder wait
// in the queue first; it is promoted when that run ends.
func (s *IntegrationTestSuite) startHolder(jobID string) string {
	s.T().Helper()
	started := s.postStart(jobID)
	if started.ID != "" {
		return started.ID
	}
	s.Require().Equal("queued", started.Outcome, "holder start: %+v", started)
	var runID string
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(jobID) {
			if run.Status == "running" {
				runID = run.ID
				return true
			}
		}
		return false
	}, runTimeout, 250*time.Millisecond, "the holder run never started")
	return runID
}

// startAndAwait starts a run and waits for it to finish, whether it started at
// once or first waited in the queue behind a run another scenario left.
func (s *IntegrationTestSuite) startAndAwait(jobID string) *runResponse {
	s.T().Helper()
	started := s.postStart(jobID)
	if started.ID != "" {
		return s.awaitRun(jobID, started.ID, runTimeout)
	}
	s.Require().Equal("queued", started.Outcome, "start: %+v", started)
	var runID string
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(jobID)
		if len(runs) == 0 {
			return false
		}
		runID = runs[0].ID
		return true
	}, runTimeout, 250*time.Millisecond, "the queued run never started")
	return s.awaitRun(jobID, runID, runTimeout)
}

// awaitQueueEmpty waits until a job's run_queue holds no row.
func (s *IntegrationTestSuite) awaitQueueEmpty(jobID string) {
	s.T().Helper()
	s.Require().Eventually(func() bool {
		var rows []queueCLIItem
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/queue", jobID), &rows)
		return len(rows) == 0
	}, runTimeout, 250*time.Millisecond, "job %s still has queued runs", jobID)
}

func (s *IntegrationTestSuite) jobQueueREST(jobID string) []queueCLIItem {
	s.T().Helper()
	var rows []queueCLIItem
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/queue", jobID), &rows)
	return rows
}

// requireNamespaceQuota409 asserts a 409 namespace_quota refusal with
// Retry-After.
func (s *IntegrationTestSuite) requireNamespaceQuota409(resp *http.Response, what string) {
	s.T().Helper()
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusConflict, resp.StatusCode, "%s: %s", what, body)
	s.Equal("5", resp.Header.Get("Retry-After"), "%s must advertise Retry-After", what)
	var refusal struct {
		Error             string `json:"error"`
		Namespace         string `json:"namespace"`
		MaxConcurrentRuns int    `json:"max_concurrent_runs"`
		Message           string `json:"message"`
	}
	s.Require().NoError(json.Unmarshal(body, &refusal), "%s: %s", what, body)
	s.Equal("namespace_quota", refusal.Error, what)
	s.Equal("marketing", refusal.Namespace, what)
	s.Equal(1, refusal.MaxConcurrentRuns, what)
}

// TestNamespaceQuotaHoldsSecondPolicyFreeRun is D2's scenario: with marketing's
// single slot taken, a second policy-free job's start is parked with
// held_reason namespace_quota — visible through `caesium job queue` (table and
// --json) and REST — and runs once the first finishes.
func (s *IntegrationTestSuite) TestNamespaceQuotaHoldsSecondPolicyFreeRun() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	holder := s.applyQuotaJob(fmt.Sprintf("quota-holder-%d", suffix), "marketing", "sleep 15")
	waiter := s.applyQuotaJob(fmt.Sprintf("quota-waiter-%d", suffix), "marketing", "echo released")

	holderRun := s.startHolder(holder.ID)
	parked := s.postStart(waiter.ID)
	s.Require().Empty(parked.ID, "the second run must not start while marketing is at its quota: %+v", parked)
	s.Equal("queued", parked.Outcome)
	s.Equal("namespace_quota", parked.Reason)
	s.Require().NotEmpty(parked.QueueID)

	rows := runCLIJSON[[]queueCLIItem](s, "job", "queue", waiter.Alias, "--json", "--server", s.caesiumURL)
	s.Require().Len(rows, 1)
	s.Equal(parked.QueueID, rows[0].ID)
	s.Equal("namespace_quota", rows[0].HeldReason)
	rest := s.jobQueueREST(waiter.ID)
	s.Require().Len(rest, 1)
	s.Equal("namespace_quota", rest[0].HeldReason)

	stdout, stderr, err := s.runCLISeparate("job", "queue", waiter.Alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium job queue: %s", stderr)
	lines := nonEmptyLines(stdout)
	s.Require().GreaterOrEqual(len(lines), 2, stdout)
	s.Equal([]string{"POSITION", "PRIORITY", "STATE", "REASON", "ENQUEUED_AT", "PARAMS"}, strings.Fields(lines[0]))
	s.Equal("namespace_quota", strings.Fields(lines[1])[3])
	s.Contains(stdout, "wait on their namespace's concurrent-run quota")

	s.awaitRun(holder.ID, holderRun, runTimeout)
	s.awaitQueueEmpty(waiter.ID)
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(waiter.ID) {
			if run.Status == "succeeded" {
				return true
			}
		}
		return false
	}, runTimeout, 500*time.Millisecond, "the parked run must start and finish once marketing frees")
}

// TestNamespaceQuotaConcurrentStartsAdmitOne races starts of four policy-free
// marketing jobs against the real (dqlite) server: exactly one is admitted,
// the rest are parked, and they then run one at a time until all finish.
func (s *IntegrationTestSuite) TestNamespaceQuotaConcurrentStartsAdmitOne() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	// Take the slot once first, so a run another scenario left behind cannot
	// split the race.
	gate := s.applyQuotaJob(fmt.Sprintf("quota-race-gate-%d", suffix), "marketing", "sleep 6")
	gateRun := s.startHolder(gate.ID)
	s.awaitRun(gate.ID, gateRun, runTimeout)

	jobs := make([]*jobSummary, 4)
	for i := range jobs {
		jobs[i] = s.applyQuotaJob(fmt.Sprintf("quota-race-%d-%d", i, suffix), "marketing", "sleep 3")
	}
	results := make([]startResponse, len(jobs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range jobs {
		wg.Go(func() {
			<-start
			resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobs[i].ID), nil)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			_ = json.NewDecoder(resp.Body).Decode(&results[i])
		})
	}
	close(start)
	wg.Wait()
	created, queued := 0, 0
	for i, result := range results {
		switch {
		case result.ID != "":
			created++
		case result.Outcome == "queued" && result.Reason == "namespace_quota":
			queued++
		default:
			s.Failf("unexpected start outcome", "job %d: %+v", i, result)
		}
	}
	s.Equal(1, created, "exactly marketing's quota of one is admitted: %+v", results)
	s.Equal(3, queued)

	var mu sync.Mutex
	maxRunning := 0
	s.Require().Eventually(func() bool {
		running, done := 0, 0
		for _, job := range jobs {
			for _, run := range s.fetchRuns(job.ID) {
				switch run.Status {
				case "running":
					running++
				case "succeeded":
					done++
				}
			}
		}
		mu.Lock()
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		return done == len(jobs)
	}, 3*runTimeout, 500*time.Millisecond, "every parked run must eventually run")
	mu.Lock()
	defer mu.Unlock()
	s.LessOrEqual(maxRunning, 1, "marketing must never run two at once")
}

// TestNamespaceQuotaRefusesRetryAtCap: at cap, a whole-run retry (REST and
// `caesium run retry`) and a partition retry that would reopen a finished run
// are refused with 409 namespace_quota and Retry-After; once the slot frees
// the retry is accepted.
func (s *IntegrationTestSuite) TestNamespaceQuotaRefusesRetryAtCap() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	failing := s.applyQuotaJob(fmt.Sprintf("quota-retry-%d", suffix), "marketing", "exit 1")
	failedRun := s.startAndAwait(failing.ID)
	s.Require().Equal("failed", failedRun.Status)

	fanJob, fanRun, processID, failIndex := s.applyFailedPartitionRun(fmt.Sprintf("quota-partition-%d", suffix), "marketing")

	holder := s.applyQuotaJob(fmt.Sprintf("quota-retry-holder-%d", suffix), "marketing", "sleep 15")
	holderRun := s.startHolder(holder.ID)

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, failing.ID, failedRun.ID), nil)
	s.Require().NoError(err)
	s.requireNamespaceQuota409(resp, "retry at cap")

	stdout, stderr, cliErr := s.runCLISeparate("run", "retry", "--job-id", failing.ID, "--run-id", failedRun.ID, "--server", s.caesiumURL)
	s.Require().Error(cliErr, "caesium run retry must fail at cap: %s", stdout)
	s.Contains(stderr, "(409)", "the CLI must report the refusal")
	s.Contains(stderr, "quotas.maxConcurrentRuns", "and say it is the namespace quota: %s", stderr)

	resp, err = s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry",
		s.caesiumURL, fanJob.ID, fanRun.ID, processID, failIndex), nil)
	s.Require().NoError(err)
	s.requireNamespaceQuota409(resp, "partition retry at cap")
	s.Equal("failed", s.fetchRun(failing.ID, failedRun.ID).Status, "a refused retry leaves the run terminal")

	s.awaitRun(holder.ID, holderRun, runTimeout)
	resp, err = s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, failing.ID, failedRun.ID), nil)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "a retry once the slot frees is accepted: %s", body)
	s.awaitRun(failing.ID, failedRun.ID, runTimeout)
}

// applyFailedPartitionRun applies a fanned-out job in namespace whose "fail"
// partition fails, runs it to completion, and returns the job, the finished
// run, the fanned task's id and the failed instance's index.
func (s *IntegrationTestSuite) applyFailedPartitionRun(alias, namespace string) (*jobSummary, *runResponse, string, int) {
	s.T().Helper()
	dir := s.writeJobManifest(fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"keep\",\"fail\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "if [ \"$CAESIUM_PARTITION\" = fail ]; then exit 1; fi; echo ok"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      failurePolicy: continue
`, alias, namespace))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	finished := s.startAndAwait(job.ID)
	processID := s.jobTaskIDByName(job.ID, "process")
	partitions := partitionsByValue(s.expandedPartitions(s.listPartitions(job.ID, finished.ID, processID)))
	failRow, ok := partitions["fail"]
	s.Require().True(ok, "the fixture must produce a failed instance: %v", partitions)
	s.Require().Equal("failed", failRow.Status)
	return job, finished, processID, failRow.Index
}

// quotaBackfillJob is a policy-free hourly marketing job whose runs take a
// few seconds, for two-date backfills.
func (s *IntegrationTestSuite) quotaBackfillJob(alias string) *jobSummary {
	s.T().Helper()
	def := s.quotaDefinition(alias, "marketing", "sleep 3")
	def.Trigger.Configuration = map[string]any{"cron": "0 * * * *"}
	s.applyDefinition(def)
	return s.requireJobByAlias(alias)
}

var quotaBackfillStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// TestNamespaceQuotaBackfillWaitsAndCompletes: a two-date backfill whose own
// maxConcurrent is two waits on marketing's quota of one — its dates are never
// queued and never counted failed — and completes with exact accounting.
func (s *IntegrationTestSuite) TestNamespaceQuotaBackfillWaitsAndCompletes() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	holder := s.applyQuotaJob(fmt.Sprintf("quota-bf-holder-%d", suffix), "marketing", "sleep 8")
	job := s.quotaBackfillJob(fmt.Sprintf("quota-bf-%d", suffix))
	holderRun := s.startHolder(holder.ID)

	backfill := s.createBackfill(job.ID, quotaBackfillStart, quotaBackfillStart.Add(2*time.Hour), 2, "all")
	time.Sleep(2 * time.Second)
	s.Empty(s.listJobRunSummaries(job.ID), "no backfill date may run while the holder fills marketing")
	s.Empty(s.jobQueueREST(job.ID), "a backfill child is never queued")

	s.awaitRun(holder.ID, holderRun, runTimeout)
	done := s.awaitBackfill(job.ID, backfill.ID, 3*runTimeout)
	s.Equal("succeeded", done.Status)
	s.Equal(2, done.TotalRuns)
	s.Equal(2, done.CompletedRuns)
	s.Zero(done.FailedRuns)
	s.Len(s.listJobRunSummaries(job.ID), 2)
}

// TestNamespaceQuotaCancelHeldBackfill: a backfill whose dates wait on the
// quota stays cancellable; it ends cancelled with nothing run or counted.
func (s *IntegrationTestSuite) TestNamespaceQuotaCancelHeldBackfill() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	holder := s.applyQuotaJob(fmt.Sprintf("quota-bfc-holder-%d", suffix), "marketing", "sleep 15")
	job := s.quotaBackfillJob(fmt.Sprintf("quota-bfc-%d", suffix))
	holderRun := s.startHolder(holder.ID)

	backfill := s.createBackfill(job.ID, quotaBackfillStart, quotaBackfillStart.Add(2*time.Hour), 1, "all")
	time.Sleep(2 * time.Second)
	resp := s.cancelBackfillRequest(job.ID, backfill.ID)
	_ = resp.Body.Close()
	s.Require().Less(resp.StatusCode, 300, "cancel a held backfill")
	done := s.awaitBackfill(job.ID, backfill.ID, runTimeout)
	s.Equal("cancelled", done.Status)
	s.Zero(done.CompletedRuns)
	s.Zero(done.FailedRuns)
	s.Empty(s.listJobRunSummaries(job.ID), "nothing ran for a backfill cancelled while held")
	s.awaitRun(holder.ID, holderRun, runTimeout)
}

// TestNamespaceQuotaRoundRobinAcrossJobs: two jobs with two parked runs each
// share marketing's slot in turns — A, B, A, B — instead of one job draining
// its queue first.
func (s *IntegrationTestSuite) TestNamespaceQuotaRoundRobinAcrossJobs() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	holder := s.applyQuotaJob(fmt.Sprintf("quota-rr-holder-%d", suffix), "marketing", "sleep 10")
	jobA := s.applyQuotaJob(fmt.Sprintf("quota-rr-a-%d", suffix), "marketing", "sleep 1")
	jobB := s.applyQuotaJob(fmt.Sprintf("quota-rr-b-%d", suffix), "marketing", "sleep 1")
	holderRun := s.startHolder(holder.ID)
	for _, id := range []string{jobA.ID, jobA.ID, jobB.ID, jobB.ID} {
		parked := s.postStart(id)
		s.Require().Equal("namespace_quota", parked.Reason, "%+v", parked)
	}
	s.awaitRun(holder.ID, holderRun, runTimeout)

	type started struct {
		job string
		at  time.Time
	}
	var order []started
	s.Require().Eventually(func() bool {
		order = order[:0]
		for _, job := range []*jobSummary{jobA, jobB} {
			for _, run := range s.fetchRuns(job.ID) {
				at, err := time.Parse(time.RFC3339Nano, run.StartedAt)
				if err != nil {
					return false
				}
				order = append(order, started{job: job.ID, at: at})
			}
		}
		return len(order) == 4 && len(s.jobQueueREST(jobA.ID)) == 0 && len(s.jobQueueREST(jobB.ID)) == 0
	}, 3*runTimeout, time.Second, "every parked run must start")
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			if order[j].at.Before(order[i].at) {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	for i := 1; i < len(order); i++ {
		s.NotEqual(order[i-1].job, order[i].job, "jobs must alternate in marketing's slot: %+v", order)
	}
	s.awaitJobIdle(jobA.ID)
	s.awaitJobIdle(jobB.ID)
}

// TestNamespaceQuotaQueuedRunDroppedAfterMove: a parked request belongs to
// marketing; when its job moves to finance, the request is dropped — never
// started under finance — and a retry of its idempotent start says so.
func (s *IntegrationTestSuite) TestNamespaceQuotaQueuedRunDroppedAfterMove() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	holder := s.applyQuotaJob(fmt.Sprintf("quota-move-holder-%d", suffix), "marketing", "sleep 15")
	moverAlias := fmt.Sprintf("quota-move-%d", suffix)
	mover := s.applyQuotaJob(moverAlias, "marketing", "echo moved")
	holderRun := s.startHolder(holder.ID)

	key := fmt.Sprintf("quota-move-key-%d", suffix)
	keyedStart := func() startResponse {
		req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, mover.ID), bytes.NewReader([]byte(`{}`)))
		s.Require().NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		s.authorize(req)
		resp, err := http.DefaultClient.Do(req)
		s.Require().NoError(err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		s.Require().NoError(err)
		s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))
		var out startResponse
		s.Require().NoError(json.Unmarshal(body, &out), string(body))
		return out
	}
	parked := keyedStart()
	s.Require().Equal("queued", parked.Outcome)
	s.Require().Equal("namespace_quota", parked.Reason)

	s.applyDefinition(s.quotaDefinition(moverAlias, "finance", "echo moved"))
	s.awaitQueueEmpty(mover.ID)
	s.Empty(s.fetchRuns(mover.ID), "a request parked in marketing must never start in finance")
	replayed := keyedStart()
	s.Equal("dropped", replayed.Outcome, "%+v", replayed)
	s.Equal("namespace_moved", replayed.Reason)
	s.awaitRun(holder.ID, holderRun, runTimeout)
}

// TestNamespaceQuotaRefusesReplayAtCap runs on the distributed lane, the one
// where a quarantined replay that re-executes reaches materialization: at cap
// it is refused with 409 namespace_quota and Retry-After, and accepted once
// the slot frees.
func (s *IntegrationTestSuite) TestNamespaceQuotaRefusesReplayAtCap() {
	s.requireMarketingQuota()
	if strings.TrimSpace(os.Getenv("CAESIUM_EXECUTION_MODE")) != "distributed" {
		s.T().Skip("a re-executing replay needs the distributed lane (just integration-test-distributed)")
	}
	suffix := time.Now().UnixNano()
	def := s.quotaDefinition(fmt.Sprintf("quota-replay-%d", suffix), "marketing", "echo replay")
	def.Metadata.ReplaySafe = true
	s.applyDefinition(def)
	job := s.requireJobByAlias(def.Metadata.Alias)
	baseline := s.startAndAwait(job.ID)
	s.Require().Equal("succeeded", baseline.Status)

	holder := s.applyQuotaJob(fmt.Sprintf("quota-replay-holder-%d", suffix), "marketing", "sleep 15")
	holderRun := s.startHolder(holder.ID)
	key := fmt.Sprintf("quota-replay-key-%d", suffix)
	refused := s.postReplay(job.ID, baseline.ID, &key, `{"set":{"mode":"what-if"}}`)
	s.Require().Equal(http.StatusConflict, refused.status, refused.body)
	s.Contains(refused.body, `"error":"namespace_quota"`)

	s.awaitRun(holder.ID, holderRun, runTimeout)
	accepted := s.postReplay(job.ID, baseline.ID, &key, `{"set":{"mode":"what-if"}}`)
	s.Require().Equal(http.StatusAccepted, accepted.status, accepted.body)
}

// serveQuotaPolicy serves the auth lane's CURRENT policy with the given
// namespaces' maxConcurrentRuns set, reloads it, and returns the revision.
func (s *IntegrationTestSuite) serveQuotaPolicy(path string, base []byte, caps map[string]int) string {
	s.T().Helper()
	return s.serveEditedPolicy(path, base, func(p *policy.AccessPolicy) {
		for namespace, limit := range caps {
			settings := p.Namespaces[namespace]
			settings.Quotas = &policy.Quotas{MaxConcurrentRuns: limit}
			p.Namespaces[namespace] = settings
		}
	})
}

// serveEditedPolicy serves base edited by edit on the auth lane, reloads it
// through `caesium auth policy reload`, and returns the revision in force.
func (s *IntegrationTestSuite) serveEditedPolicy(path string, base []byte, edit func(*policy.AccessPolicy)) string {
	s.T().Helper()
	p, err := policy.Parse(base)
	s.Require().NoError(err)
	edit(p)
	data, err := yaml.Marshal(p)
	s.Require().NoError(err)
	s.writeAuthPolicy(path, data)
	reload := runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Require().Equal(policy.Hash(data), reload.Revision, "the edited policy is in force")
	return reload.Revision
}

// requireNamespaceUndeclared409 asserts a 409 namespace_undeclared refusal
// naming namespace, with no Retry-After (waiting does not help).
func (s *IntegrationTestSuite) requireNamespaceUndeclared409(status int, header http.Header, body, namespace, what string) {
	s.T().Helper()
	s.Require().Equal(http.StatusConflict, status, "%s: %s", what, body)
	s.Empty(header.Get("Retry-After"), what)
	var refusal struct {
		Error     string `json:"error"`
		Namespace string `json:"namespace"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &refusal), "%s: %s", what, body)
	s.Equal("namespace_undeclared", refusal.Error, what)
	s.Equal(namespace, refusal.Namespace, what)
}

// postForStatus POSTs body with the suite's credentials and an optional
// Idempotency-Key, returning the status, headers and body.
func (s *IntegrationTestSuite) postForStatus(path, body, idempotencyKey string) (int, http.Header, string) {
	s.T().Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+path, reader)
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	s.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, resp.Header, string(out)
}

// TestAuthNamespaceUndeclaredRefusesEveryStart drives fold B through live
// HTTP: jobs run in a namespace a reload then removes from the served policy.
// A manual start, a run retry, a partition retry and a replay of them all
// answer 409 namespace_undeclared, and none writes a run or a queued request.
func (s *IntegrationTestSuite) TestAuthNamespaceUndeclaredRefusesEveryStart() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() { s.restoreSecretScopePolicy(path, original) })
	suffix := time.Now().UnixNano()
	retired := fmt.Sprintf("retired-%d", suffix%1_000_000)
	s.serveEditedPolicy(path, original, func(p *policy.AccessPolicy) { p.Namespaces[retired] = policy.Namespace{} })

	replayable := s.quotaDefinition(fmt.Sprintf("undeclared-replay-%d", suffix), retired, "echo ok")
	replayable.Metadata.ReplaySafe = true
	s.applyDefinition(replayable)
	starter := s.requireJobByAlias(replayable.Metadata.Alias)
	baseline := s.startAndAwait(starter.ID)
	s.Require().Equal("succeeded", baseline.Status)
	failing := s.applyQuotaJob(fmt.Sprintf("undeclared-retry-%d", suffix), retired, "exit 1")
	failedRun := s.startAndAwait(failing.ID)
	s.Require().Equal("failed", failedRun.Status)
	fanJob, fanRun, processID, failIndex := s.applyFailedPartitionRun(fmt.Sprintf("undeclared-partition-%d", suffix), retired)
	for _, job := range []*jobSummary{starter, failing, fanJob} {
		s.T().Cleanup(func() {
			if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+job.ID, nil); err == nil {
				_ = resp.Body.Close()
			}
		})
	}

	// The reload removes the namespace while its jobs remain.
	s.serveEditedPolicy(path, original, func(*policy.AccessPolicy) {})

	status, header, body := s.postForStatus("/v1/jobs/"+starter.ID+"/run", "", "")
	s.requireNamespaceUndeclared409(status, header, body, retired, "manual start")
	status, header, body = s.postForStatus(fmt.Sprintf("/v1/jobs/%s/runs/%s/retry", failing.ID, failedRun.ID), "", "")
	s.requireNamespaceUndeclared409(status, header, body, retired, "run retry")
	status, header, body = s.postForStatus(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry", fanJob.ID, fanRun.ID, processID, failIndex), "", "")
	s.requireNamespaceUndeclared409(status, header, body, retired, "partition retry")
	status, header, body = s.postForStatus(fmt.Sprintf("/v1/jobs/%s/runs/%s/replay", starter.ID, baseline.ID), `{"set":{}}`, fmt.Sprintf("undeclared-replay-key-%d", suffix))
	s.requireNamespaceUndeclared409(status, header, body, retired, "replay")

	s.Len(s.fetchRuns(starter.ID), 1, "a refused start or replay writes no run")
	s.Empty(s.jobQueueREST(starter.ID), "a refused start is not queued")
	s.Equal("failed", s.fetchRun(failing.ID, failedRun.ID).Status, "a refused retry leaves the run terminal")
	s.Len(s.fetchRuns(fanJob.ID), 1)
}

// TestAuthNamespaceQuotaRoundRobinTwoNamespaces: with marketing AND finance
// capped at one (finance's cap added by a live policy reload), each namespace
// drains on its own — finance's parked run starts as soon as finance frees,
// while marketing stays full — and neither ever runs two at once.
func (s *IntegrationTestSuite) TestAuthNamespaceQuotaRoundRobinTwoNamespaces() {
	s.requireAuthLane()
	s.requireMarketingQuota()
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() { s.restoreSecretScopePolicy(path, original) })
	s.serveQuotaPolicy(path, original, map[string]int{"finance": 1})

	suffix := time.Now().UnixNano()
	mktHolder := s.applyQuotaJob(fmt.Sprintf("quota-2ns-mkt-holder-%d", suffix), "marketing", "sleep 25")
	finHolder := s.applyQuotaJob(fmt.Sprintf("quota-2ns-fin-holder-%d", suffix), "finance", "sleep 5")
	mktWaiter := s.applyQuotaJob(fmt.Sprintf("quota-2ns-mkt-%d", suffix), "marketing", "echo marketing")
	finWaiter := s.applyQuotaJob(fmt.Sprintf("quota-2ns-fin-%d", suffix), "finance", "echo finance")
	mktRun := s.startHolder(mktHolder.ID)
	finRun := s.startHolder(finHolder.ID)
	s.Equal("namespace_quota", s.postStart(mktWaiter.ID).Reason)
	s.Equal("namespace_quota", s.postStart(finWaiter.ID).Reason, "the reloaded finance quota is enforced")

	s.awaitRun(finHolder.ID, finRun, runTimeout)
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(finWaiter.ID) {
			if run.Status == "succeeded" {
				return true
			}
		}
		return false
	}, runTimeout, 500*time.Millisecond, "finance's parked run must start while marketing is still full")
	s.Equal("running", s.fetchRun(mktHolder.ID, mktRun).Status, "marketing's holder is still running")
	s.Len(s.jobQueueREST(mktWaiter.ID), 1, "marketing's parked run still waits")

	s.awaitRun(mktHolder.ID, mktRun, runTimeout)
	s.awaitQueueEmpty(mktWaiter.ID)
	s.awaitJobIdle(mktWaiter.ID)
}

// TestIncidentAgentRetryDeferredByNamespaceQuota: an agent's retry_from_failure
// proposed while its namespace is full is not executed; the action records the
// deferral (the namespace quota) instead of a silent failure.
func (s *IntegrationTestSuite) TestIncidentAgentRetryDeferredByNamespaceQuota() {
	s.requireAuthLane()
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("quota-agent-%d", suffix)
	def := s.quotaDefinition(alias, "marketing", "exit 1")
	def.Metadata.Remediation = &schema.MetadataRemediation{
		Profile:  "triage-only",
		Classes:  []string{"unknown"},
		Autonomy: &schema.RemediationAutonomy{Allow: []string{"retry_from_failure"}},
	}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)

	// Preconditions produced through the catalog: a failed marketing run and
	// an open incident on it, so no live agent session competes for the retry.
	conn := s.openIntegrationCatalogGorm()
	jobID := uuid.MustParse(job.ID)
	runID := uuid.New()
	now := time.Now().UTC()
	s.Require().NoError(conn.Create(&models.JobRun{
		ID: runID, JobID: jobID, Namespace: "marketing", Status: "failed",
		StartedAt: now.Add(-time.Minute), CompletedAt: &now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}).Error)
	inc, _, err := iincident.NewStore(conn).OpenOrAppend(s.T().Context(), iincident.OpenParams{
		Namespace: "marketing",
		JobID:     jobID,
		RunID:     &runID,
		TaskName:  "work",
		Class:     iincident.ClassUnknown,
		LastError: "namespace quota deferral seed",
	})
	s.Require().NoError(err)
	key, err := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret)).
		MintAgentSessionKey(inc.ID, []string{alias}, time.Hour)
	s.Require().NoError(err)

	holder := s.applyQuotaJob(fmt.Sprintf("quota-agent-holder-%d", suffix), "marketing", "sleep 15")
	holderRun := s.startHolder(holder.ID)
	status, body := s.postAgentMCP(key.Plaintext, inc.ID, map[string]any{
		"jsonrpc": "2.0",
		"id":      "retry",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "propose_action",
			"arguments": map[string]any{"type": "retry_from_failure", "params": map[string]any{"run_id": runID.String()}},
		},
	})
	s.Require().Equal(http.StatusOK, status, string(body))
	s.Contains(string(body), "retry deferred", "the action must record the deferral: %s", body)
	s.Contains(string(body), "concurrent-run quota", "and name the namespace quota as its cause: %s", body)
	s.Equal("failed", s.fetchRun(job.ID, runID.String()).Status, "a deferred retry does not reopen the run")
	s.awaitRun(holder.ID, holderRun, runTimeout)
}

// TestAuthNamespaceQueuedRequestKeepsItsOwner drives fold A through
// authenticated HTTP: a request parked in marketing stays marketing's after
// its job moves to finance. While finance is full (so the dequeuer cannot
// reach the row) a finance-confined viewer's GET /v1/jobs/:id/queue omits it,
// a finance-confined operator's DELETE answers 404, a caller that may read
// marketing still sees it, and only an operator in marketing may cancel it.
// It never starts in finance.
func (s *IntegrationTestSuite) TestAuthNamespaceQueuedRequestKeepsItsOwner() {
	s.requireAuthLane()
	s.requireMarketingQuota()
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() { s.restoreSecretScopePolicy(path, original) })
	s.serveQuotaPolicy(path, original, map[string]int{"finance": 1})

	suffix := time.Now().UnixNano()
	mktHolder := s.applyQuotaJob(fmt.Sprintf("quota-own-mkt-holder-%d", suffix), "marketing", "sleep 25")
	finHolder := s.applyQuotaJob(fmt.Sprintf("quota-own-fin-holder-%d", suffix), "finance", "sleep 25")
	moverAlias := fmt.Sprintf("quota-own-mover-%d", suffix)
	mover := s.applyQuotaJob(moverAlias, "marketing", "echo moved")
	mktRun := s.startHolder(mktHolder.ID)
	parked := s.postStart(mover.ID)
	s.Require().Equal("queued", parked.Outcome, "%+v", parked)
	s.Require().Equal("namespace_quota", parked.Reason)
	finRun := s.startHolder(finHolder.ID)
	s.applyDefinition(s.quotaDefinition(moverAlias, "finance", "echo moved"))

	financeViewer := s.mintNamespacedKey("viewer", "finance")
	financeOperator := s.mintNamespacedKey("operator", "finance")
	bothViewer := s.mintNamespacedKey("viewer", "marketing", "finance")
	queuePath := "/v1/jobs/" + mover.ID + "/queue"

	var hidden []queueCLIItem
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, queuePath, financeViewer.Plaintext, "")), &hidden))
	s.Empty(hidden, "a finance-confined caller must not see a request marketing parked")
	s.requireNSStatus(http.StatusNotFound, http.MethodDelete, queuePath+"/"+parked.QueueID, financeOperator.Plaintext, "")

	var visible []queueCLIItem
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, queuePath, bothViewer.Plaintext, "")), &visible))
	s.Require().Len(visible, 1, "a caller that may read marketing still sees marketing's request")
	s.Equal(parked.QueueID, visible[0].ID)
	s.Equal("namespace_quota", visible[0].HeldReason)
	unrestricted := s.jobQueueREST(mover.ID)
	s.Require().Len(unrestricted, 1, "the refused DELETE removed nothing")
	s.Equal(parked.QueueID, unrestricted[0].ID)

	// Operator in the request's own namespace (marketing) as well as the
	// job's (finance) may cancel it. Read-but-not-operate (viewer@marketing +
	// operator@finance) is a 403 pinned by TestAuthorizeQueuedRunCancel: an
	// API key carries one role, so the lane cannot mint mixed grants.
	bothOperator := s.mintNamespacedKey("operator", "marketing", "finance")
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, queuePath+"/"+parked.QueueID, bothOperator.Plaintext, "")
	s.Empty(s.jobQueueREST(mover.ID), "the owner's operator cancelled the request")

	s.awaitRun(finHolder.ID, finRun, runTimeout)
	s.Empty(s.fetchRuns(mover.ID), "marketing's request must never start in finance")
	s.awaitRun(mktHolder.ID, mktRun, runTimeout)
}

// TestNamespaceQuotaParksSecondHTTPTriggerOfARunningJob: native triggers admit
// through AdmitRun. With marketing's one slot held by a policy-free job's own
// HTTP-triggered run, a second fire of the hook used to resume that live run
// (resetting its in-flight task, which then ran again) instead of reaching
// Start. It is now parked with held_reason namespace_quota whatever the job's
// policy (design §10.3): the live task keeps its container, no second run
// exists while marketing is full, and the parked fire runs once it frees.
func (s *IntegrationTestSuite) TestNamespaceQuotaParksSecondHTTPTriggerOfARunningJob() {
	s.requireMarketingQuota()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("quota-http-%d", suffix)
	hook := fmt.Sprintf("quota-http-%d", suffix)
	def := s.quotaDefinition(alias, "marketing", "sleep 15")
	def.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{"path": "/hooks/" + hook}}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)
	fire := func() {
		resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/hooks/%s", s.caesiumURL, hook), strings.NewReader(`{}`))
		s.Require().NoError(err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.Require().NoError(err)
		s.Require().Equal(http.StatusAccepted, resp.StatusCode, "fire %s: %s", hook, body)
	}

	// A run another scenario left behind may park this first fire too; it
	// starts when that run ends.
	fire()
	var live runResponse
	s.Require().Eventually(func() bool {
		for _, run := range s.fetchRuns(job.ID) {
			if run.Status != "running" {
				continue
			}
			detail := s.fetchRun(job.ID, run.ID)
			if len(detail.Tasks) == 1 && detail.Tasks[0].Status == "running" && detail.Tasks[0].RuntimeID != "" {
				live = detail
				return true
			}
		}
		return false
	}, runTimeout, 250*time.Millisecond, "the first fire never reached a running container")
	liveTask := live.Tasks[0]

	fire()
	s.Require().Eventually(func() bool {
		for _, row := range s.jobQueueREST(job.ID) {
			if row.HeldReason == "namespace_quota" {
				return true
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "the second fire must be parked by marketing's quota")
	s.Require().Len(s.fetchRuns(job.ID), 1, "the second fire must not create a run while marketing is full")
	after := s.fetchRun(job.ID, live.ID)
	s.Require().Len(after.Tasks, 1)
	s.Equal(liveTask.RuntimeID, after.Tasks[0].RuntimeID, "the live run's task must not be reset and run again")
	s.Equal(liveTask.StartedAt, after.Tasks[0].StartedAt)

	s.Equal("succeeded", s.awaitRun(job.ID, live.ID, runTimeout).Status)
	s.awaitQueueEmpty(job.ID)
	s.Require().Eventually(func() bool {
		succeeded := 0
		for _, run := range s.fetchRuns(job.ID) {
			if run.Status == "succeeded" {
				succeeded++
			}
		}
		return succeeded == 2
	}, runTimeout, 500*time.Millisecond, "the parked fire must run once marketing frees")
}
