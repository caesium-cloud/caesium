package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRecordSnapshotRejectsConcurrentTerminalRegression(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-terminal")
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	wrote, err := store.RecordSnapshot(ctx, directObservation(ref, 1, DisplayRunning, "event-run", at))
	require.NoError(t, err)
	require.True(t, wrote.Wrote)

	terminalAt := at.Add(time.Minute)
	wrote, err = store.RecordSnapshot(ctx, directObservation(ref, 2, DisplayCompleted, "event-done", terminalAt))
	require.NoError(t, err)
	require.True(t, wrote.Wrote)
	require.Equal(t, int64(2), countModel(t, db, &models.ExternalExecutionSnapshot{}))

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.RecordSnapshot(ctx, directObservation(ref, int64(10+i), DisplayRunning, fmt.Sprintf("regress-%d", i), terminalAt.Add(time.Duration(i+1)*time.Second)))
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.ErrorIs(t, err, ErrTerminalRegression)
	}
	require.Equal(t, int64(2), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	var terminal int64
	require.NoError(t, db.Model(&models.ExternalExecutionSnapshot{}).Where("terminal = ?", true).Count(&terminal).Error)
	require.Equal(t, int64(1), terminal)

	// A newer terminal observation may append. A non-terminal one still may not.
	wrote, err = store.RecordSnapshot(ctx, directObservation(ref, 30, DisplayFailed, "event-failed", terminalAt.Add(time.Hour)))
	require.NoError(t, err)
	require.True(t, wrote.Wrote)
	_, err = store.RecordSnapshot(ctx, directObservation(ref, 31, DisplayTimedOut, "event-timeout", terminalAt.Add(2*time.Hour)))
	require.NoError(t, err)
	_, err = store.RecordSnapshot(ctx, directObservation(ref, 32, DisplayRunning, "event-back", terminalAt.Add(3*time.Hour)))
	require.ErrorIs(t, err, ErrTerminalRegression)
	_, err = store.RecordSnapshot(ctx, directObservation(ref, 1, DisplayRunning, "event-late", terminalAt.Add(4*time.Hour)))
	require.ErrorIs(t, err, ErrStaleGeneration)
	require.Equal(t, int64(4), countModel(t, db, &models.ExternalExecutionSnapshot{}))
}

func TestRecordSnapshotConcurrentSameGenerationHasOneWinner(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-race")
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.RecordSnapshot(ctx, directObservation(ref, 1, DisplayRunning, fmt.Sprintf("event-%d", i), at.Add(time.Duration(i)*time.Second)))
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.ErrorIs(t, err, ErrStaleGeneration)
	}
	require.Equal(t, 1, wins)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecution{}))
}

func TestAdmitOperationCollapsesIdempotencyKey(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-op")
	requireDirect(t, store, ref)

	req := operationRequest(ref, "key-1", "fingerprint-a")
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	decisions := make(chan OperationDecision, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision, err := store.AdmitOperation(ctx, req)
			if err != nil {
				errs <- err
				return
			}
			decisions <- decision
		}()
	}
	close(start)
	wg.Wait()
	close(decisions)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var firstID uuid.UUID
	created := 0
	seen := 0
	for decision := range decisions {
		seen++
		require.False(t, decision.Conflict)
		if firstID == uuid.Nil {
			firstID = decision.Operation.ID
		}
		require.Equal(t, firstID, decision.Operation.ID)
		if decision.Created {
			created++
		}
	}
	require.Equal(t, n, seen)
	require.Equal(t, 1, created)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))

	replay, err := store.AdmitOperation(ctx, req)
	require.NoError(t, err)
	require.False(t, replay.Created)
	require.False(t, replay.Conflict)
	require.Equal(t, firstID, replay.Operation.ID)

	changed := req
	changed.RequestFingerprint = "fingerprint-b"
	_, err = store.AdmitOperation(ctx, changed)
	require.ErrorIs(t, err, ErrIdempotencyMismatch)
	otherAction := req
	otherAction.Action = "signal"
	_, err = store.AdmitOperation(ctx, otherAction)
	require.ErrorIs(t, err, ErrIdempotencyMismatch)
	otherExec := operationRequest(mustExecutionRef(t, "primary", "exec-other"), "key-1", "fingerprint-a")
	requireDirect(t, store, otherExec.Execution)
	_, err = store.AdmitOperation(ctx, otherExec)
	require.ErrorIs(t, err, ErrIdempotencyMismatch)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))
	var stored models.ConnectorOperation
	require.NoError(t, db.First(&stored, "id = ?", firstID).Error)
	require.Equal(t, "fingerprint-a", stored.RequestFingerprint)
}

func TestAdmitOperationConflictsOnOpenTarget(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-open")
	requireDirect(t, store, ref)

	require.NotEqual(t,
		models.OperationOpenKey("a/b", "c", "approve"),
		models.OperationOpenKey("a", "b/c", "approve"),
	)

	firstReq := operationRequest(ref, "key-a", "fp-a")
	secondReq := operationRequest(ref, "key-b", "fp-b")
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	type result struct {
		decision OperationDecision
		err      error
		key      string
	}
	out := make(chan result, n)
	for i := range n {
		wg.Add(1)
		req := firstReq
		if i%2 == 1 {
			req = secondReq
		}
		go func(req OperationRequest) {
			defer wg.Done()
			<-start
			decision, err := store.AdmitOperation(ctx, req)
			out <- result{decision: decision, err: err, key: req.IdempotencyKey}
		}(req)
	}
	close(start)
	wg.Wait()
	close(out)

	var got []result
	created := 0
	var winner uuid.UUID
	winnerKey := ""
	for item := range out {
		require.NoError(t, item.err)
		got = append(got, item)
		if item.decision.Created {
			created++
			require.False(t, item.decision.Conflict)
			winnerKey = item.decision.Operation.IdempotencyKey
		}
		if winner == uuid.Nil {
			winner = item.decision.Operation.ID
		}
		require.Equal(t, winner, item.decision.Operation.ID)
	}
	require.Len(t, got, n)
	require.Equal(t, 1, created)
	require.NotEqual(t, uuid.Nil, winner)
	require.NotEmpty(t, winnerKey)
	for _, item := range got {
		if item.decision.Operation.IdempotencyKey != item.key {
			require.True(t, item.decision.Conflict)
			require.False(t, item.decision.Created)
		}
	}
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))

	loser := firstReq
	if winnerKey == firstReq.IdempotencyKey {
		loser = secondReq
	}
	again, err := store.AdmitOperation(ctx, loser)
	require.NoError(t, err)
	require.False(t, again.Created)
	require.True(t, again.Conflict)
	require.Equal(t, winner, again.Operation.ID)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))
}

func TestUnknownOperationKeepsOpenGuard(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-unknown")
	requireDirect(t, store, ref)

	first, err := store.AdmitOperation(ctx, operationRequest(ref, "key-a", "fp-a"))
	require.NoError(t, err)
	require.True(t, first.Created)
	require.Equal(t, OperationSubmitted, first.Operation.State)
	require.NotNil(t, first.Operation.OpenKey)

	moved, err := store.SetOperationState(ctx, first.Operation.ID, OperationUnknown)
	require.NoError(t, err)
	require.Equal(t, OperationUnknown, moved.State)
	require.NotNil(t, moved.OpenKey)

	second, err := store.AdmitOperation(ctx, operationRequest(ref, "key-b", "fp-b"))
	require.NoError(t, err)
	require.False(t, second.Created)
	require.True(t, second.Conflict)
	require.Equal(t, first.Operation.ID, second.Operation.ID)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))

	_, err = store.SetOperationState(ctx, first.Operation.ID, OperationSubmitted)
	require.NoError(t, err)
	completed, err := store.SetOperationState(ctx, first.Operation.ID, OperationCompleted)
	require.NoError(t, err)
	require.Equal(t, OperationCompleted, completed.State)
	require.Nil(t, completed.OpenKey)

	_, err = store.SetOperationState(ctx, first.Operation.ID, OperationAccepted)
	require.ErrorIs(t, err, ErrOperationTerminal)
	var stuck models.ConnectorOperation
	require.NoError(t, db.First(&stuck, "id = ?", first.Operation.ID).Error)
	require.Equal(t, OperationCompleted, stuck.State)
	require.Nil(t, stuck.OpenKey)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))

	third, err := store.AdmitOperation(ctx, operationRequest(ref, "key-c", "fp-c"))
	require.NoError(t, err)
	require.True(t, third.Created)
	require.False(t, third.Conflict)
	rejected, err := store.SetOperationState(ctx, third.Operation.ID, OperationRejected)
	require.NoError(t, err)
	require.Nil(t, rejected.OpenKey)
	_, err = store.SetOperationState(ctx, third.Operation.ID, OperationUnknown)
	require.ErrorIs(t, err, ErrOperationTerminal)

	// The original key still resolves to the completed receipt and does not open another row.
	replay, err := store.AdmitOperation(ctx, operationRequest(ref, "key-a", "fp-a"))
	require.NoError(t, err)
	require.False(t, replay.Created)
	require.False(t, replay.Conflict)
	require.Equal(t, first.Operation.ID, replay.Operation.ID)
	require.Equal(t, int64(2), countModel(t, db, &models.ConnectorOperation{}))
}

func TestActivateEpochHasOneConcurrentWinner(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()

	err := store.ActivateEpoch(ctx, "", "")
	require.ErrorIs(t, err, ErrEmptyEpoch)
	_, err = store.ActiveEpoch(ctx)
	require.ErrorIs(t, err, ErrNotFound)

	err = store.ActivateEpoch(ctx, "already", "next")
	require.ErrorIs(t, err, ErrEpochMismatch)
	_, err = store.ActiveEpoch(ctx)
	require.ErrorIs(t, err, ErrNotFound)

	const n = 8
	stored, err := oneEpochWinner(t, store, "", n, "epoch")
	require.NoError(t, err)
	current, err := store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, stored, current)

	stored, err = oneEpochWinner(t, store, stored, n, "moved")
	require.NoError(t, err)
	current, err = store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, stored, current)

	err = store.ActivateEpoch(ctx, "not-the-stored-value", "elsewhere")
	require.ErrorIs(t, err, ErrEpochMismatch)
	unchanged, err := store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, stored, unchanged)
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorConfiguration{}))
}

func TestStoreReopenPreservesCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	db := openSQLiteFile(t, path)
	migrateStore(t, db)
	store := NewStore(db)
	ctx := context.Background()
	from := mustExecutionRef(t, "primary", "from")
	to := mustExecutionRef(t, "primary", "to")
	at := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)
	wrote, err := store.RecordSnapshot(ctx, directObservation(from, 4, DisplayRunning, "src", at))
	require.NoError(t, err)
	require.True(t, wrote.Wrote)
	_, err = store.RecordSnapshot(ctx, directObservation(to, 1, DisplayRunning, "dst", at))
	require.NoError(t, err)
	relation, err := store.RecordRelation(ctx, from, to, RelationParent, []byte(`{"edge":"a"}`))
	require.NoError(t, err)
	decision, err := store.AdmitOperation(ctx, operationRequest(from, "receipt-key", "fingerprint"))
	require.NoError(t, err)
	require.NoError(t, store.ActivateEpoch(ctx, "", "epoch-kept"))

	closeSQLite(t, db)

	reopened := openSQLiteFile(t, path)
	store = NewStore(reopened)
	epoch, err := store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, "epoch-kept", epoch)

	again, err := store.RecordSnapshot(ctx, directObservation(from, 4, DisplayRunning, "src", at.Add(time.Hour)))
	require.NoError(t, err)
	require.False(t, again.Wrote)
	require.WithinDuration(t, at, again.ObservedAt, time.Second)
	require.Equal(t, int64(2), countModel(t, reopened, &models.ExternalExecution{}))
	require.Equal(t, int64(2), countModel(t, reopened, &models.ExternalExecutionSnapshot{}))

	sameRelation, err := store.RecordRelation(ctx, from, to, RelationParent, []byte(`{"edge":"a"}`))
	require.NoError(t, err)
	require.Equal(t, relation.ID, sameRelation.ID)
	require.Equal(t, int64(1), countModel(t, reopened, &models.ExternalExecutionRelation{}))

	replay, err := store.AdmitOperation(ctx, operationRequest(from, "receipt-key", "fingerprint"))
	require.NoError(t, err)
	require.False(t, replay.Created)
	require.Equal(t, decision.Operation.ID, replay.Operation.ID)
	require.Equal(t, int64(1), countModel(t, reopened, &models.ConnectorOperation{}))
}

func TestUnchangedSnapshotDoesNotWrite(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-same")
	firstAt := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(3 * time.Hour)
	obs := directObservation(ref, 7, DisplayRunning, "event-7", firstAt)
	obs.Metadata = []byte(`{"rows":2}`)

	wrote, err := store.RecordSnapshot(ctx, obs)
	require.NoError(t, err)
	require.True(t, wrote.Wrote)
	require.WithinDuration(t, firstAt, wrote.ObservedAt, time.Second)

	obs.ObservedAt = secondAt
	obs.Generation = 99
	again, err := store.RecordSnapshot(ctx, obs)
	require.NoError(t, err)
	require.False(t, again.Wrote)
	require.WithinDuration(t, firstAt, again.ObservedAt, time.Second)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))

	var snap models.ExternalExecutionSnapshot
	require.NoError(t, db.First(&snap).Error)
	require.WithinDuration(t, firstAt, snap.ObservedAt, time.Second)
	var ident models.ExternalExecution
	require.NoError(t, db.First(&ident).Error)
	require.NotNil(t, ident.LatestObservedAt)
	require.WithinDuration(t, firstAt, *ident.LatestObservedAt, time.Second)
	require.NotNil(t, ident.LatestGeneration)
	require.Equal(t, int64(99), *ident.LatestGeneration)
	require.False(t, ident.Referenced)

	late := obs
	late.Generation = 8
	late.SourceEventID = "event-8"
	_, err = store.RecordSnapshot(ctx, late)
	require.ErrorIs(t, err, ErrStaleGeneration)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))
}

func TestReadCatalogEpochTreatsMissingRowAsEmpty(t *testing.T) {
	db := openStoreDB(t)
	ctx := context.Background()
	fingerprint, err := ReadCatalogEpoch(ctx, db)
	require.NoError(t, err)
	require.Empty(t, fingerprint)

	require.NoError(t, NewStore(db).ActivateEpoch(ctx, "", "epoch-1"))
	fingerprint, err = ReadCatalogEpoch(ctx, db)
	require.NoError(t, err)
	require.Equal(t, "epoch-1", fingerprint)
}

func TestRetentionKeepsReferencedSnapshotsAndCapsTheRest(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	db := openStoreDB(t)
	store := NewStore(db, func() time.Time { return now })
	ctx := context.Background()

	oldUnref := mustExecutionRef(t, "primary", "old-unref")
	oldRef := mustExecutionRef(t, "primary", "old-ref")
	young1 := mustExecutionRef(t, "primary", "young-1")
	young2 := mustExecutionRef(t, "primary", "young-2")
	young3 := mustExecutionRef(t, "primary", "young-3")
	other := mustExecutionRef(t, "other", "foreign-old")
	require.NoError(t, store.ActivateEpoch(ctx, "", "retain-epoch"))

	require.NoError(t, recordAt(store, oldUnref, now.Add(-48*time.Hour)))
	require.NoError(t, recordAt(store, oldRef, now.Add(-48*time.Hour)))
	require.NoError(t, recordAt(store, young1, now.Add(-3*time.Hour)))
	require.NoError(t, recordAt(store, young2, now.Add(-2*time.Hour)))
	require.NoError(t, recordAt(store, young3, now.Add(-time.Hour)))
	require.NoError(t, recordAt(store, other, now.Add(-48*time.Hour)))
	require.NoError(t, store.MarkReferenced(ctx, oldRef.OpaqueID()))
	decision, err := store.AdmitOperation(ctx, operationRequest(young3, "keep-me", "fp"))
	require.NoError(t, err)
	relation, err := store.RecordRelation(ctx, oldRef, young3, RelationContinuation, []byte(`{"why":"verified"}`))
	require.NoError(t, err)

	err = store.EnforceRetention(ctx, "primary", 0, 2)
	require.ErrorIs(t, err, ErrRetentionLimit)
	err = store.EnforceRetention(ctx, "primary", time.Hour, 0)
	require.ErrorIs(t, err, ErrRetentionLimit)
	err = store.EnforceRetention(ctx, "primary", MaxUnreferencedSnapshotAge+time.Second, 2)
	require.ErrorIs(t, err, ErrRetentionLimit)
	err = store.EnforceRetention(ctx, "primary", time.Hour, MaxUnreferencedSnapshots+1)
	require.ErrorIs(t, err, ErrRetentionLimit)

	require.NoError(t, store.EnforceRetention(ctx, "primary", 24*time.Hour, 2))

	require.True(t, snapshotExists(t, db, oldUnref.OpaqueID()))
	require.True(t, snapshotExists(t, db, oldRef.OpaqueID()))
	require.True(t, snapshotExists(t, db, young1.OpaqueID()))
	require.True(t, snapshotExists(t, db, young2.OpaqueID()))
	require.True(t, snapshotExists(t, db, young3.OpaqueID()))
	require.True(t, snapshotExists(t, db, other.OpaqueID()))
	require.Equal(t, int64(6), countModel(t, db, &models.ExternalExecution{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionRelation{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ConnectorOperation{}))
	var storedRelation models.ExternalExecutionRelation
	require.NoError(t, db.First(&storedRelation, "id = ?", relation.ID).Error)
	var storedOp models.ConnectorOperation
	require.NoError(t, db.First(&storedOp, "id = ?", decision.Operation.ID).Error)
	epoch, err := store.ActiveEpoch(ctx)
	require.NoError(t, err)
	require.Equal(t, "retain-epoch", epoch)

	// A later non-latest snapshot ages out. The identity's latest row stays.
	aged := mustExecutionRef(t, "aged", "two-gens")
	require.NoError(t, recordGen(store, aged, 1, now.Add(-48*time.Hour)))
	require.NoError(t, recordGen(store, aged, 2, now.Add(-time.Hour)))
	require.NoError(t, store.EnforceRetention(ctx, "aged", 24*time.Hour, 2))
	require.Equal(t, int64(1), snapshotCount(t, db, aged.OpaqueID()))
	var kept models.ExternalExecutionSnapshot
	require.NoError(t, db.Where("opaque_id = ?", aged.OpaqueID()).First(&kept).Error)
	require.Equal(t, int64(2), kept.Generation)

	// The cap keeps exactly maxCount historical snapshots, plus the latest.
	hist := mustExecutionRef(t, "capped", "hist")
	bounds := WriteBounds{MaxUnreferencedSnapshots: 2}
	base := now.Add(-30 * time.Minute)
	for gen := int64(1); gen <= 4; gen++ {
		_, err := store.RecordSnapshot(ctx, directObservation(hist, gen, DisplayRunning, fmt.Sprintf("g-%d", gen), base.Add(time.Duration(gen)*time.Second)), bounds)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), snapshotCount(t, db, hist.OpaqueID()))
	var gens []int64
	require.NoError(t, db.Model(&models.ExternalExecutionSnapshot{}).Where("opaque_id = ?", hist.OpaqueID()).Order("generation").Pluck("generation", &gens).Error)
	require.Equal(t, []int64{2, 3, 4}, gens)
	require.True(t, catalogIdentityPresent(t, db, hist.OpaqueID()))
	require.True(t, snapshotExists(t, db, oldRef.OpaqueID()))

	_, err = store.RecordSnapshot(ctx, directObservation(mustExecutionRef(t, "bounded", "meta"), 1, DisplayRunning, "meta", now, bytes.Repeat([]byte("m"), 64)), WriteBounds{MaxMetadataBytes: 32})
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	_, err = store.RecordSnapshot(ctx, directObservation(mustExecutionRef(t, "bounded", "over"), 1, DisplayRunning, "over", now), WriteBounds{MaxUnreferencedSnapshots: MaxUnreferencedSnapshots + 1})
	require.ErrorIs(t, err, ErrRetentionLimit)
}

func TestDiscoveryDoesNotInsertIdentityAndStaleGenerationWritesNothing(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "exec-disc")
	at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	_, err := store.RecordSnapshot(ctx, observation(ref, 1, DisplayRunning, SourceDiscovery, "disc", at))
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, int64(0), countModel(t, db, &models.ExternalExecution{}))
	require.Equal(t, int64(0), countModel(t, db, &models.ExternalExecutionSnapshot{}))

	_, err = store.RecordSnapshot(ctx, directObservation(ref, 5, DisplayRunning, "direct-5", at))
	require.NoError(t, err)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecution{}))

	_, err = store.RecordSnapshot(ctx, observation(ref, 5, DisplayFailed, SourceDiscovery, "disc-5", at.Add(time.Minute)))
	require.ErrorIs(t, err, ErrDiscoverySuperseded)
	_, err = store.RecordSnapshot(ctx, observation(ref, 4, DisplayRunning, SourceDiscovery, "disc-4", at.Add(2*time.Minute)))
	require.ErrorIs(t, err, ErrDiscoverySuperseded)
	_, err = store.RecordSnapshot(ctx, directObservation(ref, 4, DisplayFailed, "direct-4", at.Add(3*time.Minute)))
	require.ErrorIs(t, err, ErrStaleGeneration)
	_, err = store.RecordSnapshot(ctx, directObservation(ref, 5, DisplayFailed, "direct-5b", at.Add(4*time.Minute)))
	require.ErrorIs(t, err, ErrStaleGeneration)
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecution{}))

	wrote, err := store.RecordSnapshot(ctx, observation(ref, 6, DisplayRunning, SourceDiscovery, "disc-6", at.Add(5*time.Minute)))
	require.NoError(t, err)
	require.True(t, wrote.Wrote)
	require.Equal(t, int64(2), countModel(t, db, &models.ExternalExecutionSnapshot{}))

	same, err := store.RecordSnapshot(ctx, directObservation(ref, 6, DisplayRunning, "disc-6", at.Add(6*time.Minute)))
	require.NoError(t, err)
	require.False(t, same.Wrote)
	require.Equal(t, int64(2), countModel(t, db, &models.ExternalExecutionSnapshot{}))
	require.Equal(t, int64(1), countModel(t, db, &models.ExternalExecution{}))

	oversized := bytes.Repeat([]byte("x"), MaxPageMetadataBytes+1)
	before := countModel(t, db, &models.ExternalExecution{})
	_, err = store.RecordSnapshot(ctx, directObservation(mustExecutionRef(t, "primary", "too-big"), 1, DisplayRunning, "big", at, oversized))
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	require.Equal(t, before, countModel(t, db, &models.ExternalExecution{}))
	require.Equal(t, int64(2), countModel(t, db, &models.ExternalExecutionSnapshot{}))

	_, err = store.RecordSnapshot(ctx, directObservation(ref, 1, "cancelled", "brit", at))
	require.ErrorIs(t, err, ErrInvalidObservation)

	_, err = store.AdmitOperation(ctx, operationRequest(mustExecutionRef(t, "primary", "never-read"), "missing", "fp"))
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, int64(0), countModel(t, db, &models.ConnectorOperation{}))

	_, err = store.RecordRelation(ctx, ref, mustExecutionRef(t, "primary", "missing-to"), RelationChild, []byte(`{}`))
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, int64(0), countModel(t, db, &models.ExternalExecutionRelation{}))
	_, err = store.RecordRelation(ctx, ref, ref, "sibling", []byte(`{}`))
	require.ErrorIs(t, err, ErrInvalidRelation)
	require.Equal(t, int64(0), countModel(t, db, &models.ExternalExecutionRelation{}))
}

func TestFreshAndUpgradedCatalogsGainConnectorTables(t *testing.T) {
	fresh := openSQLiteFile(t, filepath.Join(t.TempDir(), "fresh.db"))
	migrateStore(t, fresh)
	for _, table := range connectorTables {
		require.Truef(t, fresh.Migrator().HasTable(table), "fresh catalog missing %s", table)
		assertNoRunColumns(t, fresh, table)
	}

	upgraded := openSQLiteFile(t, filepath.Join(t.TempDir(), "upgraded.db"))
	require.NoError(t, upgraded.AutoMigrate(&models.Trigger{}, &models.Job{}))
	triggerID := uuid.New()
	jobID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, upgraded.Create(&models.Trigger{
		ID:        triggerID,
		Alias:     "keep-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, upgraded.Create(&models.Job{
		ID:        jobID,
		Alias:     "keep-job",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	migrateStore(t, upgraded)
	for _, table := range connectorTables {
		require.Truef(t, upgraded.Migrator().HasTable(table), "upgraded catalog missing %s", table)
	}
	var jobs int64
	require.NoError(t, upgraded.Model(&models.Job{}).Where("alias = ?", "keep-job").Count(&jobs).Error)
	require.Equal(t, int64(1), jobs)
	require.True(t, upgraded.Migrator().HasTable("jobs"))
}

func TestOperationRequiresIdentityAndBoundsActor(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	ctx := context.Background()
	ref := mustExecutionRef(t, "primary", "bounded")
	requireDirect(t, store, ref)
	req := operationRequest(ref, "key", "fp")
	req.Actor = bytes.Repeat([]byte("a"), MaxPageMetadataBytes+1)
	_, err := store.AdmitOperation(ctx, req)
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	require.Equal(t, int64(0), countModel(t, db, &models.ConnectorOperation{}))
	var ident models.ExternalExecution
	require.NoError(t, db.Where("opaque_id = ?", ref.OpaqueID()).First(&ident).Error)
	require.False(t, ident.Referenced)

	req.Actor = []byte(`{"principal_kind":"user","stable_id":"u1"}`)
	decision, err := store.AdmitOperation(ctx, req)
	require.NoError(t, err)
	require.True(t, decision.Created)
	require.NoError(t, db.Where("opaque_id = ?", ref.OpaqueID()).First(&ident).Error)
	require.True(t, ident.Referenced)
	require.NotContains(t, string(decision.Operation.Actor), "secret")
}

var connectorTables = []string{
	"external_executions",
	"external_execution_snapshots",
	"external_execution_relations",
	"connector_operations",
	"connector_configurations",
}

func oneEpochWinner(t *testing.T, store *Store, previous string, n int, prefix string) (string, error) {
	t.Helper()
	start := make(chan struct{})
	wins := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		next := fmt.Sprintf("%s-%d", prefix, i)
		go func(next string) {
			defer wg.Done()
			<-start
			err := store.ActivateEpoch(context.Background(), previous, next)
			if err != nil {
				errs <- err
				return
			}
			wins <- next
		}(next)
	}
	close(start)
	wg.Wait()
	close(wins)
	close(errs)
	var winner string
	count := 0
	for next := range wins {
		winner = next
		count++
	}
	if count != 1 {
		return "", fmt.Errorf("epoch winners = %d, want 1", count)
	}
	for err := range errs {
		if !errors.Is(err, ErrEpochMismatch) {
			return "", err
		}
	}
	return winner, nil
}

func migrateStore(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(
		&models.ExternalExecution{},
		&models.ExternalExecutionSnapshot{},
		&models.ExternalExecutionRelation{},
		&models.ConnectorOperation{},
		&models.ConnectorConfiguration{},
	))
}

func openStoreDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openSQLiteFile(t, filepath.Join(t.TempDir(), "store.db"))
	migrateStore(t, db)
	return db
}

func openSQLiteFile(t *testing.T, path string) *gorm.DB {
	t.Helper()
	dsn := "file:" + path + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func closeSQLite(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
}

func mustExecutionRef(t *testing.T, connectionID, id string) ExecutionReference {
	t.Helper()
	ref, err := NewExecutionReference(connectionID, map[string]string{"execution": id})
	require.NoError(t, err)
	return ref
}

func directObservation(ref ExecutionReference, generation int64, display, eventID string, at time.Time, metadata ...[]byte) Observation {
	obs := observation(ref, generation, display, SourceDirect, eventID, at)
	if len(metadata) > 0 {
		obs.Metadata = metadata[0]
	}
	return obs
}

func observation(ref ExecutionReference, generation int64, display, source, eventID string, at time.Time) Observation {
	return Observation{
		Execution:     ref,
		Generation:    generation,
		SourceEventID: eventID,
		NativeStatus:  display,
		DisplayStatus: display,
		Availability:  AvailabilityAvailable,
		Completeness:  CompletenessComplete,
		SourceKind:    source,
		ObservedAt:    at,
	}
}

func operationRequest(ref ExecutionReference, key, fingerprint string) OperationRequest {
	return OperationRequest{
		ConnectionID:       ref.ConnectionID,
		IdempotencyKey:     key,
		Execution:          ref,
		Action:             "approve_publication",
		RequestFingerprint: fingerprint,
		Actor:              []byte(`{"principal_kind":"user","stable_id":"user-1"}`),
		BindingVersion:     "1",
		ExternalUpdateID:   "update-" + key,
	}
}

func requireDirect(t *testing.T, store *Store, ref ExecutionReference) {
	t.Helper()
	_, err := store.RecordSnapshot(context.Background(), directObservation(ref, 1, DisplayRunning, "direct", time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)))
	require.NoError(t, err)
}

func recordAt(store *Store, ref ExecutionReference, at time.Time) error {
	return recordGen(store, ref, 1, at)
}

func recordGen(store *Store, ref ExecutionReference, generation int64, at time.Time) error {
	_, err := store.RecordSnapshot(context.Background(), directObservation(ref, generation, DisplayRunning, fmt.Sprintf("retained-%d", generation), at))
	return err
}

func snapshotCount(t *testing.T, db *gorm.DB, opaqueID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.ExternalExecutionSnapshot{}).Where("opaque_id = ?", opaqueID).Count(&n).Error)
	return n
}

func countModel(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(model).Count(&n).Error)
	return n
}

func snapshotExists(t *testing.T, db *gorm.DB, opaqueID string) bool {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.ExternalExecutionSnapshot{}).Where("opaque_id = ?", opaqueID).Count(&n).Error)
	return n > 0
}

func catalogIdentityPresent(t *testing.T, db *gorm.DB, opaqueID string) bool {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.ExternalExecution{}).Where("opaque_id = ?", opaqueID).Count(&n).Error)
	return n == 1
}

func assertNoRunColumns(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	var createSQL string
	require.NoError(t, db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&createSQL).Error)
	lower := strings.ToLower(createSQL)
	require.NotContains(t, lower, "workflow_id")
	require.NotContains(t, lower, "run_id")
	require.NotContains(t, lower, "namespace")
	require.NotContains(t, lower, "job_runs")
	require.NotContains(t, lower, "task_runs")
}
