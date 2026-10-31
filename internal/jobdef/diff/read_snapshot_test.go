package diff

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	dbfaults "github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var errCheckpoint = errors.New("checkpoint in progress")

// A read-only COMMIT that dqlite refuses fails after fn has succeeded: the
// whole transaction runs again, read-only each time, and the retry is counted.
func TestReadSnapshotRetriesAFailedCommit(t *testing.T) {
	store := openDiffTestDB(t)
	defer closeDiffTestDB(store)
	conn, faults := dbfaults.FailCommits(t, store, errCheckpoint, 1)
	retries := metrictestutil.CounterTotal(t, metrics.DBBusyRetriesTotal)

	calls := 0
	require.NoError(t, ReadSnapshot(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	}))

	require.Equal(t, 2, calls, "fn runs again after the failed COMMIT")
	require.Equal(t, 1, faults.Rollbacks, "the failed attempt is released before the next snapshot")
	require.Equal(t, []sql.TxOptions{{ReadOnly: true}, {ReadOnly: true}}, faults.Options)
	require.InDelta(t, retries+1, metrictestutil.CounterTotal(t, metrics.DBBusyRetriesTotal), 0)
}

// Statements inside a transaction bypass the pool's retry too, so a contended
// read inside fn is retried the same way.
func TestReadSnapshotRetriesAContendedRead(t *testing.T) {
	store := openDiffTestDB(t)
	defer closeDiffTestDB(store)

	calls := 0
	require.NoError(t, ReadSnapshot(t.Context(), store, func(*gorm.DB) error {
		calls++
		if calls == 1 {
			return errCheckpoint
		}
		return nil
	}))
	require.Equal(t, 2, calls)
}

// Persistent contention stays a failure, on the shared budget: never a hang,
// never a false success.
func TestReadSnapshotSurfacesContentionWhenRetriesRunOut(t *testing.T) {
	store := openDiffTestDB(t)
	defer closeDiffTestDB(store)
	attempts := len(db.BusyRetryBackoffs) + 1
	conn, faults := dbfaults.FailCommits(t, store, errCheckpoint, attempts+1)

	calls := 0
	err := ReadSnapshot(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	})

	require.ErrorIs(t, err, errCheckpoint)
	require.Equal(t, attempts, calls)
	require.Equal(t, attempts, faults.Rollbacks)
}

func TestReadSnapshotDoesNotRetryOtherErrors(t *testing.T) {
	failure := errors.New("disk I/O error")
	t.Run("from COMMIT", func(t *testing.T) {
		store := openDiffTestDB(t)
		defer closeDiffTestDB(store)
		conn, _ := dbfaults.FailCommits(t, store, failure, 1)
		calls := 0
		err := ReadSnapshot(t.Context(), conn, func(*gorm.DB) error {
			calls++
			return nil
		})
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	})
	t.Run("from fn", func(t *testing.T) {
		store := openDiffTestDB(t)
		defer closeDiffTestDB(store)
		calls := 0
		err := ReadSnapshot(t.Context(), store, func(*gorm.DB) error {
			calls++
			return failure
		})
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	})
}

func TestReadSnapshotHonoursCancellation(t *testing.T) {
	t.Run("before the first attempt", func(t *testing.T) {
		store := openDiffTestDB(t)
		defer closeDiffTestDB(store)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := ReadSnapshot(ctx, store, func(*gorm.DB) error {
			t.Fatal("fn must not run on a cancelled context")
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("during the backoff", func(t *testing.T) {
		store := openDiffTestDB(t)
		defer closeDiffTestDB(store)
		conn, _ := dbfaults.FailCommits(t, store, errCheckpoint, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		err := ReadSnapshot(ctx, conn, func(*gorm.DB) error {
			calls++
			cancel()
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, calls, "a cancelled retry does not start another attempt")
	})
}

// postgresDialector names a test handle "postgres", which is all ReadSnapshot
// consults; the statements still run on SQLite.
type postgresDialector struct{ gorm.Dialector }

func (postgresDialector) Name() string { return "postgres" }

// PostgreSQL's READ COMMITTED would take a new view per statement, so every
// attempt, not only the first, keeps REPEATABLE READ.
func TestReadSnapshotKeepsRepeatableReadOnEveryAttempt(t *testing.T) {
	store := openDiffTestDB(t)
	defer closeDiffTestDB(store)
	conn, faults := dbfaults.FailCommits(t, store, errCheckpoint, 1)
	conn.Dialector = postgresDialector{conn.Dialector}

	require.NoError(t, ReadSnapshot(t.Context(), conn, func(*gorm.DB) error { return nil }))

	want := sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	require.Equal(t, []sql.TxOptions{want, want}, faults.Options)
}

// The first attempt builds a complete baseline and its COMMIT fails; a job then
// moves out of the caller's namespace. The retry's baseline replaces the first
// attempt's outright: nothing is doubled and the moved job does not linger.
func TestLoadDatabaseSpecsConfinedRetryKeepsOnlyTheLastSnapshot(t *testing.T) {
	store := openDiffTestDB(t)
	defer closeDiffTestDB(store)
	definition := func(alias string) schema.Definition {
		return schema.Definition{
			Metadata: schema.Metadata{Alias: alias, Namespace: "marketing"},
			Trigger:  schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": "/hooks/" + alias}},
			Steps:    []schema.Step{{Name: "task", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}}},
		}
	}
	insertDiffDefinition(t, store, definition("stays"))
	insertDiffDefinition(t, store, definition("moves"))

	conn, faults := dbfaults.FailCommits(t, store, errCheckpoint, 1)
	faults.AfterRollback = func() {
		require.NoError(t, store.Model(&models.Job{}).Where("alias = ?", "moves").Update("namespace", "finance").Error)
	}
	specs, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), conn)

	require.NoError(t, err)
	require.Equal(t, 2, faults.Begins)
	require.Len(t, specs, 1)
	require.Contains(t, specs, "stays")
	require.Len(t, specs["stays"].Steps, 1)
}
