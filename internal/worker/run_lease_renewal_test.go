package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
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
