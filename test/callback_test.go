//go:build integration

package test

import (
	"context"
	"fmt"
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

func newFlakyCallbackReceiver() *flakyCallbackReceiver {
	r := &flakyCallbackReceiver{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		r.calls.Add(1)
		if !r.healed.Load() {
			http.Error(w, "callback receiver is down", http.StatusInternalServerError)
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
