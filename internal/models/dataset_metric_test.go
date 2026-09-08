package models

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestDatasetMetricRegisteredAfterTaskRun pins two things AutoMigrate depends
// on: DatasetMetric is in the migration set at all, and it follows its FK
// parent TaskRun so the CASCADE constraint can be built.
func TestDatasetMetricRegisteredAfterTaskRun(t *testing.T) {
	metricIdx, taskRunIdx := -1, -1
	for i, model := range All {
		switch model.(type) {
		case *DatasetMetric:
			metricIdx = i
		case *TaskRun:
			taskRunIdx = i
		}
	}
	require.NotEqual(t, -1, metricIdx, "DatasetMetric must be registered in models.All")
	require.NotEqual(t, -1, taskRunIdx)
	assert.Greater(t, metricIdx, taskRunIdx, "DatasetMetric must follow its FK parent TaskRun")
}

func TestDatasetMetricMigratesAndRoundTrips(t *testing.T) {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(All...))
	require.True(t, db.Migrator().HasTable(&DatasetMetric{}))

	row := DatasetMetric{
		ID:        uuid.New(),
		TaskRunID: uuid.New(),
		Name:      "warehouse/orders",
		Metric:    "rowCount",
		Value:     10400312,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(&row).Error)

	var read DatasetMetric
	require.NoError(t, db.Where("id = ?", row.ID).First(&read).Error)
	assert.Equal(t, "", read.Namespace, "namespace is reserved and defaults to empty in v1")
	assert.Equal(t, "warehouse/orders", read.Name)
	assert.InDelta(t, 10400312, read.Value, 0.001)
}

// TestDatasetDeclarationCarriesAssertionSpec guards the EXTEND (not fork) of the
// shared registry model: the circuit breaker's spec columns live beside the
// freshness SLO columns on one row.
func TestDatasetDeclarationCarriesAssertionSpec(t *testing.T) {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(All...))

	for _, column := range []string{"assertions_json", "on_violation", "release"} {
		assert.True(t, db.Migrator().HasColumn(&DatasetDeclaration{}, column),
			"dataset_declarations must carry %s", column)
	}
}
