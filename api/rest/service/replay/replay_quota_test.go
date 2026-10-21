package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// lockedDispatcher records dispatches from concurrent requests.
type lockedDispatcher struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (d *lockedDispatcher) DispatchReplay(_ context.Context, runID uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, runID)
	return nil
}

// useDefaultQuotaPolicy serves a policy file capping "default" (the fixture
// job's namespace) at one running run.
func useDefaultQuotaPolicy(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: AccessPolicy
namespaces:
  default:
    quotas:
      maxConcurrentRuns: 1
`), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Errorf("timed out waiting for %s", what)
	}
}

// TestReplayIdenticalRequestLosingTheQuotaRaceReturnsTheWinner: two identical
// re-executing replays both miss the fingerprint lookup; the first commits a
// running replay that fills its namespace's quota of one. The second is
// refused by the quota gate, not by the fingerprint index — and must still
// answer with the first's replay, exactly as the unique-violation race does,
// not 409 namespace_quota.
func TestReplayIdenticalRequestLosingTheQuotaRaceReturnsTheWinner(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	useDefaultQuotaPolicy(t)

	// Order the race: both requests' first fingerprint lookup runs (and
	// misses) before either materializes, and the second materializes only
	// after the first has returned.
	var lookups atomic.Int32
	secondLookedUp := make(chan struct{})
	winnerDone := make(chan struct{})
	var winnerOnce sync.Once
	const hook = "test:replay_quota_race"
	require.NoError(t, f.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), "replay_fingerprint = ") {
			return
		}
		switch lookups.Add(1) {
		case 1:
			awaitSignal(t, secondLookedUp, "the second request's lookup")
		case 2:
			close(secondLookedUp)
			awaitSignal(t, winnerDone, "the first request to finish")
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove(hook) })

	dispatcher := &lockedDispatcher{}
	svc := (&Service{ctx: replayTestContext(t), store: f.store, dispatcher: dispatcher}).WithExecutionMode("distributed")
	deferrals := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)
	req := Request{
		JobID:          f.jobID,
		BaselineRunID:  f.runID,
		Set:            map[string]string{"mode": "what-if"},
		IdempotencyKey: "quota-race",
		Principal:      f.principal,
	}

	results := make([]*Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = svc.Replay(req)
			winnerOnce.Do(func() { close(winnerDone) })
		})
	}
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i], "request %d", i)
		require.NotNil(t, results[i])
	}
	require.Equal(t, results[0].Run.ID, results[1].Run.ID, "both requests answer with the one replay")
	require.NotEqual(t, results[0].Existing, results[1].Existing, "one created it, the other found it")
	require.Equal(t, runstorage.StatusRunning, results[0].Run.Status)

	fingerprint, err := Fingerprint(f.jobID, f.runID, f.principal, req.Set, req.IdempotencyKey)
	require.NoError(t, err)
	var replays int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("replay_fingerprint = ?", fingerprint).Count(&replays).Error)
	require.EqualValues(t, 1, replays)
	require.Equal(t, deferrals, metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace),
		"a request answered with the winner's replay is not a quota deferral")
}

// TestReplayRefusedByTheQuotaWithoutAWinnerStaysRefused: when the namespace
// is full of other runs, a new replay is refused with the quota error and
// nothing is written.
func TestReplayRefusedByTheQuotaWithoutAWinnerStaysRefused(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	useDefaultQuotaPolicy(t)
	require.NoError(t, f.db.Create(&models.JobRun{
		ID: uuid.New(), JobID: f.jobID, Namespace: models.DefaultNamespace, Status: string(runstorage.StatusRunning),
		StartedAt: f.now, CreatedAt: f.now, UpdatedAt: f.now,
	}).Error)

	dispatcher := &lockedDispatcher{}
	deferrals := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)
	_, err := (&Service{ctx: replayTestContext(t), store: f.store, dispatcher: dispatcher}).WithExecutionMode("distributed").Replay(Request{
		JobID:          f.jobID,
		BaselineRunID:  f.runID,
		Set:            map[string]string{"mode": "what-if"},
		IdempotencyKey: "quota-full",
		Principal:      f.principal,
	})
	require.ErrorIs(t, err, runstorage.ErrNamespaceQuota)
	require.Equal(t, deferrals+1, metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace),
		"a refusal that stands counts one deferral")
	require.Empty(t, dispatcher.calls)
	var replays int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("quarantine = ?", true).Count(&replays).Error)
	require.Zero(t, replays)
}
