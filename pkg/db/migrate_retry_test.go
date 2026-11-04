package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// injectFirstIndexFailure makes the first CREATE INDEX on triggers fail. For
// the already-exists variant the index is really created first, exactly as a
// racing member would have, so recovery depends on the re-run seeing it.
func injectFirstIndexFailure(t *testing.T, conn *gorm.DB, failure string) *int {
	t.Helper()
	raw, err := conn.DB()
	require.NoError(t, err)
	fired := 0
	require.NoError(t, conn.Callback().Raw().Before("gorm:raw").Register("test:inject", func(tx *gorm.DB) {
		stmt := tx.Statement.SQL.String()
		if fired > 0 || !strings.Contains(stmt, "CREATE INDEX") || !strings.Contains(stmt, "idx_triggers_normalized_path") {
			return
		}
		fired++
		if strings.Contains(failure, "already exists") {
			_, execErr := raw.ExecContext(context.Background(), stmt)
			require.NoError(t, execErr)
		}
		_ = tx.AddError(errors.New(failure))
	}))
	return &fired
}

func TestMigrateRouterRecoversFromInjectedMigrationFailure(t *testing.T) {
	for _, failure := range []string{
		"index idx_triggers_normalized_path already exists",
		"database is locked",
	} {
		t.Run(failure, func(t *testing.T) {
			conn := openMigrationTestDB(t)
			fired := injectFirstIndexFailure(t, conn, failure)
			router, err := NewRouter(conn, nil, nil)
			require.NoError(t, err)

			require.NoError(t, migrateRouter(context.Background(), router, []time.Duration{1, 1, 1}, noWait))
			assert.Equal(t, 1, *fired, "the injected failure must have hit the first attempt")
			assert.True(t, conn.Migrator().HasIndex(&models.Trigger{}, "idx_triggers_normalized_path"))
			assert.True(t, conn.Migrator().HasTable(&models.TaskRun{}))
		})
	}
}

func TestMigrateRouterPersistentLockReportsAttempts(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.Callback().Raw().Before("gorm:raw").Register("test:lock", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "CREATE INDEX") {
			_ = tx.AddError(errors.New("database is locked"))
		}
	}))
	router, err := NewRouter(conn, nil, nil)
	require.NoError(t, err)

	err = migrateRouter(context.Background(), router, []time.Duration{1, 1}, noWait)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still failing after 3 attempts")
	assert.Contains(t, err.Error(), "database is locked")
}

func noWait(context.Context, time.Duration) error { return nil }

func TestMigrateModelsIsIdempotentWithPrecreatedIndex(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, migrateModels(conn, models.All...))
	// Second run over a fully migrated schema must succeed.
	require.NoError(t, migrateModels(conn, models.All...))

	// Index created out of band (as a racing member would) before this
	// member's AutoMigrate runs.
	conn2 := openMigrationTestDB(t)
	require.NoError(t, conn2.Exec("CREATE TABLE triggers (id text primary key, normalized_path text)").Error)
	require.NoError(t, conn2.Exec("CREATE INDEX idx_triggers_normalized_path ON triggers(normalized_path)").Error)
	require.NoError(t, migrateModels(conn2, &models.Trigger{}))
	require.True(t, conn2.Migrator().HasIndex(&models.Trigger{}, "idx_triggers_normalized_path"))
}

func TestMigrateWithRetryRetriesTransientThenSucceeds(t *testing.T) {
	for _, transient := range []error{
		errors.New("database is locked"),
		errors.New("index idx_triggers_normalized_path already exists"),
	} {
		calls := 0
		err := migrateWithRetry(context.Background(), func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		}, []time.Duration{1, 1, 1, 1}, noWait)
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	}
}

func TestMigrateWithRetryGivesUpAfterBound(t *testing.T) {
	calls := 0
	err := migrateWithRetry(context.Background(), func() error {
		calls++
		return errors.New("database is locked")
	}, []time.Duration{1, 1, 1}, noWait)
	require.Error(t, err)
	assert.Equal(t, 4, calls, "initial attempt plus one per backoff")
}

func TestMigrateWithRetryDoesNotRetryNonTransient(t *testing.T) {
	calls := 0
	err := migrateWithRetry(context.Background(), func() error {
		calls++
		return errors.New("syntax error near CREATE")
	}, []time.Duration{1, 1, 1}, noWait)
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}
