package db

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared&_busy_timeout=5000"
	conn, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := conn.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return conn
}

func indexDDL(t *testing.T, conn *gorm.DB) (string, bool) {
	t.Helper()
	def, found, err := taskRunIndexDefinition(conn)
	require.NoError(t, err)
	return def, found
}

// TestMigrateTaskRunUniquePartitionIndexUpgradesLegacyIndex reproduces the
// upgrade an EXISTING deployment goes through: a task_runs table already
// carrying the pre-fan-out non-unique (job_run_id, task_id) index under the name
// idx_taskrun_jobrun_task. AutoMigrate would see that name and leave it alone
// forever, so the unique (job_run_id, task_id, partition_index) constraint would
// never be created and the core fan-out invariant would go unenforced in
// production while passing on every fresh CI database.
func TestMigrateTaskRunUniquePartitionIndexUpgradesLegacyIndex(t *testing.T) {
	conn := openMigrationTestDB(t)

	// Build the table WITHOUT the index, then create the legacy index by hand,
	// exactly as the pre-fan-out struct tags would have.
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	require.NoError(t, conn.Exec("DROP INDEX IF EXISTS idx_taskrun_jobrun_task").Error)
	require.NoError(t, conn.Exec(
		"CREATE INDEX idx_taskrun_jobrun_task ON task_runs(job_run_id, task_id)").Error)

	legacy, found := indexDDL(t, conn)
	require.True(t, found)
	require.False(t, indexIsUniqueOverPartitionIndex(legacy), "fixture must start on the legacy shape")

	// AutoMigrate ALONE is not enough — this is the defect being fixed.
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	stillLegacy, _ := indexDDL(t, conn)
	require.False(t, indexIsUniqueOverPartitionIndex(stillLegacy),
		"AutoMigrate skips an index whose name already exists; without an explicit migration the legacy index survives")

	// The explicit migration + AutoMigrate does the upgrade.
	require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))

	upgraded, found := indexDDL(t, conn)
	require.True(t, found)
	assert.True(t, indexIsUniqueOverPartitionIndex(upgraded),
		"index must now be UNIQUE over (job_run_id, task_id, partition_index); got %q", upgraded)

	assertPartitionUniquenessEnforced(t, conn)
}

func TestMigrateTaskRunUniquePartitionIndexIsIdempotent(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))

	before, found := indexDDL(t, conn)
	require.True(t, found)
	require.True(t, indexIsUniqueOverPartitionIndex(before), "a fresh AutoMigrate already produces the new shape")

	for i := 0; i < 3; i++ {
		require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
		require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	}

	after, found := indexDDL(t, conn)
	require.True(t, found)
	assert.Equal(t, before, after, "a correct index must be left completely alone")
	assertPartitionUniquenessEnforced(t, conn)
}

func TestMigrateTaskRunUniquePartitionIndexNoTableIsNoOp(t *testing.T) {
	conn := openMigrationTestDB(t)
	// No task_runs table at all — a brand new database. Nothing to inspect and
	// nothing to drop; AutoMigrate will create the correct index.
	require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
	_, found := indexDDL(t, conn)
	assert.False(t, found)
}

// assertPartitionUniquenessEnforced proves the migrated index actually
// constrains the data, not just that its DDL string looks right: two instances
// of one (run, task) at DIFFERENT partition indexes are allowed, and a duplicate
// partition index is rejected.
func assertPartitionUniquenessEnforced(t *testing.T, conn *gorm.DB) {
	t.Helper()
	runID := uuid.New()
	taskID := uuid.New()
	newRow := func(index int) *models.TaskRun {
		return &models.TaskRun{
			ID:             uuid.New(),
			JobRunID:       runID,
			TaskID:         taskID,
			AtomID:         uuid.New(),
			Engine:         models.AtomEngineDocker,
			Image:          "alpine:3.23",
			Command:        `["echo","hi"]`,
			Status:         "pending",
			PartitionIndex: index,
			PartitionCount: 2,
		}
	}
	require.NoError(t, conn.Create(newRow(0)).Error, "two siblings at distinct partition indexes must be allowed")
	require.NoError(t, conn.Create(newRow(1)).Error)

	err := conn.Create(newRow(1)).Error
	require.Error(t, err, "a duplicate (job_run_id, task_id, partition_index) must be rejected by the unique index")
	assert.Contains(t, err.Error(), "UNIQUE")
}
