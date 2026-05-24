package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// mockRunLeaseRenewer is a test double for RunLeaseRenewer.
type mockRunLeaseRenewer struct {
	mu            sync.Mutex
	renewCalls    int
	ownedRunIDs   []uuid.UUID
	renewedIDs    [][]uuid.UUID
	renewedExpiry []time.Time
	errorOnRenew  error
}

func (m *mockRunLeaseRenewer) OwnedRuns(_ context.Context, _ string) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uuid.UUID(nil), m.ownedRunIDs...), nil
}

func (m *mockRunLeaseRenewer) RenewRunLeases(_ context.Context, _ string, ids []uuid.UUID, newExpiry time.Time) (int64, error) {
	if m.errorOnRenew != nil {
		return 0, m.errorOnRenew
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renewCalls++
	m.renewedIDs = append(m.renewedIDs, append([]uuid.UUID(nil), ids...))
	m.renewedExpiry = append(m.renewedExpiry, newExpiry)
	return int64(len(ids)), nil
}

// noopRunLeaseClaimer satisfies TaskClaimer and always returns nil (no task).
type noopRunLeaseClaimer struct{}

func (noopRunLeaseClaimer) ClaimNext(_ context.Context) (*models.TaskRun, error) {
	return nil, nil
}

// TestRunRunLeaseRenewal_BatchesAllOwnedRuns verifies that the renewal
// function issues a single batched UPDATE for all runs owned by this node.
func TestRunRunLeaseRenewal_BatchesAllOwnedRuns(t *testing.T) {
	renewer := &mockRunLeaseRenewer{
		ownedRunIDs: []uuid.UUID{uuid.New(), uuid.New(), uuid.New()},
	}

	w := &Worker{
		runLeaseRenewer: renewer,
		runLeaseTTL:     30 * time.Second,
		runLeaseNodeID:  "10.0.0.1:9001",
	}

	w.renewRunLeasesNow(context.Background())

	renewer.mu.Lock()
	defer renewer.mu.Unlock()

	require.Equal(t, 1, renewer.renewCalls, "single batched renewal call expected")
	require.Len(t, renewer.renewedIDs[0], 3, "all 3 owned runs must be in one call")
}

// TestRunRunLeaseRenewal_SkipWhenNoneOwned verifies that no RenewRunLeases
// call is made when the node owns no runs.
func TestRunRunLeaseRenewal_SkipWhenNoneOwned(t *testing.T) {
	renewer := &mockRunLeaseRenewer{
		ownedRunIDs: []uuid.UUID{},
	}

	w := &Worker{
		runLeaseRenewer: renewer,
		runLeaseTTL:     30 * time.Second,
		runLeaseNodeID:  "10.0.0.1:9001",
	}

	w.renewRunLeasesNow(context.Background())

	renewer.mu.Lock()
	defer renewer.mu.Unlock()

	require.Equal(t, 0, renewer.renewCalls,
		"no renewal call should be made when no runs are owned")
}

// TestRunRunLeaseRenewal_ExtendsByLeaseTTL verifies that the new expiry is
// approximately now + leaseTTL.
func TestRunRunLeaseRenewal_ExtendsByLeaseTTL(t *testing.T) {
	renewer := &mockRunLeaseRenewer{
		ownedRunIDs: []uuid.UUID{uuid.New()},
	}

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
