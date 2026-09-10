//go:build integration

package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests exercise the callback dispatcher end-to-end with a real DB (sqlite)
// to align with integration expectations while avoiding external engines.

func TestIntegrationCallbackDispatchAndRetry(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "integration-cb",
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "integration-callback-job",
		TriggerID: trig.ID,
	}
	require.NoError(t, db.Create(job).Error)

	atom := &models.Atom{
		ID:     uuid.New(),
		Engine: models.AtomEngineDocker,
		Image:  "alpine:3.23",
	}
	cmd, err := jsonutil.MarshalSliceString([]string{"echo", "hello"})
	require.NoError(t, err)
	atom.Command = cmd
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  job.ID,
		AtomID: atom.ID,
	}
	require.NoError(t, db.Create(task).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTask(runEntry.ID, task, atom, 0))
	require.NoError(t, store.StartTask(runEntry.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runEntry.ID, task.ID, "success", nil, nil))
	require.NoError(t, store.Complete(runEntry.ID, nil))

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if attempts.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cb := &models.Callback{
		ID:            uuid.New(),
		JobID:         job.ID,
		Type:          models.CallbackTypeNotification,
		Configuration: `{"url":"` + server.URL + `"}`,
	}
	require.NoError(t, db.Create(cb).Error)

	dispatcher := callback.NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())

	// First attempt should record failure.
	err = dispatcher.Dispatch(context.Background(), job.ID, runEntry.ID, nil)
	require.Error(t, err)

	var callbackRuns []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 1)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)

	// Retry should succeed and create another run record.
	err = dispatcher.RetryFailed(context.Background(), runEntry.ID)
	require.NoError(t, err)

	callbackRuns = nil
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 2)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)
	require.Equal(t, models.CallbackRunStatusSucceeded, callbackRuns[1].Status)
}

// TestRunRetryCallbacksCLI drives `caesium run retry-callbacks` against the
// LIVE server (Stream C6). The command previously had no integration coverage
// at all and — more to the point — no way to reach a remote server: it only
// opened the in-process store, which binds a native dqlite node on
// CAESIUM_NODE_ADDRESS and therefore cannot run beside a server that already
// holds that address. It now takes --server (the same Changed("server")
// convention as `caesium run retry`) and posts to the shipped
// POST /v1/jobs/:id/runs/:run_id/callbacks/retry endpoint.
//
// The receiver below 500s the run-completion dispatch, then flips to 200, so
// the retry's effect is observable at the receiver: a second delivery that only
// happens if the command really re-dispatched.
//
// The receiver binds inside the test runner process, which only shares the
// caesium server's network namespace on the docker + podman lanes (the test
// container runs with --network=container:<server-container> there). On the
// kubernetes lane the server runs in a separate kind pod reachable only via
// the one-directional kubectl port-forward the CLI uses, so a callback the
// server dispatches can never reach back to the receiver's loopback address.
func (s *IntegrationTestSuite) TestRunRetryCallbacksCLI() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("callback receiver runs in the test process and is not reachable from the caesium server pod under CAESIUM_TEST_ENGINE=%s; covered on the docker + podman lanes, where the test runner shares the server's network namespace", s.engineType)
	}

	receiver := newFlakyCallbackReceiver()
	defer receiver.Close()

	alias := fmt.Sprintf("integration-retry-callbacks-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
callbacks:
  - type: notification
    configuration:
      url: %q
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", "echo callback-ok"]
`, alias, receiver.URL())

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)
	s.Require().NotNil(jobEntry)

	runID := s.triggerRun(jobEntry.ID)
	s.Require().Equal("succeeded", s.awaitRun(jobEntry.ID, runID, runTimeout).Status)

	// The completion dispatch is asynchronous with respect to the run's
	// terminal status, so wait for the first (failing) delivery.
	s.Require().Eventually(func() bool {
		return receiver.Count() >= 1
	}, 60*time.Second, 250*time.Millisecond, "the run-completion callback must be attempted and fail")

	receiver.Heal()
	before := receiver.Count()

	stdout, stderr, err := s.runCLISeparate(
		"run", "retry-callbacks",
		"--job-id", jobEntry.ID,
		"--run-id", runID,
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium run retry-callbacks failed:\nstdout: %s\nstderr: %s", stdout, stderr)
	s.Contains(stdout, "Retried failed callbacks for run "+runID)
	s.NotContains(stdout, "Usage:", "usage must never reach stdout:\n%s", stdout)

	s.Require().Eventually(func() bool {
		return receiver.Count() > before
	}, 30*time.Second, 250*time.Millisecond,
		"retry-callbacks must re-deliver the previously failed callback")
	s.True(receiver.Succeeded(), "the healed receiver must have accepted the retried delivery")
}

// callbackRunResponse mirrors the `callbacks[]` entries of
// GET /v1/jobs/:id/runs/:run_id (internal/run.CallbackRun). It is declared here
// rather than folded into runResponse so the assertion names the exact JSON keys
// the Console reads.
type callbackRunResponse struct {
	ID           string `json:"id"`
	CallbackID   string `json:"callback_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	HTTPStatus   int    `json:"http_status"`
	ResponseBody string `json:"response_body"`
	RetryCount   int    `json:"retry_count"`
}

type runCallbacksResponse struct {
	ID        string                `json:"id"`
	Status    string                `json:"status"`
	Callbacks []callbackRunResponse `json:"callbacks"`
}

// TestRunCallbackFailureDetail drives the enriched CallbackRun fields through
// their real surface: a job whose callback points at a receiver that answers
// 500 with a body, then GET /v1/jobs/:id/runs/:run_id to prove `http_status`,
// `response_body` and `retry_count` are actually persisted and serialised.
// A unit test on the dispatcher proves the sender; only this proves the wiring
// all the way to the JSON the run detail page reads.
//
// The retry leg then re-dispatches through the shipped REST endpoint and
// asserts the second attempt row carries retry_count=1, which is the field's
// whole point: how many deliveries this callback has already burned.
//
// Same lane constraint as TestRunRetryCallbacksCLI — the receiver binds inside
// the test runner process, which shares the server's network namespace only on
// the docker and podman lanes.
func (s *IntegrationTestSuite) TestRunCallbackFailureDetail() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("callback receiver runs in the test process and is not reachable from the caesium server pod under CAESIUM_TEST_ENGINE=%s; covered on the docker + podman lanes, where the test runner shares the server's network namespace", s.engineType)
	}

	receiver := newFlakyCallbackReceiver() // never healed: every delivery 500s
	defer receiver.Close()

	alias := fmt.Sprintf("integration-callback-detail-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 3 * * *"
callbacks:
  - type: notification
    configuration:
      url: %q
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", "echo callback-detail-ok"]
`, alias, receiver.URL())

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)
	s.Require().NotNil(jobEntry)

	runID := s.triggerRun(jobEntry.ID)
	s.Require().Equal("succeeded", s.awaitRun(jobEntry.ID, runID, runTimeout).Status)

	first := s.awaitCallbackAttempts(jobEntry.ID, runID, 1)[0]
	s.Equal("failed", first.Status)
	s.Equal(http.StatusInternalServerError, first.HTTPStatus,
		"the rejecting status code must reach the API, not just the formatted error string")
	s.Contains(first.ResponseBody, callbackReceiverDownBody,
		"the target's response body must be recorded so a 500 can be triaged without the receiver's logs")
	s.Equal(0, first.RetryCount, "the run-completion dispatch is the first attempt")

	// Heal the receiver and re-dispatch through the shipped retry endpoint. The
	// endpoint propagates a still-failing delivery as a 500, so healing first
	// keeps this assertion about the recorded detail rather than about the
	// retry endpoint's error semantics (already covered by
	// TestRunRetryCallbacksCLI).
	receiver.Heal()

	resp, err := s.doRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/callbacks/retry", s.caesiumURL, jobEntry.ID, runID), nil)
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().NoError(resp.Body.Close())
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "retry request failed: %s", string(body))

	attempts := s.awaitCallbackAttempts(jobEntry.ID, runID, 2)
	byRetryCount := make(map[int]callbackRunResponse, len(attempts))
	for _, attempt := range attempts {
		byRetryCount[attempt.RetryCount] = attempt
	}
	s.Require().Len(byRetryCount, 2, "each attempt records the deliveries that preceded it")

	s.Equal("failed", byRetryCount[0].Status)
	s.Equal(http.StatusInternalServerError, byRetryCount[0].HTTPStatus)

	retried, ok := byRetryCount[1]
	s.Require().True(ok, "the retry must record retry_count=1")
	s.Equal("succeeded", retried.Status)
	s.Equal(http.StatusOK, retried.HTTPStatus,
		"the healed receiver's 200 must be recorded too, not only failure statuses")
}

// awaitCallbackAttempts polls the run detail endpoint until at least want
// completed callback attempts are visible. Callback dispatch is asynchronous
// with respect to the run's terminal status, so the rows appear after the run
// is already succeeded.
func (s *IntegrationTestSuite) awaitCallbackAttempts(jobID, runID string, want int) []callbackRunResponse {
	s.T().Helper()

	var attempts []callbackRunResponse
	s.Require().Eventually(func() bool {
		var detail runCallbacksResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &detail); err != nil {
			return false
		}
		completed := make([]callbackRunResponse, 0, len(detail.Callbacks))
		for _, cb := range detail.Callbacks {
			if cb.Status == "succeeded" || cb.Status == "failed" {
				completed = append(completed, cb)
			}
		}
		if len(completed) < want {
			return false
		}
		attempts = completed
		return true
	}, 60*time.Second, 250*time.Millisecond,
		"expected at least %d completed callback attempts on run %s", want, runID)

	return attempts
}

// flakyCallbackReceiver answers 500 until Heal is called, then 200. It records
// every delivery so a test can prove a retry actually reached the wire. It
// listens inside the test runner container, which shares the server's network
// namespace on the docker + podman integration lanes, so the server can reach
// it on loopback there (see TestRunRetryCallbacksCLI for the kubernetes lane,
// where that does not hold).
type flakyCallbackReceiver struct {
	server    *httptest.Server
	calls     atomic.Int32
	healed    atomic.Bool
	succeeded atomic.Bool
}

// callbackReceiverDownBody is the body an unhealed receiver answers with. It is
// a constant because TestRunCallbackFailureDetail asserts it survives the round
// trip into CallbackRun.response_body.
const callbackReceiverDownBody = "callback receiver is down"

func newFlakyCallbackReceiver() *flakyCallbackReceiver {
	r := &flakyCallbackReceiver{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		r.calls.Add(1)
		if !r.healed.Load() {
			http.Error(w, callbackReceiverDownBody, http.StatusInternalServerError)
			return
		}
		r.succeeded.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *flakyCallbackReceiver) URL() string     { return r.server.URL }
func (r *flakyCallbackReceiver) Count() int      { return int(r.calls.Load()) }
func (r *flakyCallbackReceiver) Heal()           { r.healed.Store(true) }
func (r *flakyCallbackReceiver) Succeeded() bool { return r.succeeded.Load() }
func (r *flakyCallbackReceiver) Close()          { r.server.Close() }
