package db

import (
	"fmt"
	"testing"
	"time"

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

	for range 3 {
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

// This is the persisted incident layout from before namespaces became required.
type legacyNamespaceIncident struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	Namespace       *string   `gorm:"type:text;index"`
	JobID           uuid.UUID `gorm:"type:uuid;not null"`
	TaskName        string
	Class           string                `gorm:"not null"`
	Status          models.IncidentStatus `gorm:"not null"`
	DedupeKey       string                `gorm:"not null"`
	ActiveDedupeKey *string               `gorm:"uniqueIndex:idx_incidents_active_dedupe"`
	OpenedAt        time.Time             `gorm:"not null"`
	CreatedAt       time.Time             `gorm:"not null"`
	UpdatedAt       time.Time             `gorm:"not null"`
}

func (legacyNamespaceIncident) TableName() string { return "incidents" }

func TestNamespaceMigrationRepairsLegacyNullsAndPreservesHistory(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&legacyNamespaceIncident{}))
	now := time.Now().UTC().Truncate(time.Second)
	empty, marketing := "", "marketing"
	namespaces := []*string{nil, &empty, &marketing}
	ids := make([]uuid.UUID, len(namespaces))
	for i, namespace := range namespaces {
		jobID := uuid.New()
		key := fmt.Sprintf("%s|extract|unknown", jobID)
		ids[i] = uuid.New()
		row := legacyNamespaceIncident{ID: ids[i], Namespace: namespace, JobID: jobID, TaskName: "extract", Class: "unknown", Status: models.IncidentStatusOpen, DedupeKey: key, ActiveDedupeKey: &key, OpenedAt: now, CreatedAt: now, UpdatedAt: now}
		if i == 1 {
			row.Status = models.IncidentStatusClosed
			row.ActiveDedupeKey = nil
		}
		require.NoError(t, conn.Create(&row).Error)
	}
	for range 2 {
		require.NoError(t, MigrateNamespaceDefaults(conn))
		require.NoError(t, migrateModels(conn, &models.Incident{}))
	}
	for i, id := range ids {
		var row models.Incident
		require.NoError(t, conn.First(&row, "id = ?", id).Error)
		want := "default"
		if i == 2 {
			want = "marketing"
		}
		require.Equal(t, want, row.Namespace)
		require.Equal(t, fmt.Sprintf("%s|%s|extract|unknown", want, row.JobID), row.DedupeKey)
		if i == 1 {
			require.Nil(t, row.ActiveDedupeKey)
		} else {
			require.Equal(t, row.DedupeKey, *row.ActiveDedupeKey)
		}
		require.True(t, now.Equal(row.OpenedAt), "history's original timestamp must not move")
	}
	require.True(t, conn.Migrator().HasIndex(&models.Incident{}, "idx_incidents_namespace"))
	require.Error(t, conn.Exec("UPDATE incidents SET namespace = NULL WHERE id = ?", ids[0]).Error, "NULL must be rejected after the upgrade")
	columns, err := conn.Migrator().ColumnTypes(&models.Incident{})
	require.NoError(t, err)
	for _, column := range columns {
		if column.Name() == "namespace" {
			nullable, ok := column.Nullable()
			require.True(t, ok)
			require.False(t, nullable)
			value, ok := column.DefaultValue()
			require.True(t, ok)
			require.Equal(t, "default", value)
		}
	}
}

func TestNamespaceMigrationNormalizesOnlyUnsetOwnershipAcrossTables(t *testing.T) {
	conn := openMigrationTestDB(t)
	for _, table := range []string{"jobs", "job_runs", "backfills"} {
		require.NoError(t, conn.Exec("CREATE TABLE "+table+" (id TEXT PRIMARY KEY, namespace TEXT)").Error)
		require.NoError(t, conn.Exec("INSERT INTO "+table+" (id, namespace) VALUES ('null', NULL), ('empty', ''), ('owned', 'finance')").Error)
	}
	require.NoError(t, MigrateNamespaceDefaults(conn))
	for _, table := range []string{"jobs", "job_runs", "backfills"} {
		var namespaces []string
		require.NoError(t, conn.Table(table).Order("id").Pluck("namespace", &namespaces).Error)
		require.Equal(t, []string{"default", "default", "finance"}, namespaces)
	}
	require.NoError(t, MigrateNamespaceDefaults(conn))
}

func TestNamespaceMigrationFreshDatabaseIsNoOp(t *testing.T) {
	require.NoError(t, MigrateNamespaceDefaults(openMigrationTestDB(t)))
}
