//go:build integration

package test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// Live scenarios for identity-and-access B4: collections filtered to a
// confined principal's namespaces, and history under a job filtered by each
// row's persisted namespace. Confined keys are minted by namespacedKey.

// nsGet sends an authenticated GET and returns the status, headers and body.
func (s *IntegrationTestSuite) nsGet(path, token string) (int, http.Header, string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, resp.Header, string(body)
}

// streamEventsWithKey reads GET /v1/events?query as token until done reports
// true for the events read so far or the timeout passes, and returns them.
func (s *IntegrationTestSuite) streamEventsWithKey(query, token string, timeout time.Duration, done func([]event.Event) bool) []event.Event {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(s.T().Context(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+"/v1/events"+query, nil)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var events []event.Event
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var evt event.Event
		s.Require().NoError(json.Unmarshal([]byte(data), &evt))
		events = append(events, evt)
		if done != nil && done(events) {
			break
		}
	}
	return events
}

// readEventStream is streamEventsWithKey without assertions, for a reader on
// another goroutine: it returns the events read until done or ctx ends, the
// response status, and any error other than the context ending.
func (s *IntegrationTestSuite) readEventStream(ctx context.Context, query, token string, done func([]event.Event) bool) ([]event.Event, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+"/v1/events"+query, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var events []event.Event
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var evt event.Event
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			return events, resp.StatusCode, err
		}
		events = append(events, evt)
		if done(events) {
			break
		}
	}
	return events, resp.StatusCode, nil
}

func streamHasRun(events []event.Event, runID string, typ event.Type) bool {
	for _, evt := range events {
		if evt.RunID.String() == runID && (typ == "" || evt.Type == typ) {
			return true
		}
	}
	return false
}

// requireBodyFree asserts a serialized response names none of the foreign
// identifiers (aliases, ids, payload markers).
func (s *IntegrationTestSuite) requireBodyFree(body, what string, foreign ...string) {
	s.T().Helper()
	for _, value := range foreign {
		s.Require().NotContains(body, value, "%s leaked a foreign value", what)
	}
}

// TestAuthNamespaceCollectionsFilter drives every B4 collection with a
// marketing-confined viewer against a fixture with a marketing and a finance
// job: the job list and a job read, the trigger list (including a trigger
// shared with the finance job, which is also omitted from the job read), the
// incident list (no frozen allowlist), stats, and the event stream (catch-up
// and live fan-out). The serialized bodies never name the finance job, its
// alias or its runs; a cluster viewer is unchanged.
func (s *IntegrationTestSuite) TestAuthNamespaceCollectionsFilter() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("b4coll")
	defer s.closeNamespacedRoutesFixture(f)
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	bothViewer := s.namespacedKey(f, "viewer", `["marketing","finance"]`).Plaintext
	clusterViewer := s.namespacedKey(f, "viewer", "").Plaintext
	mkt, fin := f.mktJob.ID, f.finJob.ID
	mktRun, finRun := s.triggerRun(mkt), s.triggerRun(fin)
	s.awaitRun(mkt, mktRun, runTimeout)
	s.awaitRun(fin, finRun, runTimeout)
	foreign := []string{fin, f.finAlias, finRun}

	// Jobs: the list and its recent runs, and a job read with its latest run.
	body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs", mktViewer, "")
	s.Contains(body, f.mktAlias)
	s.requireBodyFree(body, "job list", foreign...)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs", clusterViewer, ""), f.finAlias)
	// ?order_by= takes only columns: an expression over rows the caller may
	// not read is a 400, never SQL.
	probe := url.QueryEscape("CASE WHEN EXISTS(SELECT 1 FROM jobs WHERE namespace='finance') THEN 1 ELSE 2 END")
	s.requireNSStatus(http.StatusBadRequest, http.MethodGet, "/v1/jobs?order_by="+probe, mktViewer, "")
	s.requireNSStatus(http.StatusBadRequest, http.MethodGet, "/v1/triggers?order_by="+probe, mktViewer, "")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs?order_by=alias%20desc", mktViewer, ""), f.mktAlias)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt, mktViewer, "")
	s.Contains(body, mktRun, "the latest run is the caller's own")
	s.Contains(body, `"trigger":{`, "the job's own trigger is embedded")

	var mktTrigger, finTrigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, mkt).Scan(&mktTrigger))
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, fin).Scan(&finTrigger))
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers", mktViewer, "")
	s.Contains(body, mktTrigger)
	s.requireBodyFree(body, "trigger list", finTrigger)

	// The finance job now shares the marketing job's trigger (precondition
	// seeded in the catalog): its configuration is readable only with viewer
	// in both namespaces, so it disappears from the marketing viewer's job
	// read and trigger list.
	s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, mktTrigger, fin)
	f.undo = append(f.undo, func() { s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, finTrigger, fin) })
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt, mktViewer, "")
	s.NotContains(body, `"trigger":{`, "a shared trigger is omitted for a caller confined to one of its namespaces")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt, bothViewer, ""), `"trigger":{`)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt, clusterViewer, ""), `"trigger":{`)
	s.NotContains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers", mktViewer, ""), mktTrigger)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers", bothViewer, ""), mktTrigger)

	// Incidents (seeded closed, so remediation never picks them up), each
	// with a frozen allowlist naming the finance job.
	now := time.Now().UTC()
	incident := func(job, namespace string) string {
		id := uuid.NewString()
		s.catalogExec(f, 1, `INSERT INTO incidents (id, namespace, job_id, class, status, dedupe_key, allowed_jobs, opened_at, closed_at, created_at, updated_at) VALUES (?, ?, ?, 'b4_namespace_check', 'closed', ?, ?, ?, ?, ?, ?)`,
			id, namespace, job, "b4/"+id, `["`+f.finAlias+`"]`, now, now, now, now)
		return id
	}
	mktIncident, finIncident := incident(mkt, "marketing"), incident(fin, "finance")
	f.undo = append(f.undo, func() { s.catalogExec(f, -1, `DELETE FROM incidents WHERE id IN (?, ?)`, mktIncident, finIncident) })
	var incidents struct {
		Incidents []struct {
			ID string `json:"id"`
		} `json:"incidents"`
		Total int `json:"total"`
	}
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents?job_id="+mkt, mktViewer, "")
	s.Require().NoError(json.Unmarshal([]byte(body), &incidents))
	s.Equal(1, incidents.Total)
	s.Require().Len(incidents.Incidents, 1)
	s.Equal(mktIncident, incidents.Incidents[0].ID)
	s.requireBodyFree(body, "incident list", f.finAlias, finIncident)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents?job_id="+fin, mktViewer, "")
	s.Require().NoError(json.Unmarshal([]byte(body), &incidents))
	s.Zero(incidents.Total, "another namespace's incidents are not counted")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents?job_id="+mkt, clusterViewer, ""), f.finAlias,
		"an unrestricted principal still gets the allowlist")

	// Stats: every job a confined caller's aggregates name is in marketing.
	for _, path := range []string{"/v1/stats", "/v1/stats/summary?window=24h"} {
		body = s.requireNSStatus(http.StatusOK, http.MethodGet, path, mktViewer, "")
		s.requireBodyFree(body, path, foreign...)
		var stats struct {
			Jobs struct {
				Total int `json:"total"`
			} `json:"jobs"`
			SlowestJobs []struct {
				JobID string `json:"job_id"`
			} `json:"slowest_jobs"`
			TopFailing []struct {
				JobID string `json:"job_id"`
			} `json:"top_failing"`
		}
		s.Require().NoError(json.Unmarshal([]byte(body), &stats))
		var cluster struct {
			Jobs struct {
				Total int `json:"total"`
			} `json:"jobs"`
		}
		s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, path, clusterViewer, "")), &cluster))
		s.Less(stats.Jobs.Total, cluster.Jobs.Total, path)
		for _, entry := range append(stats.SlowestJobs, stats.TopFailing...) {
			var job struct {
				Namespace string `json:"namespace"`
			}
			status, _, jobBody := s.nsGet("/v1/jobs/"+entry.JobID, s.authAPIKey)
			if status == http.StatusNotFound {
				continue // deleted by a concurrent scenario
			}
			s.Require().NoError(json.Unmarshal([]byte(jobBody), &job))
			s.Equal("marketing", job.Namespace, "%s names job %s", path, entry.JobID)
		}
	}

	// Event stream: the catch-up carries the marketing run's events and never
	// the finance run's; a finance run_id is refused; a run started while the
	// subscriber is connected reaches it only from marketing.
	events := s.streamEventsWithKey("?job_id="+mkt, mktViewer, 20*time.Second, func(events []event.Event) bool {
		return streamHasRun(events, mktRun, event.TypeRunStarted)
	})
	s.True(streamHasRun(events, mktRun, event.TypeRunStarted), "the catch-up delivers the caller's run")
	events = s.streamEventsWithKey("?job_id="+fin, mktViewer, 3*time.Second, nil)
	s.Empty(events, "another namespace's job yields nothing")
	status, _, _ := s.nsGet("/v1/events?run_id="+finRun, mktViewer)
	s.Equal(http.StatusForbidden, status)

	// The subscriber reads in the background (no assertions off the test
	// goroutine) while the test starts a finance and a marketing run.
	var liveMkt atomic.Value
	type streamRead struct {
		events []event.Event
		status int
		err    error
	}
	reads := make(chan streamRead, 1)
	streamCtx, cancelStream := context.WithTimeout(s.T().Context(), 90*time.Second)
	defer cancelStream()
	go func() {
		events, status, err := s.readEventStream(streamCtx, "?types=run_started,run_completed", mktViewer, func(events []event.Event) bool {
			id, _ := liveMkt.Load().(string)
			return id != "" && streamHasRun(events, id, event.TypeRunStarted)
		})
		reads <- streamRead{events, status, err}
	}()
	time.Sleep(2 * time.Second)
	newFin := s.triggerRun(fin)
	newMkt := s.triggerRun(mkt)
	liveMkt.Store(newMkt)
	read := <-reads
	s.Require().Equal(http.StatusOK, read.status, read.err)
	events = read.events
	s.True(streamHasRun(events, newMkt, event.TypeRunStarted), "a live marketing run reaches the marketing subscriber")
	s.False(streamHasRun(events, newFin, ""), "a live finance run never reaches it")
	s.False(streamHasRun(events, finRun, ""))
	s.awaitRun(mkt, newMkt, runTimeout)
	s.awaitRun(fin, newFin, runTimeout)
}

// movedJobDefinition is a cache-enabled job whose single step is
// deterministic, so a run after a move is a cache miss in the new namespace.
func (s *IntegrationTestSuite) movedJobDefinition(alias, namespace string) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Metadata.Cache = true
	return def
}

// TestAuthNamespaceMovedJobHistory applies a job in marketing, runs it, moves
// it to finance and runs it again. A finance viewer reads only the finance
// run (list, total, latest run, cache entries, why baseline) and cannot diff
// against the marketing run; a marketing viewer cannot read the moved job at
// all; a cluster viewer sees both. Retrying the marketing run is refused 409
// namespace_moved for every caller that may reach it, whole-run and partition
// alike.
func (s *IntegrationTestSuite) TestAuthNamespaceMovedJobHistory() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("b4move")
	defer s.closeNamespacedRoutesFixture(f)
	alias := fmt.Sprintf("b4-moved-%d", f.stamp)
	s.applyDefinition(s.movedJobDefinition(alias, "marketing"))
	job := s.requireJobByAlias(alias)
	f.jobs = append(f.jobs, job.ID)
	oldRun := s.triggerRun(job.ID)
	s.Equal("marketing", s.awaitRun(job.ID, oldRun, runTimeout).Namespace)
	s.applyDefinition(s.movedJobDefinition(alias, "finance"))
	newRun := s.triggerRun(job.ID)
	s.Equal("finance", s.awaitRun(job.ID, newRun, runTimeout).Namespace)

	finViewer := s.namespacedKey(f, "viewer", `["finance"]`).Plaintext
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	bothViewer := s.namespacedKey(f, "viewer", `["marketing","finance"]`).Plaintext
	clusterViewer := s.namespacedKey(f, "viewer", "").Plaintext
	finRunner := s.namespacedKey(f, "runner", `["finance"]`).Plaintext
	bothRunner := s.namespacedKey(f, "runner", `["marketing","finance"]`).Plaintext

	status, header, body := s.nsGet("/v1/jobs/"+job.ID+"/runs", finViewer)
	s.Require().Equal(http.StatusOK, status, body)
	var runs []runResponse
	s.Require().NoError(json.Unmarshal([]byte(body), &runs))
	s.Require().Len(runs, 1)
	s.Equal(newRun, runs[0].ID)
	s.Equal("1", header.Get("X-Caesium-Total-Count"), "the total counts only readable runs")
	s.requireBodyFree(body, "runs list", oldRun)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID, finViewer, "")
	s.Contains(body, newRun)
	s.requireBodyFree(body, "job read", oldRun)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/cache", finViewer, "")
	s.Contains(body, newRun, "the finance run filled its own cache entry")
	s.requireBodyFree(body, "cache entries", oldRun)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/cache", bothViewer, ""), oldRun)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/runs/"+newRun+"/why?task=ok", finViewer, "")
	s.requireBodyFree(body, "why", oldRun)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/runs/"+newRun+"/why?task=ok", clusterViewer, ""), oldRun,
		"an unrestricted caller still diffs against the prior owner's run")
	diff := "/v1/jobs/" + job.ID + "/runs/diff?left=" + oldRun + "&right=" + newRun
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, diff, finViewer, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, diff, bothViewer, "")

	// The old owner cannot reach the moved job at all (its current namespace
	// is finance); the cluster viewer reads both runs.
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+job.ID+"/runs", mktViewer, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+job.ID, mktViewer, "")
	status, header, _ = s.nsGet("/v1/jobs/"+job.ID+"/runs", clusterViewer)
	s.Require().Equal(http.StatusOK, status)
	s.Equal("2", header.Get("X-Caesium-Total-Count"))

	// Retrying the marketing run would execute the finance job model inside
	// the old owner's run.
	retry := "/v1/jobs/" + job.ID + "/runs/" + oldRun + "/retry"
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, retry, finRunner, "")
	for _, key := range []string{bothRunner, s.authAPIKey} {
		status, body := s.nsRequest(http.MethodPost, retry, key, "", nil)
		s.Require().Equal(http.StatusConflict, status, body)
		var moved struct {
			Error        string `json:"error"`
			Message      string `json:"message"`
			RunNamespace string `json:"run_namespace"`
			JobNamespace string `json:"job_namespace"`
		}
		s.Require().NoError(json.Unmarshal([]byte(body), &moved))
		s.Equal("namespace_moved", moved.Error)
		s.Equal("marketing", moved.RunNamespace)
		s.Equal("finance", moved.JobNamespace)
		s.Contains(moved.Message, "/replay")
		partition := "/v1/jobs/" + job.ID + "/runs/" + oldRun + "/tasks/ok/partitions/0/retry"
		status, body = s.nsRequest(http.MethodPost, partition, key, "", nil)
		s.Require().Equal(http.StatusConflict, status, body)
		s.Contains(body, "namespace_moved")
	}
	s.Equal("succeeded", s.fetchRun(job.ID, oldRun).Status, "a refused retry changes nothing")
}

// eventDeliveryDefinition is a job triggered by events of eventType.
func (s *IntegrationTestSuite) eventDeliveryDefinition(alias, namespace, eventType string) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Trigger = schema.Trigger{Type: "event", Configuration: map[string]any{
		"events": []any{map[string]any{"type": eventType, "source": "b4"}},
	}}
	return def
}

// backfillDefinition is a job whose cron fires only on 29 February, so a
// backfill over past leap days has exactly the dates the window holds and the
// schedule itself never fires during the test. sleep keeps each child running.
func (s *IntegrationTestSuite) backfillDefinition(alias, namespace string, sleep int) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Trigger = schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "17 3 29 2 *"}}
	if sleep > 0 {
		def.Steps[0].Command = []string{"sh", "-c", fmt.Sprintf("sleep %d", sleep)}
	}
	return def
}

// awaitJobIdle waits until the job has a run and none of its runs is still in
// flight (a running run blocks a re-apply).
func (s *IntegrationTestSuite) awaitJobIdle(jobID string) {
	s.T().Helper()
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(jobID)
		if len(runs) == 0 {
			return false
		}
		for _, run := range runs {
			switch run.Status {
			case "pending", "queued", "running":
				return false
			}
		}
		return true
	}, runTimeout, time.Second, "job %s still has a run in flight", jobID)
}

func (s *IntegrationTestSuite) awaitNamespacedBackfill(jobID, backfillID string) map[string]any {
	s.T().Helper()
	var backfill map[string]any
	s.Require().Eventually(func() bool {
		status, _, body := s.nsGet("/v1/jobs/"+jobID+"/backfills/"+backfillID, s.authAPIKey)
		if status != http.StatusOK || json.Unmarshal([]byte(body), &backfill) != nil {
			return false
		}
		return backfill["status"] != "running"
	}, runTimeout, time.Second)
	return backfill
}

// TestAuthNamespaceDeliveriesAndBackfills: event deliveries are shown to a
// confined caller only when every run they started is in its namespaces, so a
// delivery made before a job moved (with its private payload) never reaches
// the new owner; a moved job's backfill deduplicates only against its own
// namespace's runs; and confined runner and operator keys create and cancel a
// running backfill in their namespace and are refused elsewhere.
func (s *IntegrationTestSuite) TestAuthNamespaceDeliveriesAndBackfills() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("b4deliv")
	defer s.closeNamespacedRoutesFixture(f)
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	finViewer := s.namespacedKey(f, "viewer", `["finance"]`).Plaintext
	mktRunner := s.namespacedKey(f, "runner", `["marketing"]`).Plaintext
	finRunner := s.namespacedKey(f, "runner", `["finance"]`).Plaintext
	finOperator := s.namespacedKey(f, "operator", `["finance"]`).Plaintext

	// Deliveries: a marketing-only event, a finance-only event and an event
	// both jobs listen to.
	mktType := fmt.Sprintf("b4.%d.mkt", f.stamp)
	finType := fmt.Sprintf("b4.%d.fin", f.stamp)
	mktAlias, finAlias := fmt.Sprintf("b4-deliv-mkt-%d", f.stamp), fmt.Sprintf("b4-deliv-fin-%d", f.stamp)
	s.applyDefinition(s.eventDeliveryDefinition(mktAlias, "marketing", mktType))
	s.applyDefinition(s.eventDeliveryDefinition(finAlias, "finance", finType))
	mktJob, finJob := s.requireJobByAlias(mktAlias), s.requireJobByAlias(finAlias)
	f.jobs = append(f.jobs, mktJob.ID, finJob.ID)
	mktSecret := "b4-mkt-private-" + uuid.NewString()
	finSecret := "b4-fin-private-" + uuid.NewString()
	mktEvent := s.postEvent(fmt.Sprintf(`{"type":%q,"source":"b4","data":{"marker":%q}}`, mktType, mktSecret))
	finEvent := s.postEvent(fmt.Sprintf(`{"type":%q,"source":"b4","data":{"marker":%q}}`, finType, finSecret))
	s.Equal(1, mktEvent.RunsStarted)
	s.Equal(1, finEvent.RunsStarted)
	// A run in flight blocks the re-apply that moves the job below.
	s.awaitJobIdle(mktJob.ID)
	s.awaitJobIdle(finJob.ID)

	body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/events/ingested?source=b4&limit=500", mktViewer, "")
	s.Contains(body, mktSecret)
	s.requireBodyFree(body, "ingested events", finSecret, finEvent.EventID)
	var mktTrigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, mktJob.ID).Scan(&mktTrigger))
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger+"/events", mktViewer, ""), mktSecret)

	// The marketing event-trigger job moves to finance. Its trigger's
	// history keeps the pre-move delivery, whose run stayed in marketing: the
	// new owner can read the trigger but not that delivery or its payload.
	s.applyDefinition(s.eventDeliveryDefinition(mktAlias, "finance", mktType))
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger+"/events", finViewer, "")
	s.requireBodyFree(body, "moved trigger history", mktSecret, mktEvent.EventID)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger+"/events", s.authAPIKey, ""), mktSecret)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/events/ingested?source=b4&limit=500", finViewer, "")
	s.Contains(body, finSecret)
	s.requireBodyFree(body, "ingested events after the move", mktSecret)

	// Lifecycle provenance: a marketing job listening for a finance job's
	// completions (the lifecycle bridge) starts marketing runs, but each
	// delivery carries the finance run's payload, so marketing never sees it.
	sourceAlias := fmt.Sprintf("b4-lifecycle-src-%d", f.stamp)
	s.applyDefinition(s.namespacedJobDefinition(sourceAlias, "finance"))
	sourceJob := s.requireJobByAlias(sourceAlias)
	f.jobs = append(f.jobs, sourceJob.ID)
	listenerAlias := fmt.Sprintf("b4-lifecycle-dst-%d", f.stamp)
	listener := s.namespacedJobDefinition(listenerAlias, "marketing")
	listener.Trigger = schema.Trigger{Type: "event", Configuration: map[string]any{
		"events": []any{map[string]any{"type": "run_completed", "source": "caesium", "filter": map[string]any{"job_alias": sourceAlias}}},
	}}
	s.applyDefinition(listener)
	listenerJob := s.requireJobByAlias(listenerAlias)
	f.jobs = append(f.jobs, listenerJob.ID)
	var listenerTrigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, listenerJob.ID).Scan(&listenerTrigger))
	sourceRun := s.triggerRun(sourceJob.ID)
	s.awaitRun(sourceJob.ID, sourceRun, runTimeout)
	s.Require().Eventually(func() bool {
		_, _, adminBody := s.nsGet("/v1/triggers/"+listenerTrigger+"/events", s.authAPIKey)
		return strings.Contains(adminBody, sourceRun)
	}, runTimeout, time.Second, "the lifecycle bridge delivers the finance completion to the marketing listener")
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+listenerTrigger+"/events", mktViewer, "")
	s.requireBodyFree(body, "lifecycle deliveries", sourceRun)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/events/ingested?source=caesium&limit=500", mktViewer, "")
	s.requireBodyFree(body, "lifecycle ingested events", sourceRun)

	// Lifecycle labels: a marketing listener's delivery of a marketing
	// completion is marketing's to read, but the job labels the bridge copies
	// from the job's current row (a new owner's after a move) never reach a
	// confined caller; an unrestricted caller still receives them.
	labelSecret := "b4-lifecycle-label-" + uuid.NewString()
	ownSourceAlias := fmt.Sprintf("b4-lifecycle-own-src-%d", f.stamp)
	ownSource := s.namespacedJobDefinition(ownSourceAlias, "marketing")
	ownSource.Metadata.Labels = map[string]string{"owner": labelSecret}
	s.applyDefinition(ownSource)
	ownSourceJob := s.requireJobByAlias(ownSourceAlias)
	f.jobs = append(f.jobs, ownSourceJob.ID)
	ownListenerAlias := fmt.Sprintf("b4-lifecycle-own-dst-%d", f.stamp)
	ownListener := s.namespacedJobDefinition(ownListenerAlias, "marketing")
	ownListener.Trigger = schema.Trigger{Type: "event", Configuration: map[string]any{
		"events": []any{map[string]any{"type": "run_completed", "source": "caesium", "filter": map[string]any{"job_alias": ownSourceAlias}}},
	}}
	s.applyDefinition(ownListener)
	ownListenerJob := s.requireJobByAlias(ownListenerAlias)
	f.jobs = append(f.jobs, ownListenerJob.ID)
	var ownListenerTrigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, ownListenerJob.ID).Scan(&ownListenerTrigger))
	ownRun := s.triggerRun(ownSourceJob.ID)
	s.awaitRun(ownSourceJob.ID, ownRun, runTimeout)
	ownEvents := "/v1/triggers/" + ownListenerTrigger + "/events"
	s.Require().Eventually(func() bool {
		_, _, adminBody := s.nsGet(ownEvents, s.authAPIKey)
		return strings.Contains(adminBody, ownRun)
	}, runTimeout, time.Second, "the lifecycle bridge delivers the marketing completion to the marketing listener")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, ownEvents, s.authAPIKey, ""), labelSecret, "an unrestricted caller still receives the labels")
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, ownEvents, mktViewer, "")
	s.Contains(body, ownRun, "the owned lifecycle delivery is listed")
	s.requireBodyFree(body, "owned lifecycle deliveries", labelSecret, `"job_labels"`)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/events/ingested?source=caesium&limit=500", mktViewer, "")
	s.Contains(body, ownRun, "the owned lifecycle event is listed")
	s.requireBodyFree(body, "owned lifecycle ingested events", labelSecret, `"job_labels"`)

	// Moved-job backfill dedupe: two leap days backfilled in marketing, then
	// the job moves to finance and the same window is backfilled again with
	// reprocess none. Only the backfill's own namespace counts, so both dates
	// run again under the new owner (before B4 the marketing runs skipped
	// them and reported their outcomes).
	window := `{"start":"2020-02-28T00:00:00Z","end":"2024-03-01T00:00:00Z","reprocess":"none"}`
	bfAlias := fmt.Sprintf("b4-backfill-%d", f.stamp)
	s.applyDefinition(s.backfillDefinition(bfAlias, "marketing", 0))
	bfJob := s.requireJobByAlias(bfAlias)
	f.jobs = append(f.jobs, bfJob.ID)
	first := decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+bfJob.ID+"/backfill", mktRunner, window))
	s.Equal(float64(2), s.awaitNamespacedBackfill(bfJob.ID, first)["total_runs"])
	s.applyDefinition(s.backfillDefinition(bfAlias, "finance", 0))
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs/"+bfJob.ID+"/backfill", mktRunner, window)
	second := decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+bfJob.ID+"/backfill", finRunner, window))
	moved := s.awaitNamespacedBackfill(bfJob.ID, second)
	s.Equal(float64(2), moved["total_runs"], "the old namespace's runs do not satisfy the new owner's dates")
	s.Equal("finance", moved["namespace"])
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+bfJob.ID+"/backfills", finViewer, "")
	s.Contains(body, second)
	s.requireBodyFree(body, "backfill list", first)

	// A deterministic cancel: each child sleeps, so the backfill is still
	// running when the confined operator cancels it.
	slowAlias := fmt.Sprintf("b4-backfill-slow-%d", f.stamp)
	s.applyDefinition(s.backfillDefinition(slowAlias, "finance", 60))
	slowJob := s.requireJobByAlias(slowAlias)
	f.jobs = append(f.jobs, slowJob.ID)
	running := decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+slowJob.ID+"/backfill", finRunner, window))
	cancel := "/v1/jobs/" + slowJob.ID + "/backfills/" + running + "/cancel"
	s.requireNSStatus(http.StatusForbidden, http.MethodPut, cancel, finRunner, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPut, cancel, mktRunner, "")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodPut, cancel, finOperator, ""), running)
	s.NotEqual("running", s.awaitNamespacedBackfill(slowJob.ID, running)["status"])
}

// TestAuthNamespaceStreamOwnership drives the two stream payload rules on the
// live server. Run and task payloads carry the job's labels as they are when
// the event is built, which after a move would be the new owner's: a
// confined subscriber never receives job labels at all. And a marketing run
// refused at admission because its consumed dataset is held never streams to
// a confined subscriber (its task rows name the upstream hold).
func (s *IntegrationTestSuite) TestAuthNamespaceStreamOwnership() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("b4stream")
	defer s.closeNamespacedRoutesFixture(f)
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	clusterViewer := s.namespacedKey(f, "viewer", "").Plaintext

	alias := fmt.Sprintf("b4-labelled-%d", f.stamp)
	labelSecret := "b4-label-" + uuid.NewString()
	def := s.namespacedJobDefinition(alias, "marketing")
	def.Metadata.Labels = map[string]string{"owner": labelSecret}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)
	f.jobs = append(f.jobs, job.ID)
	run := s.triggerRun(job.ID)
	s.awaitRun(job.ID, run, runTimeout)
	untilCompleted := func(events []event.Event) bool { return streamHasRun(events, run, event.TypeRunCompleted) }
	confinedRun := s.streamEventsWithKey("?run_id="+run, mktViewer, 20*time.Second, untilCompleted)
	s.True(streamHasRun(confinedRun, run, event.TypeRunCompleted), "the owner streams its run")
	for _, evt := range confinedRun {
		s.NotContains(string(evt.Payload), labelSecret, "event %s carried job labels to a confined subscriber", evt.Type)
		s.NotContains(string(evt.Payload), `"job_labels"`)
	}
	clusterRun := s.streamEventsWithKey("?run_id="+run, clusterViewer, 20*time.Second, untilCompleted)
	leaked := false
	for _, evt := range clusterRun {
		leaked = leaked || strings.Contains(string(evt.Payload), labelSecret)
	}
	s.True(leaked, "the unrestricted stream still carries the labels")

	// A held upstream dataset (finance's producer) refuses the marketing
	// consumer's run at admission.
	dataset := fmt.Sprintf("b4-held-%d", f.stamp)
	producerAlias := fmt.Sprintf("b4-held-producer-%d", f.stamp)
	producer := s.namespacedJobDefinition(producerAlias, "finance")
	producer.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: dataset}}}
	s.applyDefinition(producer)
	producerJob := s.requireJobByAlias(producerAlias)
	f.jobs = append(f.jobs, producerJob.ID)
	holdID := uuid.NewString()
	now := time.Now().UTC()
	s.catalogExec(f, 1, `INSERT INTO dataset_holds (id, namespace, name, status, active_key, held_by_job_id, opened_at, created_at, updated_at) VALUES (?, '', ?, 'active', ?, ?, ?, ?, ?)`,
		holdID, dataset, dataset+"/"+holdID, producerJob.ID, now, now, now)
	f.undo = append(f.undo, func() { s.catalogExec(f, -1, `DELETE FROM dataset_holds WHERE id = ?`, holdID) })
	consumerAlias := fmt.Sprintf("b4-held-consumer-%d", f.stamp)
	consumer := s.namespacedJobDefinition(consumerAlias, "marketing")
	consumer.Steps[0].Datasets = &schema.StepDatasets{Consumes: []schema.ConsumedDataset{{Name: dataset}}}
	s.applyDefinition(consumer)
	consumerJob := s.requireJobByAlias(consumerAlias)
	f.jobs = append(f.jobs, consumerJob.ID)
	heldRun := s.triggerHeldRun(consumerJob.ID)
	s.Equal("skipped", heldRun.Status)
	adminEvents := s.streamEventsWithKey("?job_id="+consumerJob.ID, clusterViewer, 20*time.Second, func(events []event.Event) bool {
		return streamHasRun(events, heldRun.ID, event.TypeTaskSkipped)
	})
	s.True(streamHasRun(adminEvents, heldRun.ID, event.TypeTaskSkipped), "the refused run did emit task events")
	confined := s.streamEventsWithKey("?job_id="+consumerJob.ID, mktViewer, 5*time.Second, nil)
	s.False(streamHasRun(confined, heldRun.ID, ""), "a hold-refused run never streams to a confined subscriber")
	for _, evt := range confined {
		s.NotContains(string(evt.Payload), holdID)
	}
}

// TestRunRetryRefusesMovedNamespace runs on every lane: a run left in its
// job's previous namespace is never retried, for an unrestricted caller too,
// and the 409 names both namespaces and points at replay.
func (s *IntegrationTestSuite) TestRunRetryRefusesMovedNamespace() {
	alias := fmt.Sprintf("b4-retry-moved-%d", time.Now().UnixNano())
	s.applyDefinition(s.namespacedJobDefinition(alias, "marketing"))
	job := s.requireJobByAlias(alias)
	defer func() {
		resp, err := s.doJSONRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+job.ID, nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	runID := s.triggerRun(job.ID)
	s.awaitRun(job.ID, runID, runTimeout)
	s.applyDefinition(s.namespacedJobDefinition(alias, "finance"))

	for _, path := range []string{
		"/v1/jobs/" + job.ID + "/runs/" + runID + "/retry",
		"/v1/jobs/" + job.ID + "/runs/" + runID + "/tasks/ok/partitions/0/retry",
	} {
		resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+path, nil)
		s.Require().NoError(err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.Require().NoError(err)
		s.Require().Equal(http.StatusConflict, resp.StatusCode, string(body))
		var moved struct {
			Error        string `json:"error"`
			Message      string `json:"message"`
			RunNamespace string `json:"run_namespace"`
			JobNamespace string `json:"job_namespace"`
		}
		s.Require().NoError(json.Unmarshal(body, &moved))
		s.Equal("namespace_moved", moved.Error)
		s.Equal("marketing", moved.RunNamespace)
		s.Equal("finance", moved.JobNamespace)
		s.Contains(moved.Message, `"marketing"`)
		s.Contains(moved.Message, `"finance"`)
		s.Contains(moved.Message, "/replay")
	}
	s.Equal("succeeded", s.fetchRun(job.ID, runID).Status)
}

// TestDataAssertionBaselineStaysWithRunNamespace runs on every lane with data
// assertions on: a job whose dataset history was produced in marketing moves
// to finance, and its first finance run is judged against finance's history
// only. With no clean finance sample its deltaFromBaseline assertion yields no
// verdict, instead of failing the run against (and recording in its
// violation) marketing's baseline median and sample count.
func (s *IntegrationTestSuite) TestDataAssertionBaselineStaysWithRunNamespace() {
	s.requireDataAssertionsLane()
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("b4-assert-moved-%d", suffix)
	dataset := fmt.Sprintf("b4.assert.moved.%d", suffix)
	inNamespace := func(manifest, namespace string) string {
		withNamespace := strings.Replace(manifest, "  alias: "+alias+"\n", "  alias: "+alias+"\n  namespace: "+namespace+"\n", 1)
		s.Require().NotEqual(manifest, withNamespace, "the manifest names the alias on its own line")
		return withNamespace
	}

	seeder, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 10000})
	s.Require().NoError(err)
	job := s.applyAssertionsJob(alias, inNamespace(assertionsSeederManifest(alias, dataset, seeder), "marketing"))
	defer func() {
		resp, err := s.doJSONRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+job.ID, nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	for range baselineMinSamples {
		runID := s.triggerRun(job.ID)
		run := s.awaitRun(job.ID, runID, runTimeout)
		s.Require().Equal("succeeded", run.Status, "every seeding run must succeed: only clean samples count toward a baseline")
		s.Require().Equal("marketing", run.Namespace)
	}

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 10})
	s.Require().NoError(err)
	moved := s.applyAssertionsJob(alias, inNamespace(assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {deltaFromBaseline: 50%}", "fail"), "finance"))
	s.Require().Equal(job.ID, moved.ID, "the job moved, it was not replaced")
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("finance", run.Namespace)
	s.Equal("succeeded", run.Status, "marketing's baseline never judges the finance run")
	task := s.requireRunTaskByName(job.ID, run, "load")
	for _, violation := range task.DataViolations {
		s.NotEqual("deltaFromBaseline", violation.Assertion, "finance has no history, so there is no delta verdict: %+v", violation)
	}
}
