package backfill

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestIsContentionErrRecognisesPoisonedConnection(t *testing.T) {
	// `cannot start a transaction within a transaction` is the connection-state
	// poisoning that follows when a `checkpoint in progress` rollback fails to
	// clear an open transaction. Without retry on this error, one transient
	// dqlite blip permanently breaks one pooled connection for the lifetime of
	// the process and every caller that draws it cascades into failure.
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("some other error"), false},
		{"database is locked", errors.New("database is locked"), true},
		{"database is busy", errors.New("database is busy"), true},
		{"checkpoint in progress", errors.New("checkpoint in progress"), true},
		{"transaction within a transaction", errors.New("cannot start a transaction within a transaction"), true},
		{"wrapped error preserves classification", wrapError(errors.New("checkpoint in progress")), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isContentionErr(tc.err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWithBusyRetryRetriesContentionThenSucceeds(t *testing.T) {
	attempts := 0
	err := withBusyRetry(func() error {
		attempts++
		if attempts < 3 {
			// First two attempts fail with the poisoned-connection error;
			// the helper must retry rather than surface it to the caller.
			return errors.New("cannot start a transaction within a transaction")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, attempts, "expected helper to retry past contention errors")
}

func TestWithBusyRetryReturnsNonContentionImmediately(t *testing.T) {
	attempts := 0
	want := errors.New("not retryable")
	err := withBusyRetry(func() error {
		attempts++
		return want
	})
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, attempts, "non-contention errors must not be retried")
}

func wrapError(err error) error {
	return &wrappedErr{inner: err}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrap: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestRequestCancelMarksIntentWithoutTerminalTransition(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusRunning), backfill.Status)
	require.NotNil(t, backfill.CancelRequestedAt)

	cancelRequested, err := store.IsCancelRequested(backfillID)
	require.NoError(t, err)
	require.True(t, cancelRequested)
}

func TestMarkCancelledTransitionsRunningBackfill(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))
	require.NoError(t, store.MarkCancelled(backfillID))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusCancelled), backfill.Status)
	require.NotNil(t, backfill.CancelRequestedAt)
	require.NotNil(t, backfill.CompletedAt)

	cancelRequested, err := store.IsCancelRequested(backfillID)
	require.NoError(t, err)
	require.False(t, cancelRequested)
}

func TestCompleteDoesNotOverwriteCancelledBackfill(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))
	require.NoError(t, store.MarkCancelled(backfillID))
	require.NoError(t, store.Complete(backfillID, false))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusCancelled), backfill.Status)
	require.NotNil(t, backfill.CompletedAt)
}

func newBackfillTestStore(t *testing.T) (*Store, *gorm.DB, uuid.UUID) {
	t.Helper()

	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, db.AutoMigrate(models.All...))

	triggerID := uuid.New()
	jobID := uuid.New()
	backfillID := uuid.New()

	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Alias:     "trigger-" + triggerID.String(),
		Type:      models.TriggerTypeCron,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "job-" + jobID.String(),
		TriggerID: triggerID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, db.Create(&models.Backfill{
		ID:            backfillID,
		JobID:         jobID,
		Status:        string(models.BackfillStatusRunning),
		Start:         time.Now().UTC().Add(-time.Hour),
		End:           time.Now().UTC(),
		MaxConcurrent: 1,
		Reprocess:     string(models.ReprocessNone),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}).Error)

	return NewStore(db), db, backfillID
}
