package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func observationConfig() *Config {
	return &Config{Version: 1, Connections: []Connection{{ID: "primary", Provider: ProviderTemporal, Endpoint: "localhost:7233", Scope: "default", Enabled: true, Limits: DefaultLimits()}}}
}
func TestObservationActivationGuardsEpochAndRemovedIdentity(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	cfg := observationConfig()
	require.NoError(t, store.ActivateConfiguration(ctx, cfg, "", "first"))
	var writes atomic.Int64
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("observe-count-create", func(*gorm.DB) { writes.Add(1) }))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("observe-count-update", func(*gorm.DB) { writes.Add(1) }))
	require.NoError(t, store.ActivateConfiguration(ctx, cfg, "", "first"))
	require.Zero(t, writes.Load(), "matching nodes should join without a write")
	require.ErrorIs(t, store.ActivateConfiguration(ctx, cfg, "", "second"), ErrEpochMismatch)
	require.NoError(t, store.ActivateConfiguration(ctx, &Config{Version: 1}, "first", "removed"))
	cfg.Connections[0].Endpoint = "localhost:7234"
	require.ErrorContains(t, store.ActivateConfiguration(ctx, cfg, "removed", "repoint"), "immutable")
	active, err := store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, "removed", active)
	cfg.Connections[0].ID = "replacement"
	require.NoError(t, store.ActivateConfiguration(ctx, cfg, "removed", "new"))
	require.ErrorIs(t, store.RequireEpoch(ctx, "first"), ErrEpochMismatch)
}
func TestRecordDirectUnchangedPollingHasNoCatalogWrites(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	limits := DefaultLimits()
	ref := mustExecutionRef(t, "primary", "same")
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "", "epoch"))
	at := time.Now().UTC()
	obs := directObservation(ref, 1, DisplayRunning, "event1", at, []byte(`{"status":"running"}`))
	result, err := store.RecordDirect(ctx, "epoch", obs, limits)
	require.NoError(t, err)
	require.True(t, result.Wrote)
	var writes atomic.Int64
	count := func(*gorm.DB) { writes.Add(1) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("observe-create", count))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("observe-update", count))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("observe-delete", count))
	for generation := int64(2); generation < 10; generation++ {
		obs.Generation = generation
		obs.ObservedAt = at.Add(time.Second)
		result, err = store.RecordDirect(ctx, "epoch", obs, limits)
		require.NoError(t, err)
		require.False(t, result.Wrote)
		require.WithinDuration(t, at, result.ObservedAt, time.Microsecond)
	}
	require.Zero(t, writes.Load())
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "epoch", "new"))
	obs.Generation = 10
	obs.SourceEventID = "event2"
	obs.DisplayStatus = DisplayCompleted
	_, err = store.RecordDirect(ctx, "epoch", obs, limits)
	require.ErrorIs(t, err, ErrEpochMismatch)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))
}
func TestDirectRetentionBoundsLatestSnapshotsAndPreservesReferenced(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	limits := DefaultLimits()
	limits.MaxUnreferencedSnapshots = 2
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "", "epoch"))
	pinned := mustExecutionRef(t, "primary", "referenced")
	_, err := store.RecordDirect(ctx, "epoch", directObservation(pinned, 1, DisplayRunning, "event", time.Now()), limits)
	require.NoError(t, err)
	require.NoError(t, store.MarkReferenced(ctx, pinned.OpaqueID()))
	for i := range 5 {
		ref := mustExecutionRef(t, "primary", fmt.Sprint(i))
		_, err := store.RecordDirect(ctx, "epoch", directObservation(ref, 1, DisplayRunning, "event", time.Now()), limits)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	_, err = store.Snapshot(ctx, pinned, limits)
	require.NoError(t, err)
	old := mustExecutionRef(t, "primary", "aged")
	_, err = store.RecordDirect(ctx, "epoch", directObservation(old, 1, DisplayRunning, "event", time.Now().Add(-25*time.Hour)), limits)
	require.NoError(t, err)
	_, err = store.Snapshot(ctx, old, limits)
	require.ErrorIs(t, err, ErrNotFound)
}
func TestReadGuardCoalescesAndBoundsDistinctRPCs(t *testing.T) {
	store := NewStore(openStoreDB(t))
	ctx := context.Background()
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "", "epoch"))
	limits := DefaultLimits()
	limits.MaxInFlightRPCs = 1
	limits.ReadsPerMinute = 2
	guard := NewReadGuard(store, "epoch", limits)
	require.NoError(t, guard.Allow(ctx, "actor"))
	require.NoError(t, guard.Allow(ctx, "actor"))
	require.ErrorIs(t, guard.Allow(ctx, "actor"), ErrReadBudget)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	fn := func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []byte(`{"ok":true}`), nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); _, err := guard.Do(ctx, "same", fn); errs <- err }()
	<-entered
	_, err := guard.Do(ctx, "other", fn)
	require.ErrorIs(t, err, ErrBusy)
	wg.Add(1)
	go func() { defer wg.Done(); _, err := guard.Do(ctx, "same", fn); errs <- err }()
	// The in-flight follower is explicitly observed before releasing the leader.
	require.Eventually(t, func() bool { guard.mu.Lock(); defer guard.mu.Unlock(); return len(guard.flights) == 1 }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), calls.Load())
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "epoch", "new"))
	_, err = guard.Do(ctx, "after", fn)
	require.ErrorIs(t, err, ErrEpochMismatch)
}
func TestStatelessExecutionReferencesDoNotInsertCatalogRows(t *testing.T) {
	db := openStoreDB(t)
	ref := mustExecutionRef(t, "primary", "opaque")
	decoded, err := ParseReferenceID("primary", ReferenceID(ref))
	require.NoError(t, err)
	require.Equal(t, ref, decoded)
	_, err = ParseReferenceID("other", ReferenceID(ref))
	require.ErrorIs(t, err, ErrInvalidReference)
	require.Zero(t, countModel(t, db, &models.ExternalExecution{}))
}

func TestObservationGenerationUsesSharedCatalogAndRefusesOverflow(t *testing.T) {
	store := NewStore(openStoreDB(t))
	ctx := context.Background()
	limits := DefaultLimits()
	ref := mustExecutionRef(t, "primary", "clock-independent")
	require.NoError(t, store.ActivateConfiguration(ctx, observationConfig(), "", "epoch"))
	// A snapshot from a fast-clock node is deliberately far ahead of any local
	// request count. A matching node derives its next fence from that catalog.
	_, err := store.RecordDirect(ctx, "epoch", directObservation(ref, 1<<50, DisplayRunning, "event1", time.Now().Add(time.Hour)), limits)
	require.NoError(t, err)
	generation, err := store.NextObservationGeneration(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, int64(1<<50)+1, generation)
	// Real later source evidence from a node whose wall time is earlier persists.
	_, err = store.RecordDirect(ctx, "epoch", directObservation(ref, generation, DisplayCompleted, "event2", time.Now().Add(-time.Hour)), limits)
	require.NoError(t, err)
	require.NoError(t, store.db.Model(&models.ExternalExecution{}).Where("opaque_id = ?", ref.OpaqueID()).Update("latest_generation", int64(math.MaxInt64)).Error)
	_, err = store.NextObservationGeneration(ctx, ref)
	require.ErrorIs(t, err, ErrGenerationExhausted)
}
func TestFreshConfigurationRejectsPreviousFingerprintGuard(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.ErrorIs(t, store.ActivateConfiguration(context.Background(), observationConfig(), "expected-existing", "new"), ErrEpochMismatch)
	_, err := store.ActiveEpoch(context.Background())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestExecutionReferenceRetainsDurableW4JSONCodec(t *testing.T) {
	ref := ExecutionReference{ConnectionID: "primary", Coordinates: map[string]string{"workflow_id": "workflow", "run_id": "run"}}
	encoded, err := json.Marshal(ref)
	require.NoError(t, err)
	require.JSONEq(t, `{"ConnectionID":"primary","Coordinates":{"workflow_id":"workflow","run_id":"run"}}`, string(encoded))
	var restored ExecutionReference
	require.NoError(t, json.Unmarshal([]byte(`{"ConnectionID":"primary","Coordinates":{"workflow_id":"workflow","run_id":"run"}}`), &restored))
	require.Equal(t, ref, restored)
}
