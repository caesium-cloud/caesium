package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// mockRunLeaseRenewer is a test double for RunLeaseRenewer.
type mockRunLeaseRenewer struct {
	mu            sync.Mutex
	renewCalls    int
	ownedCount    int64 // rows the next RenewOwnedLeases call should return
	renewedExpiry []time.Time
	errorOnRenew  error
}

func (m *mockRunLeaseRenewer) RenewOwnedLeases(_ context.Context, _ string, newExpiry time.Time) (int64, error) {
	if m.errorOnRenew != nil {
		return 0, m.errorOnRenew
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renewCalls++
	m.renewedExpiry = append(m.renewedExpiry, newExpiry)
	return m.ownedCount, nil
}

// noopRunLeaseClaimer satisfies TaskClaimer and always returns nil (no task).
type noopRunLeaseClaimer struct{}

func (noopRunLeaseClaimer) ClaimNext(_ context.Context) (*models.TaskRun, error) {
	return nil, nil
}

// TestRunRunLeaseRenewal_SingleRoundTrip verifies that the renewal path issues
// exactly one RenewOwnedLeases call regardless of how many runs are owned
// (the database does the filtering server-side).
func TestRunRunLeaseRenewal_SingleRoundTrip(t *testing.T) {
	renewer := &mockRunLeaseRenewer{ownedCount: 3}

	w := &Worker{
		runLeaseRenewer: renewer,
		runLeaseTTL:     30 * time.Second,
		runLeaseNodeID:  "10.0.0.1:9001",
	}

	w.renewRunLeasesNow(context.Background())

	renewer.mu.Lock()
	defer renewer.mu.Unlock()
	require.Equal(t, 1, renewer.renewCalls, "single RenewOwnedLeases call expected")
}

// TestRunRunLeaseRenewal_GaugeResetsToZero verifies that when no runs are
// owned (RenewOwnedLeases returns 0), the gauge is set to 0 rather than
// holding its last non-zero value.
func TestRunRunLeaseRenewal_GaugeResetsToZero(t *testing.T) {
	renewer := &mockRunLeaseRenewer{ownedCount: 0}

	w := &Worker{
		runLeaseRenewer: renewer,
		runLeaseTTL:     30 * time.Second,
		runLeaseNodeID:  "10.0.0.1:9001",
	}

	// Prime the gauge with a non-zero value to ensure the reset is observable.
	metrics.RunLeasesOwned.Set(7)

	w.renewRunLeasesNow(context.Background())

	require.Equal(t, float64(0), metricstestutil.GaugeValue(t, metrics.RunLeasesOwned),
		"gauge must reset to 0 when no runs are owned")
}

// TestRunRunLeaseRenewal_ExtendsByLeaseTTL verifies that the new expiry is
// approximately now + leaseTTL.
func TestRunRunLeaseRenewal_ExtendsByLeaseTTL(t *testing.T) {
	renewer := &mockRunLeaseRenewer{ownedCount: 1}

	const leaseTTL = 30 * time.Second

	w := &Worker{
		runLeaseRenewer: renewer,
		runLeaseTTL:     leaseTTL,
		runLeaseNodeID:  "10.0.0.1:9001",
	}

	before := time.Now().UTC()
	w.renewRunLeasesNow(context.Background())

	renewer.mu.Lock()
	defer renewer.mu.Unlock()

	require.Len(t, renewer.renewedExpiry, 1)
	expiry := renewer.renewedExpiry[0]
	require.WithinDuration(t, before.Add(leaseTTL), expiry, time.Second,
		"new expiry must be approximately now + leaseTTL")
}

// TestWithRunLeaseRenewal_NilWhenFlagOff verifies that when
// WithRunLeaseRenewal is not called, runLeaseRenewer is nil and
// renewRunLeasesNow is a harmless no-op.
func TestWithRunLeaseRenewal_NilWhenFlagOff(t *testing.T) {
	w := NewWorker(
		noopRunLeaseClaimer{},
		NewPool(1),
		100*time.Millisecond,
		func(_ context.Context, _ *models.TaskRun) {},
	)

	require.Nil(t, w.runLeaseRenewer,
		"runLeaseRenewer must be nil when WithRunLeaseRenewal is not called")

	// Should be a no-op; must not panic.
	w.renewRunLeasesNow(context.Background())
}

// --- Claim-loss cancellation (A4) ---
//
// A cancelled run strips claimed_by from its task rows (cancelRunTx), and a
// reassigned lease overwrites it, so in both cases the node's next batched
// RenewLeases matches fewer rows than it was given. That count is the ONLY
// signal a worker gets, and until these tests it was discarded: the container
// kept running after `caesium run cancel`, and after a lease expiry it ran
// alongside the container of the node that had re-claimed the same task.

// trackCancellable registers an in-flight claim with a real cancel func and
// returns the context that claim's executor would be running under.
func trackCancellable(w *Worker, nodeID string, expiresAt time.Time) (*models.TaskRun, context.Context) {
	task := makeTask(nodeID, expiresAt)
	ctx, cancel := context.WithCancel(context.Background())
	w.trackInFlight(task, cancel)
	return task, ctx
}

// TestBatchedRenewal_LostClaimCancelsTask is the single-claim case: the batch
// already names the loser, so no probe is needed and the task is cancelled.
func TestBatchedRenewal_LostClaimCancelsTask(t *testing.T) {
	renewer := &fakeLeaseRenewer{
		rowsAffectedFn: func(_ string, _ []uuid.UUID) int64 { return 0 },
	}
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, 5*time.Minute, 0)

	task, taskCtx := trackCancellable(w, "node-a", time.Now().Add(time.Minute))

	w.renewLeasesNow(t.Context())

	require.Equal(t, 1, renewer.callCount(),
		"a single-claim group needs no probe — the batch already named the loser")
	require.ErrorIs(t, taskCtx.Err(), context.Canceled,
		"losing the claim must cancel the task's context so monitorTask stops the container")

	w.inFlightMu.Lock()
	_, stillTracked := w.inFlight[task.ID]
	w.inFlightMu.Unlock()
	require.False(t, stillTracked, "a cancelled task must leave the in-flight set, not be re-probed every tick")
}

// TestBatchedRenewal_PartialLossCancelsOnlyTheLostTask is the batched case: the
// count says "one of these three is gone" and the worker must find out WHICH.
// Cancelling the wrong one — or all of them — would kill live work.
func TestBatchedRenewal_PartialLossCancelsOnlyTheLostTask(t *testing.T) {
	var lostID uuid.UUID
	renewer := &fakeLeaseRenewer{}
	renewer.rowsAffectedFn = func(_ string, ids []uuid.UUID) int64 {
		var n int64
		for _, id := range ids {
			if id != lostID {
				n++
			}
		}
		return n
	}

	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, 5*time.Minute, 0)

	imminent := time.Now().Add(time.Minute)
	keepTask, keepCtx := trackCancellable(w, "node-a", imminent)
	lostTask, lostCtx := trackCancellable(w, "node-a", imminent)
	otherTask, otherCtx := trackCancellable(w, "node-a", imminent)
	lostID = lostTask.ID

	w.renewLeasesNow(t.Context())

	require.ErrorIs(t, lostCtx.Err(), context.Canceled, "the task whose claim vanished must be cancelled")
	require.NoError(t, keepCtx.Err(), "a task whose claim still holds must keep running")
	require.NoError(t, otherCtx.Err(), "a task whose claim still holds must keep running")

	// 1 batched UPDATE + one probe per id.
	require.Equal(t, 4, renewer.callCount(),
		"the losers are identified by re-issuing the renewal one id at a time")

	w.inFlightMu.Lock()
	_, lostTracked := w.inFlight[lostTask.ID]
	keepClaim := w.inFlight[keepTask.ID]
	otherClaim := w.inFlight[otherTask.ID]
	w.inFlightMu.Unlock()
	require.False(t, lostTracked)
	require.True(t, keepClaim.claimExpiresAt.After(imminent), "a survivor's expiry must still advance")
	require.True(t, otherClaim.claimExpiresAt.After(imminent), "a survivor's expiry must still advance")
}

// TestBatchedRenewal_ProbeErrorDoesNotCancel pins the fail-safe direction: a
// transient error while probing is "unknown", not "lost". Cancelling on it
// would let a database blip kill a healthy container.
func TestBatchedRenewal_ProbeErrorDoesNotCancel(t *testing.T) {
	renewer := &erroringProbeRenewer{}
	w := NewWorker(&sequenceClaimer{}, NewPool(1), time.Millisecond, nil).
		WithLeaseRenewal(renewer, 5*time.Minute, 0)

	imminent := time.Now().Add(time.Minute)
	_, ctxA := trackCancellable(w, "node-a", imminent)
	_, ctxB := trackCancellable(w, "node-a", imminent)

	w.renewLeasesNow(t.Context())

	require.NoError(t, ctxA.Err(), "a probe error must not cancel a task")
	require.NoError(t, ctxB.Err(), "a probe error must not cancel a task")
}

// erroringProbeRenewer reports a partial batch match and then fails every
// single-id probe.
type erroringProbeRenewer struct {
	mu    sync.Mutex
	calls int
}

func (e *erroringProbeRenewer) RenewLeases(_ context.Context, _ string, ids []uuid.UUID, _ time.Time) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if len(ids) > 1 {
		return int64(len(ids) - 1), nil
	}
	return 0, errProbeFailed
}

var errProbeFailed = errors.New("probe failed")
