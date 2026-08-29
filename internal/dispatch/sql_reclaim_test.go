package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The SQL lane discovers expired-claim reaping through an optional interface, so
// a signature drift on *run.Store would silently turn the sweep off rather than
// fail the build.  This assertion is what makes that a compile error instead.
var _ expiredClaimReclaimer = (*run.Store)(nil)

// postJSONTo posts a JSON body to a handler with an explicit bearer token.  The
// shared postJSON helper hardcodes testToken; the loop-driven tests here run
// against a handler that must accept the loop's own token.
func postJSONTo(t *testing.T, handler http.HandlerFunc, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

// killWorkerHoldingClaim rewrites a claimed row to look like the worker holding
// it died: the claim lease lapsed and a container id was left behind.
func killWorkerHoldingClaim(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) {
	t.Helper()
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"claim_expires_at": time.Now().UTC().Add(-time.Minute),
			"runtime_id":       "container-abc",
		}).Error)
}

// TestDispatchRun_SQLModeReclaimsExpiredClaimAndCompletesRun closes the SQL
// lane's half of the owner-side reaping hole.
//
// Claimer.ReclaimExpired's live-lease guard skips rows belonging to a run whose
// owner is alive — in BOTH owner modes, because both hold a run lease — and
// PendingTasksForDispatch only ever returns `pending` rows.  So in SQL owner
// mode a worker that died mid-task left its row `running` with a dead claim,
// invisible to the reaper and to the poll alike, and the task was never re-run.
// Unlike the in-memory lane there is no in-memory state to keep in step here:
// resetting the row to pending is the whole fix, and the next poll dispatches it.
func TestDispatchRun_SQLModeReclaimsExpiredClaimAndCompletesRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID := seedPendingTaskRun(t, store)

	// A REAL dispatch handler on the far side, so the re-dispatch is genuinely
	// claimed rather than merely acknowledged by a stub.
	sub := &fakeSubmitter{}
	var handler *Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/dispatch":
			handler.HandleDispatch(w, r)
		case "/internal/complete":
			handler.HandleComplete(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)
	handler = NewHandler(store, ls, nodeID, loopToken).WithWorkerSubmitter(sub)

	generation, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)

	// The first dispatch lands and is claimed...
	_, err = store.ClaimTaskForDispatch(runID, taskID, "dead-worker:9001", generation, 1, 30*time.Second, false)
	require.NoError(t, err)
	// ...and then that worker dies.
	killWorkerHoldingClaim(t, db, runID, taskID)

	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status, "precondition: the row is stuck running")
	require.Equal(t, "dead-worker:9001", row.ClaimedBy)

	// SQL owner mode: no OwnerManager.
	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   40 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseTTL:   30 * time.Second,
		LeaseStore: &testOwnerReader{ls},
		Store:      store,
		Peers:      &testPeerLister{},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	// The stuck row was reclaimed, re-dispatched, and claimed by the live node.
	require.Len(t, sub.accepted, 1, "the reclaimed task must be re-dispatched and accepted")
	require.Equal(t, taskID, sub.accepted[0].Task.TaskID)

	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status)
	require.Equal(t, nodeID, row.ClaimedBy, "the live node now holds the claim, not the dead worker")
	require.Equal(t, 2, row.ClaimAttempt, "the reclaimed claim must advance its durable identity")
	require.NotNil(t, row.ClaimExpiresAt)
	require.True(t, row.ClaimExpiresAt.After(time.Now().UTC()), "the re-claim carries a fresh lease")

	// The worker runs it and reports back through the real completion endpoint.
	w := postJSONTo(t, handler.HandleComplete, loopToken, CompleteRequest{
		RunID:           runID,
		TaskID:          taskID,
		TaskRunID:       row.ID,
		OwnerGeneration: generation,
		Attempt:         row.ClaimAttempt,
		WorkerNode:      nodeID,
		Status:          string(run.TaskStatusSucceeded),
		Result:          "success",
	})
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)

	var outstanding int64
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status NOT IN ?", runID,
			[]string{
				string(run.TaskStatusSucceeded), string(run.TaskStatusFailed),
				string(run.TaskStatusSkipped), string(run.TaskStatusCached),
				string(run.TaskStatusCancelled),
			}).
		Count(&outstanding).Error)
	require.Zero(t, outstanding, "no task may be left non-terminal")

	// Finalizing the run is internal/job's waitForRunCompletion in this lane, not
	// the dispatch loop's — this is the call it makes once no task is outstanding.
	require.NoError(t, store.Complete(runID, nil))
	var jobRun models.JobRun
	require.NoError(t, db.First(&jobRun, "id = ?", runID).Error)
	require.Equal(t, string(run.StatusSucceeded), jobRun.Status,
		"the run finishes once the lost task has been re-run")
}

// TestDispatchRun_SQLModeLeavesLiveClaimsAlone: a claim that has NOT lapsed is
// live work.  Resetting it would race the worker into double-execution — the
// exact thing Claimer.ReclaimExpired's live-lease guard exists to prevent.
func TestDispatchRun_SQLModeLeavesLiveClaimsAlone(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID := seedPendingTaskRun(t, store)

	var dispatched int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched++
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	nodeID, apiPort := serverNodeID(t, server)

	generation, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	_, err = store.ClaimTaskForDispatch(runID, taskID, "busy-worker:9001", generation, 1, time.Hour, false)
	require.NoError(t, err)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   40 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseTTL:   30 * time.Second,
		LeaseStore: &testOwnerReader{ls},
		Store:      store,
		Peers:      &testPeerLister{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	require.Zero(t, dispatched, "a live claim must not be reclaimed and re-dispatched")
	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	require.Equal(t, string(run.TaskStatusRunning), row.Status)
	require.Equal(t, "busy-worker:9001", row.ClaimedBy)
}

// TestDispatchLoop_ReclaimClockIsThrottledAndBounded pins the two properties
// that keep the sweep off the hot path: one query per run per interval, and a
// clock map that tracks the owned set instead of growing for the life of the
// process.
func TestDispatchLoop_ReclaimClockIsThrottledAndBounded(t *testing.T) {
	loop := NewDispatchLoop(DispatchLoopConfig{NodeID: "n:9001"})
	runID := uuid.New()
	now := time.Now()

	require.True(t, loop.dueForReclaim(runID, now), "a run seen for the first time is always due")
	require.False(t, loop.dueForReclaim(runID, now.Add(ownerReclaimInterval-time.Millisecond)),
		"a second sweep inside the interval must be skipped")
	require.True(t, loop.dueForReclaim(runID, now.Add(ownerReclaimInterval)),
		"the sweep resumes once the interval has elapsed")

	loop.forgetUnownedReclaims(map[uuid.UUID]run.LeaseVersion{runID: {Generation: 1}})
	loop.reclaimMu.Lock()
	require.Len(t, loop.lastReclaim, 1, "an owned run keeps its clock")
	loop.reclaimMu.Unlock()

	loop.forgetUnownedReclaims(map[uuid.UUID]run.LeaseVersion{})
	loop.reclaimMu.Lock()
	require.Empty(t, loop.lastReclaim, "a run this node no longer owns must not leak its clock")
	loop.reclaimMu.Unlock()
}
